// Package probe fires small streaming generations at backends to measure
// time-to-first-token and decode throughput the way clients experience it.
package probe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maci0/toktop/internal/bearer"
	"github.com/maci0/toktop/internal/core"
)

var client = &http.Client{Timeout: 30 * time.Second}

// probeTokens sizes a probe: a few dozen tokens are plenty to time
// first-token latency and decode rate without turning a benchmark into an
// unbounded generation. The request asks the engine to stop there; the
// client also stops reading after this many content frames, because some
// gateways ignore max_tokens/num_predict and would otherwise generate (and
// bill) until the HTTP timeout.
const probeTokens = 32

// probeTokenTrust is the highest engine-reported eval_count /
// completion_tokens we believe. Tokenizers can overshoot the request a
// little; a billion-token usage field is junk that would poison tok/s.
const probeTokenTrust = probeTokens * 4

// probeContentBytes is a second hang-up: frame counting treats each
// SSE/NDJSON content payload as one token, so a gateway that dumps a huge
// delta in one frame would otherwise keep the connection open (and keep
// billing) until the HTTP timeout. 32 bytes per requested token covers
// any encoding of a 32-token reply.
const probeContentBytes = probeTokens * 32

// probeLineMax is the largest SSE/NDJSON frame we will buffer. A 32-token
// completion plus wrapper JSON is hundreds of bytes; a megabyte line is
// the engine ignoring the cap in one shot, and bufio.Scanner only applies
// the hang-up after the line is fully read.
const probeLineMax = 16 << 10

// ModelNameMax caps the engine-supplied model id interpolated into the
// generation request. /v1/models can return megabyte strings; HuggingFace
// ids fit in well under this.
const ModelNameMax = 256

const promptText = "Count from one to twenty as words."

// defaultRetryAfter is the floor for 429/503 backoff. A missing or tiny
// Retry-After must not disable the cap: the next --probe tick would otherwise
// POST again immediately against an overloaded or billed gateway.
const defaultRetryAfter = 15 * time.Second

// maxRetryAfter caps engine-supplied Retry-After. A hostile header must not
// silence probes for hours.
const maxRetryAfter = 5 * time.Minute

type Request struct {
	Kind  string // core.KindOllama | openai-compatible kinds
	Base  string
	Model string
}

// Run performs one probe and returns its sample (OK=false with Err set on failure).
func Run(ctx context.Context, r Request) core.ProbeSample {
	r.Model = capModel(r.Model)
	s := core.ProbeSample{At: time.Now(), Addr: r.Base, Model: r.Model}
	if r.Model == "" {
		// An empty id makes some engines load a default model (VRAM and,
		// on a billed gateway, tokens). Refuse rather than POST.
		s.Err = "no model"
		return s
	}
	start := time.Now()
	var (
		ttft    time.Duration
		tokens  int
		evalDur time.Duration
		err     error
	)
	if r.Kind == core.KindOllama {
		tokens, evalDur, ttft, err = probeOllama(ctx, r, &s)
	} else {
		tokens, ttft, err = probeOpenAI(ctx, r, &s)
	}
	total := time.Since(start)
	if err != nil {
		s.Err = err.Error()
		var se *httpStatusError
		if errors.As(err, &se) {
			s.RetryAfter = se.after
		}
		return s
	}
	// Windows clocks tick coarsely; instant local servers can land the whole
	// exchange inside one tick. Fall back to full-duration accounting.
	if ttft <= 0 {
		ttft = total
	}
	s.OK = true
	s.Tokens = tokens
	s.TTFTms = float64(ttft.Microseconds()) / 1000.0
	switch {
	case evalDur > 0:
		s.TokPS = float64(tokens) / evalDur.Seconds()
	case total > ttft && tokens > 0:
		s.TokPS = float64(tokens) / (total - ttft).Seconds()
	case tokens > 0 && total > 0:
		s.TokPS = float64(tokens) / total.Seconds()
	}
	return s
}

func probeOllama(ctx context.Context, r Request, s *core.ProbeSample) (tokens int, evalDur, ttft time.Duration, err error) {
	body, _ := json.Marshal(map[string]any{
		"model":  r.Model,
		"prompt": promptText,
		"stream": true,
		// Thinking models (deepseek-r1, qwen3, …) generate a reasoning
		// trace that num_predict does not cap. Turning think off keeps the
		// probe inside the 32-token budget instead of filling the 30s
		// timeout (and a billed gateway's invoice).
		"think": false,
		"options": map[string]any{
			"num_predict": probeTokens,
			"temperature": 0.2,
		},
	})
	resp, err := postJSON(ctx, r.Base+"/api/generate", body)
	if err != nil {
		return 0, 0, 0, err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 4<<10), probeLineMax)
	var reported, contentBytes int
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var chunk struct {
			Response     string `json:"response"`
			Done         bool   `json:"done"`
			EvalCount    int    `json:"eval_count"`
			EvalDuration int64  `json:"eval_duration"`
			Error        string `json:"error"`
		}
		if json.Unmarshal(line, &chunk) != nil {
			continue
		}
		// Ollama streams failures as {"error":…} lines with HTTP 200; decoding
		// them as content would report a green probe with invented throughput.
		if chunk.Error != "" {
			return 0, 0, ttft, fmt.Errorf("engine error: %s", core.Snippet([]byte(chunk.Error)))
		}
		// Non-stream Ollama answers in one object with both response and
		// done=true; counting only !Done frames treated that as empty.
		if chunk.Response != "" {
			tokens++
			contentBytes += len(chunk.Response)
			if ttft == 0 {
				ttft = time.Since(s.At)
			}
			if overBudget(tokens, contentBytes) { // engine ignored num_predict: hang up
				break
			}
		}
		if chunk.Done {
			if chunk.EvalCount > 0 {
				reported = chunk.EvalCount
				evalDur = time.Duration(chunk.EvalDuration) * time.Nanosecond
			}
			break
		}
	}
	if err := streamReadErr(ctx, sc.Err(), tokens); err != nil {
		return 0, 0, ttft, err
	}
	n, trust := resolveTokens(tokens, reported)
	if !trust {
		evalDur = 0 // eval_duration is paired with the rejected count
	}
	if n == 0 { // stream closed without a single token: engine is broken
		return 0, 0, ttft, fmt.Errorf("empty stream")
	}
	return n, evalDur, ttft, nil
}

func probeOpenAI(ctx context.Context, r Request, s *core.ProbeSample) (tokens int, ttft time.Duration, err error) {
	url := r.Base + "/v1/chat/completions"
	resp, err := postJSON(ctx, url, openaiBody(r.Model, true))
	if err != nil {
		var se *httpStatusError
		// One retry with the legacy field set: stream_options and
		// max_completion_tokens 400 on older llama.cpp / strict proxies.
		// 429/503 are not retried — that would be a spend multiplier.
		if errors.As(err, &se) && (se.status == http.StatusBadRequest || se.status == http.StatusUnprocessableEntity) {
			resp, err = postJSON(ctx, url, openaiBody(r.Model, false))
		}
		if err != nil {
			return 0, 0, err
		}
	}
	defer resp.Body.Close()
	if jsonNotStream(resp.Header.Get("Content-Type")) {
		return readOpenAIJSON(resp.Body, s)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 4<<10), probeLineMax)
	var reported, contentBytes int
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		payload, ok := openaiFrame(line)
		if !ok {
			continue
		}
		if payload == "[DONE]" {
			break
		}
		var chunk openaiChunk
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		// llama.cpp, LiteLLM and other gateways emit SSE error events with
		// HTTP 200; skipping them passed broken generations off as partial
		// successes (or as a context-free "empty stream").
		if msg := sseErrorMessage(chunk.Error); msg != "" {
			return 0, ttft, fmt.Errorf("engine error: %s", msg)
		}
		if chunk.Usage != nil && chunk.Usage.CompletionTokens > 0 {
			reported = chunk.Usage.CompletionTokens
		}
		for _, c := range chunk.Choices {
			text := c.Delta.Content
			if text == "" {
				text = c.Message.Content // non-stream object, or NDJSON without data:
			}
			if text != "" {
				tokens++
				contentBytes += len(text)
				if ttft == 0 {
					ttft = time.Since(s.At)
				}
			}
		}
		if overBudget(tokens, contentBytes) { // engine ignored max_tokens: hang up
			break
		}
	}
	if err := streamReadErr(ctx, sc.Err(), tokens); err != nil {
		return 0, ttft, err
	}
	n, _ := resolveTokens(tokens, reported)
	if n == 0 {
		return 0, ttft, fmt.Errorf("empty stream")
	}
	return n, ttft, nil
}

// overBudget reports that the client has seen enough generation to hang up.
// Frame count catches engines that ignore max_tokens one token at a time;
// byte count catches a single huge delta that would count as one frame.
func overBudget(tokens, contentBytes int) bool {
	return tokens >= probeTokens || contentBytes >= probeContentBytes
}

// capModel trims and bounds an engine-supplied model id. Empty after trim
// means the caller must not POST: some engines treat "" as "load default".
func capModel(name string) string {
	return core.TruncateClusters(strings.TrimSpace(name), ModelNameMax)
}

func SelectModel(models []core.ModelInfo) string {
	var fallback string
	for _, m := range models {
		name := capModel(m.Name)
		if name == "" || skipProbeModel(name) {
			continue
		}
		if m.SizeVRAM > 0 {
			return name
		}
		if fallback == "" {
			fallback = name
		}
	}
	return fallback
}

func skipProbeModel(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "embed") || strings.Contains(n, "rerank")
}

// resolveTokens picks a probe's token count. Engine-reported usage is
// preferred when it sits in a plausible band around the requested
// generation; anything outside is ignored in favour of content frames
// actually observed. Observed counts are capped at probeTokens because
// the client stops reading there.
func resolveTokens(observed, reported int) (tokens int, trustReported bool) {
	if observed <= 0 {
		return 0, false
	}
	if reported > 0 && reported <= probeTokenTrust {
		return reported, true
	}
	if observed > probeTokens {
		return probeTokens, false
	}
	return observed, false
}

// streamReadErr maps a scanner/body error onto a probe outcome. A cancelled
// caller's context is a real failure (shutdown must not mint a sample). A
// mid-stream drop after at least one token still yields a timed sample:
// hanging up is how we bound engines that ignore max_tokens, and a client
// timeout would otherwise throw away TTFT already measured.
func streamReadErr(ctx context.Context, err error, tokens int) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	if tokens > 0 {
		return nil
	}
	return err
}

// sseErrorMessage extracts an engine-reported failure from a streaming data
// payload. Gateways disagree on the shape: {"error":{"message":…}},
// {"error":"…"}, or other junk; null and absent mean no error. Unrecognized
// junk is capped by core.Snippet; a recognized message passes through as
// sent, clipped to the readout's line width at render time.
func sseErrorMessage(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Message != "" {
		return obj.Message
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return s
	}
	return core.Snippet(raw)
}

func postJSON(ctx context.Context, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	bearer.Apply(req)
	resp, err := client.Do(req)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4*core.SnippetCap))
		msg := fmt.Sprintf("%s: http %s", url, resp.Status)
		if s := core.Snippet(b); s != "" {
			msg += ": " + s
		}
		se := &httpStatusError{status: resp.StatusCode, err: errors.New(msg)}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			se.after = parseRetryAfter(resp)
		}
		return nil, se
	}
	return resp, nil
}

// openaiBody is the chat-completions probe. extra adds fields some older
// OpenAI-compat servers reject (stream_options, max_completion_tokens);
// the caller retries once without them on 400/422.
func openaiBody(model string, extra bool) []byte {
	m := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": promptText},
		},
		"max_tokens":  probeTokens,
		"n":           1, // some engines default n>1; that is n times the budget
		"temperature": 0.2,
		"stream":      true,
	}
	if extra {
		// Newer OpenAI models reject max_tokens; older engines ignore this.
		m["max_completion_tokens"] = probeTokens
		m["stream_options"] = map[string]bool{"include_usage": true}
	}
	body, _ := json.Marshal(m)
	return body
}

type openaiChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// openaiFrame pulls a JSON payload out of one stream line. SSE `data:` is
// the OpenAI shape; a bare `{...}` line is what some proxies emit instead.
func openaiFrame(line string) (payload string, ok bool) {
	if strings.HasPrefix(line, "data:") {
		return strings.TrimSpace(strings.TrimPrefix(line, "data:")), true
	}
	if strings.HasPrefix(line, "{") {
		return line, true
	}
	return "", false
}

func jsonNotStream(ct string) bool {
	ct = strings.ToLower(ct)
	if strings.Contains(ct, "event-stream") || strings.Contains(ct, "ndjson") {
		return false
	}
	return strings.Contains(ct, "json")
}

func readOpenAIJSON(body io.Reader, s *core.ProbeSample) (tokens int, ttft time.Duration, err error) {
	b, err := io.ReadAll(io.LimitReader(body, probeLineMax))
	if err != nil {
		return 0, 0, err
	}
	var chunk openaiChunk
	if json.Unmarshal(b, &chunk) != nil {
		return 0, 0, fmt.Errorf("empty stream")
	}
	if msg := sseErrorMessage(chunk.Error); msg != "" {
		return 0, 0, fmt.Errorf("engine error: %s", msg)
	}
	var reported, n int
	if chunk.Usage != nil && chunk.Usage.CompletionTokens > 0 {
		reported = chunk.Usage.CompletionTokens
	}
	for _, c := range chunk.Choices {
		text := c.Message.Content
		if text == "" {
			text = c.Delta.Content
		}
		if text != "" {
			n++
		}
	}
	tokens, _ = resolveTokens(n, reported)
	if tokens == 0 {
		return 0, 0, fmt.Errorf("empty stream")
	}
	ttft = time.Since(s.At)
	return tokens, ttft, nil
}

type httpStatusError struct {
	status int
	after  time.Duration
	err    error
}

func (e *httpStatusError) Error() string { return e.err.Error() }
func (e *httpStatusError) Unwrap() error { return e.err }

func parseRetryAfter(resp *http.Response) time.Duration {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return defaultRetryAfter
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		secs = min(max(secs, int64(defaultRetryAfter/time.Second)), int64(maxRetryAfter/time.Second))
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return clampRetryAfter(time.Until(t))
	}
	return defaultRetryAfter
}

func clampRetryAfter(d time.Duration) time.Duration {
	if d < defaultRetryAfter {
		return defaultRetryAfter
	}
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}
