// Package provider implements discovery and metric scraping for local
// inference backends: Ollama, vLLM, SGLang, TRT-LLM/Triton, llama.cpp,
// LM Studio, MLX, KoboldCpp and further engines fingerprinted by their HTTP
// surface, plus a generic OpenAI-compatible fallback. Metric scraping reads
// Prometheus /metrics where engines publish it.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/bearer"
	"github.com/maci0/toktop/internal/core"
)

// PollTimeout bounds a single metrics scrape.
const PollTimeout = 1500 * time.Millisecond

// Metrics is the raw engine state a single poll yields. Rates are derived by
// the collector from successive samples.
type Metrics struct {
	Models   []core.ModelInfo
	OutTotal float64 // generation tokens, monotonic counter
	InTotal  float64 // prompt tokens, monotonic counter
	Running  int
	Waiting  int
	KVPct    float64 // 0..100
	HasKV    bool
	TTFTms   float64 // engine-reported mean TTFT if it publishes one

	DirectOutPS    float64 // engine-reported instantaneous tok/s, if it publishes one
	HasDirectOutPS bool
	Version        string // engine software version, best effort
}

// Provider is one inference backend the collector can poll.
type Provider struct {
	Label string
	Addr  string
	Kind  string
	Poll  func(ctx context.Context) (*Metrics, error)
}

var httpClient = &http.Client{Timeout: PollTimeout, CheckRedirect: bearer.CheckRedirect}

func httpStatus(url string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4*core.SnippetCap))
	msg := fmt.Sprintf("%s: http %s", url, resp.Status)
	if s := core.Snippet(b); s != "" {
		msg += ": " + s
	}
	return errors.New(msg)
}

func getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", url, err)
	}
	bearer.Apply(req)
	resp, err := httpClient.Do(req)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return fmt.Errorf("%s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpStatus(url, resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return fmt.Errorf("%s: %w", url, err)
	}
	return nil
}

// getText fetches a URL with the given client; the caller's context bounds
// the request alongside any client timeout.
func getText(ctx context.Context, c *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("%s: %w", url, err)
	}
	bearer.Apply(req)
	resp, err := c.Do(req)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return "", fmt.Errorf("%s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", httpStatus(url, resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", fmt.Errorf("%s: %w", url, err)
	}
	return string(b), nil
}

// versionCache memoizes engine version discovery across polls. Deliberately
// not a sync.Once: an engine polled while still starting answers nothing,
// and caching that miss would blank the version readout for the whole
// session. Unresolved caches retry after versionRetry so an engine that
// never publishes a version is not probed on every scrape.
type versionCache struct {
	mu       sync.Mutex
	resolved bool
	val      string
	at       time.Time // last probe; misses retry after versionRetry
}

// versionRetry spaces out probes of an unresolved version. A hit is kept
// for the process lifetime; retrying a miss every poll would add three
// HTTP round trips to every scrape of engines with no version endpoint.
const versionRetry = 30 * time.Second

// fetch probes common engine version endpoints and caches the first success.
// Engines differ wildly here: /api/version (Ollama-style), /version (vLLM,
// llama.cpp), /get_server_info (SGLang embeds one).
func (c *versionCache) fetch(ctx context.Context, base string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resolved {
		return c.val
	}
	if !c.at.IsZero() && time.Since(c.at) < versionRetry {
		return c.val
	}
	for _, path := range []string{"/api/version", "/version", "/get_server_info"} {
		text, err := getText(ctx, httpClient, base+path)
		if err != nil {
			continue
		}
		if val := extractVersionField(text); val != "" {
			c.val, c.resolved, c.at = val, true, time.Now()
			return c.val
		}
	}
	c.at = time.Now()
	return c.val
}

// extractVersionField pulls a "version" member out of JSON-ish bodies.
func extractVersionField(body string) string {
	trimmed := strings.TrimSpace(body)
	var doc map[string]any
	if json.Unmarshal([]byte(trimmed), &doc) == nil {
		for _, key := range []string{"version", "Version"} {
			if s, ok := doc[key].(string); ok && s != "" {
				return s
			}
		}
		// nested (SGLang get_server_info)
		for _, sub := range []string{"backend_version_info", "server_info", "versions"} {
			if inner, ok := doc[sub].(map[string]any); ok {
				for _, key := range []string{"version", "sglang_version", "vllm_version"} {
					if s, ok := inner[key].(string); ok && s != "" {
						return s
					}
				}
			}
		}
		return ""
	}
	// llama.cpp /version may answer with a bare quoted string or plain text
	if trimmed == "" || len(trimmed) > 128 {
		return ""
	}
	if strings.HasPrefix(trimmed, "\"") && strings.HasSuffix(trimmed, "\"") {
		return strings.Trim(trimmed, "\"")
	}
	if !strings.ContainsAny(trimmed, "{}\n") {
		return trimmed
	}
	return ""
}

// parseProm scrapes a minimal Prometheus text exposition into family values.
// Labeled series of the same family are summed; histograms contribute their
// _sum and _count families verbatim.
func parseProm(text string) map[string]float64 {
	fam := map[string]float64{}
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, val, ok := splitMetric(line)
		if !ok || strings.HasSuffix(name, "_bucket") {
			continue
		}
		// Each series parsed finite, but the running family sum can still
		// overflow past MaxFloat64; keep the last good value rather than
		// let a poisoned family reach the stored totals and every derived
		// rate from here on.
		if sum := fam[name] + val; finite(sum) {
			fam[name] = sum
		}
	}
	return fam
}

// finite reports whether v is a usable measurement: NaN and ±Inf would
// poison downstream math and render as garbage.
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func splitMetric(line string) (string, float64, bool) {
	sp := -1
	var labels, quoted, escaped bool
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if quoted {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				quoted = false
			}
			continue
		}
		switch ch {
		case '{':
			labels = true
		case '}':
			labels = false
		case '"':
			quoted = labels
		case ' ', '\t':
			if !labels {
				sp = i
			}
		}
		if sp >= 0 {
			break
		}
	}
	if sp < 0 {
		return "", 0, false
	}
	name := line[:sp]
	fields := strings.Fields(line[sp:])
	if len(fields) == 0 {
		return "", 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || name == "" {
		return "", 0, false
	}
	// The exposition format allows NaN/+Inf values (0/0 gauges on engines
	// that have not served yet); they would poison the summed family and,
	// through the stored totals, every derived rate from here on.
	if !finite(v) {
		return "", 0, false
	}
	if i := strings.IndexByte(name, '{'); i >= 0 {
		name = name[:i]
	}
	return name, v, true
}

// classify maps scraped families onto our Metrics using fuzzy, version-tolerant
// name matching across engines (vLLM, SGLang, Triton/TRT-LLM, llama.cpp…).
func classify(fam map[string]float64, m *Metrics) {
	lower := make(map[string]float64, len(fam))
	for k, v := range fam {
		lower[strings.ToLower(k)] = v
	}
	// Iterate in sorted order: several names can contest one scalar field,
	// and random map order would flip the winner (and the rendered queue
	// depth or KV percentage) between polls on identical input.
	for _, n := range slices.Sorted(maps.Keys(lower)) {
		v := lower[n]
		hasTok := strings.Contains(n, "token")
		switch {
		case hasTok && strings.Contains(n, "total"):
			outish := core.ContainsAny(n, "generat", "predict", "complet", "eval")
			inish := core.ContainsAny(n, "prompt", "input")
			switch {
			case inish:
				m.InTotal = max(v, 0) // counters are unsigned; a negative gauge is junk
			case outish:
				m.OutTotal = max(v, 0)
			}
		case strings.Contains(n, "token_usage"):
			if v >= 0 && v <= 1 { // SGLang: fraction of token pool in use
				m.KVPct = v * 100
				m.HasKV = true
			}
		case strings.Contains(n, "req") && core.ContainsAny(n, "run", "process", "active", "inflight") &&
			!core.ContainsAny(n, "time", "duration", "second"):
			m.Running = core.SatInt(v)
		case core.ContainsAny(n, "req") && core.ContainsAny(n, "wait", "queue", "pend") &&
			!core.ContainsAny(n, "time", "duration", "second"):
			m.Waiting = core.SatInt(v) // covers vLLM requests_waiting and SGLang num_queue_reqs
		case strings.Contains(n, "cache") && core.ContainsAny(n, "usage", "util", "ratio", "perc"):
			pct := v
			if pct <= 1.0 {
				pct *= 100
			}
			if pct >= 0 && pct <= 100 {
				m.KVPct = pct
				m.HasKV = true
			}
		case strings.Contains(n, "time_to_first_token") && strings.HasSuffix(n, "_sum"):
			cnt := lower[strings.TrimSuffix(n, "_sum")+"_count"]
			if cnt > 0 {
				if ms := v / cnt * 1000; ms > 0 && finite(ms) { // junk means no reading; a denormal count overflows the mean
					m.TTFTms = ms
				}
			}
		case strings.Contains(n, "throughput") && core.ContainsAny(n, "gen", "generation", "decode"):
			if v >= 0 {
				m.DirectOutPS = v
				m.HasDirectOutPS = true
			}
		}
	}
}
