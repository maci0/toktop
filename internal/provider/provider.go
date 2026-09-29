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
	"unicode/utf8"

	"github.com/maci0/toktop/internal/bearer"
	"github.com/maci0/toktop/internal/core"
)

// PollTimeout bounds a single HTTP request, including a metrics scrape and
// the concurrent /v1/models probe. It is also the budget the collector gives a
// whole poll, so a chain of requests inside one poll (the version chain
// probes /api/version, /version and /get_server_info in turn) shares this
// window rather than getting one per hop: the chain is cut short rather than
// allowed to outlive the poll that started it.
const PollTimeout = 1500 * time.Millisecond

// jsonBodyMax caps what a single engine JSON response may contribute to this
// process. A body past it is truncated and the decode then fails, so a runaway
// endpoint costs the frame its response rather than the process its memory.
// The text cap is textCap, which refuses rather than truncates.
const jsonBodyMax = 4 << 20

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
	b, rerr := io.ReadAll(io.LimitReader(resp.Body, 4*core.SnippetCap))
	msg := fmt.Sprintf("%s: http %s", url, core.HTTPStatus(resp.Status))
	if s := core.Snippet(b); s != "" {
		msg += ": " + s
	}
	if rerr != nil {
		// A body that stopped partway is a fragment, not what the engine
		// said. The cause is wrapped, not spelled into the text, so a
		// caller can still tell a truncated transfer from a bad payload.
		return fmt.Errorf("%s: %w", msg, rerr)
	}
	return errors.New(msg)
}

// get issues one authorized GET and returns a 200 response whose body the
// caller must close. A redirect error carries the last response along, so
// that body is closed here rather than leaked to the caller's error path.
func get(ctx context.Context, c *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	bearer.Apply(req)
	resp, err := c.Do(req)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		err := httpStatus(url, resp)
		resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

// drainCap bounds the tail drainAndClose throws away so a connection can be
// reused. The bytes go to io.Discard, so this bounds time rather than memory:
// an endpoint streaming an endless body would otherwise hold the call on a read
// that answers nothing. Past the cap the connection is simply not reused, which
// is what closing an undrained body did anyway.
const drainCap = 64 << 10

// drainAndClose reads out the tail of a body the caller stopped reading and
// then closes it. net/http only returns a connection to the idle pool when its
// body is closed at EOF (the reason getJSON drains), so a decode that stops at
// the end of the JSON value has to finish the transfer here.
//
// Discovery does exactly that on a dozen requests per candidate port: every
// decodeScanJSON caller abandons the body at the end of the JSON value, and the
// body of a /metrics exposition is read whole, so without the drain every probe
// pays a fresh dial and leaves a socket in TIME_WAIT.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, drainCap))
	body.Close()
}

func getJSON(ctx context.Context, url string, out any) error {
	resp, err := get(ctx, httpClient, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(io.LimitReader(resp.Body, jsonBodyMax))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%s: %w", url, err)
	}
	// Decode stops at the end of the JSON value, not at EOF, and net/http
	// only returns a connection to the idle pool when the body is closed at
	// EOF. Without the drain every poll pays a fresh dial and leaves a
	// socket in TIME_WAIT.
	_, _ = io.Copy(io.Discard, io.MultiReader(dec.Buffered(), resp.Body))
	return nil
}

// textCap bounds a text scrape. A body past it is refused rather than
// truncated: a partial exposition parses into lower counters, and the
// collector reads a counter that fell as a restart, so the tokens in the
// truncated tail are lost for good. A JSON scrape needs no such guard, since
// a body cut mid-document fails to decode.
const textCap = 8 << 20

// getText fetches a URL with the given client; the caller's context bounds
// the request alongside any client timeout.
func getText(ctx context.Context, c *http.Client, url string) (string, error) {
	resp, err := get(ctx, c, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, textCap+1))
	if err != nil {
		return "", fmt.Errorf("%s: %w", url, err)
	}
	if len(b) > textCap {
		return "", fmt.Errorf("%s: response over %d bytes", url, textCap)
	}
	return string(b), nil
}

// versionCache memoizes engine version discovery across polls. Deliberately
// not a sync.Once: an engine polled while still starting answers nothing,
// and caching that miss would blank the version readout for the whole
// session. Unresolved caches retry after versionRetry so an engine that
// never publishes a version is not probed on every scrape, and resolved
// ones are re-asked after versionRefresh so an engine replaced under a
// running dashboard does not report its old version until the next start.
//
// The mutex guards the three fields and nothing else: the probe runs with it
// released, so a stalled endpoint costs the fetch that asked it and not every
// other reader of the cache.
type versionCache struct {
	mu       sync.Mutex
	resolved bool
	val      string
	at       time.Time // last probe; a miss retries after versionRetry, a hit after versionRefresh
}

// versionNow is the clock every version cache ages its probe windows against.
// The retry and refresh spacing decides which polls reach the network at all,
// so on the wall clock a run's version traffic is a function of how long the
// process happened to be up: the same polls replayed from the same seed hit
// different endpoints, and a cache that expires mid-replay blanks the version
// readout for that frame. SetNow replaces it, so a replayed or simulated run
// expires the windows on its own timeline. Call it before Discover or Attach,
// the way the other SetNow seams are called before their Run. Guarded, because
// a poll running on another goroutine reads it on every fetch.
var (
	versionNowMu sync.RWMutex
	versionNow   = time.Now
)

// SetNow overrides the clock the version caches time their retry and refresh
// windows against, restoring the wall clock for nil. It does not move request
// deadlines: those are real HTTP timeouts and belong to the wall clock.
func SetNow(fn func() time.Time) {
	if fn == nil {
		fn = time.Now
	}
	versionNowMu.Lock()
	versionNow = fn
	versionNowMu.Unlock()
}

// versionInstant reads the injected clock, calling it outside the lock: it is
// caller-supplied and may re-enter the package.
func versionInstant() time.Time {
	versionNowMu.RLock()
	fn := versionNow
	versionNowMu.RUnlock()
	return fn()
}

// versionRetry spaces out probes of an unresolved version. Retrying a miss
// every poll would add three HTTP round trips to every scrape of engines
// with no version endpoint.
const versionRetry = 30 * time.Second

// versionRefresh is how long a resolved version is reused before the engine
// is asked again. Engines are replaced under a running dashboard (a
// container re-pulled, an ollama upgrade, a remote host behind the forward
// restarted with a new image), and a hit kept for the process lifetime would
// report the old version until the operator quit and relaunched. The window
// is long because a version changes rarely and the probe is cheap next to
// the scrape that triggers it: three requests per engine per window, not
// three per poll.
const versionRefresh = 10 * time.Minute

// versionWindow is how long the last probe stands: a miss is retried on the
// short retry spacing, a hit on the long refresh spacing.
func versionWindow(resolved bool) time.Duration {
	if resolved {
		return versionRefresh
	}
	return versionRetry
}

// fetch probes common engine version endpoints and caches the first success.
// Engines differ wildly here: /api/version (Ollama-style), /version (vLLM,
// llama.cpp), /get_server_info (SGLang embeds one).
//
// The probe runs with c.mu released. It is up to three HTTP round trips under
// the caller's context, and holding the cache mutex across them pins every
// other reader of this cache for the whole chain: a version cache is shared by
// a provider's poll and by an ad-hoc Identify, and one engine that stalls on
// /api/version would then block the /version answer behind it as well.
// versionInstant is called with no lock held for the same reason it is
// documented that way: it is caller-supplied, and a simulated clock takes the
// demo source's own mutex.
func (c *versionCache) fetch(ctx context.Context, base string) string {
	now := versionInstant()
	if v, fresh := c.fresh(now); fresh {
		return v
	}
	val, resolved := c.probe(ctx, base)
	c.mu.Lock()
	defer c.mu.Unlock()
	// A probe that finished behind a newer one stores nothing: the later
	// stamp is the one the window has to be measured from, and overwriting it
	// with an older one would age the entry by a whole extra refresh. Both
	// probes read the same endpoints, so the value is the same either way.
	if c.at.After(now) {
		return c.val
	}
	c.at = now
	if resolved {
		c.val, c.resolved = val, true
	}
	return c.val
}

// fresh reports the cached version when the window it was recorded in still
// stands, and whether the caller should probe instead. A cache that has never
// been written is not fresh, and neither is one whose last probe has aged out
// of versionWindow.
func (c *versionCache) fresh(now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() || core.Age(now, c.at) >= versionWindow(c.resolved) {
		return "", false
	}
	return c.val, true
}

// probe asks the engine's known version endpoints in turn and reports the
// first usable answer. Every request is bounded by ctx, so a chain against a
// wedged engine costs the caller's budget and not more.
func (c *versionCache) probe(ctx context.Context, base string) (string, bool) {
	for _, path := range []string{"/api/version", "/version", "/get_server_info"} {
		text, err := getText(ctx, httpClient, base+path)
		if err != nil {
			continue
		}
		if val := extractVersionField(text); val != "" {
			return val, true
		}
	}
	return "", false
}

// versionCap is the bound capVersion applies, so every branch of
// extractVersionField shares it: the value is cached for versionRefresh and
// re-rendered every frame, so a hostile engine answering an 8 MB "version"
// member must not be able to hold it.
const versionCap = 128

// capVersion bounds a version string to versionCap grapheme clusters and
// strips what a terminal would interpret in it. The engine, not the operator,
// chooses the text. The cap counts clusters, so it never splits a multi-byte
// rune, a combining mark, or an emoji sequence. Sanitizing comes first:
// ClampField only normalizes, so the quoted and plain-text branches of
// extractVersionField would otherwise hand a bare invalid byte (an engine
// answering 0x8c and nothing else) straight through as the cached version.
// A version is one field on one line, so whitespace runs collapse to a single
// space: the quoted branch strips quotes but not what is between them, and an
// engine answering "\n" would otherwise buy itself a second row in the
// version readout.
func capVersion(s string) string {
	return core.ClampField(core.SingleLine(s), versionCap)
}

// extractVersionField pulls a "version" member out of JSON-ish bodies.
func extractVersionField(body string) string {
	trimmed := strings.TrimSpace(body)
	var doc map[string]any
	if json.Unmarshal([]byte(trimmed), &doc) == nil {
		for _, key := range []string{"version", "Version"} {
			if s, ok := doc[key].(string); ok && s != "" {
				return capVersion(s)
			}
		}
		// nested (SGLang get_server_info)
		for _, sub := range []string{"backend_version_info", "server_info", "versions"} {
			if inner, ok := doc[sub].(map[string]any); ok {
				for _, key := range []string{"version", "sglang_version", "vllm_version"} {
					if s, ok := inner[key].(string); ok && s != "" {
						return capVersion(s)
					}
				}
			}
		}
		return ""
	}
	// llama.cpp /version may answer with a bare quoted string or plain text.
	// Cheap reject for a body that is not a version at all: past this many
	// characters the plain-text answer is not a version string either.
	//
	// Counted in characters, not bytes, because versionCap is a character cap
	// everywhere else on this value and a byte count rejects strictly more:
	// 128 CJK characters are 384 bytes, so a plain-text version written in any
	// non-Latin script was dropped here while the same version in ASCII was
	// kept, and capVersion's cluster cap never got the chance to bound it.
	if trimmed == "" || utf8.RuneCountInString(trimmed) > versionCap {
		return ""
	}
	if strings.HasPrefix(trimmed, "\"") && strings.HasSuffix(trimmed, "\"") {
		return capVersion(strings.Trim(trimmed, "\""))
	}
	if !strings.ContainsAny(trimmed, "{}\n") {
		return capVersion(trimmed)
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
	// A busy vLLM publishes thousands of families per poll and only a handful
	// can match a branch below: every one of them names tokens, requests, a
	// cache or throughput. Filtering first (case-insensitively, so the common
	// path allocates nothing) keeps the lowercasing and the sort proportional
	// to the matches rather than to the exposition.
	lower := make(map[string]float64, 16)
	for k, v := range fam {
		n := core.FoldASCII(k)
		if !classifiable(n) {
			continue
		}
		lower[n] = v
	}
	// Iterate in sorted order: several names can contest one scalar field,
	// and random map order would flip the winner (and the rendered queue
	// depth or KV percentage) between polls on identical input. Sorting the
	// filtered names is the same order sorting all of them would give.
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
			// A 0..1 fraction is rescaled to a percentage; a family that
			// publishes 0..100 passes through. The value is the only
			// evidence here: vLLM's own gpu_cache_usage_perc carries a
			// fraction, so a name test would misread it.
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

// classifiable reports whether a family name can reach any branch of classify.
// The four substrings are exactly what those branches test for, and
// "time_to_first_token" is covered by "token".
// This is the pre-filter that decides whether a family reaches classify's own
// tests. It takes the name already folded by the caller, so the fold of a
// family name happens once per poll rather than once here and once there.
// core.FoldASCII leaves an all-lowercase Prometheus name as it found it, so
// the filter allocates nothing in the common case.
func classifiable(folded string) bool {
	return strings.Contains(folded, "token") || strings.Contains(folded, "req") ||
		strings.Contains(folded, "cache") || strings.Contains(folded, "throughput")
}
