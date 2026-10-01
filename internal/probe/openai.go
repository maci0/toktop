package probe

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// probeOpenAI measures a generation against the OpenAI-compatible chat
// completions dialect, whose stream is SSE and whose usage field, when the
// server sends one at all, counts completion tokens rather than decode time.
func probeOpenAI(ctx context.Context, r Request, s *core.ProbeSample) (tokens int, ttft time.Duration, err error) {
	url := r.Base + "/v1/chat/completions"
	resp, err := postOpenAI(ctx, url, r.Model, r.Base)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if jsonNotStream(resp.Header.Get("Content-Type")) {
		return readOpenAIJSON(resp.Body)
	}
	sc := bufio.NewScanner(io.LimitReader(resp.Body, probeStreamMax))
	sc.Buffer(make([]byte, 0, probeBufInit), probeLineMax)
	var reported, contentBytes, reasoning, frames int
	hungUp := false
	for sc.Scan() {
		// Per line, ahead of the frame parser, for the reason the Ollama loop
		// gives: an SSE comment or a non-JSON line is skipped below, and
		// without this it would not count toward anything.
		if overBudget(tokens+reasoning, contentBytes, frames) {
			hungUp = true
			break
		}
		frames++
		line := strings.TrimSpace(sc.Text())
		payload, ok := openaiFrame(line)
		if !ok {
			continue
		}
		if payload == "[DONE]" {
			break
		}
		var chunk openaiChunk
		// A data frame the probe cannot decode is not a frame to skip: the
		// tokens it carried are gone, and the frames after it are counted as
		// if they were the whole generation, so the sample would report a
		// short, plausible measurement with OK set. Same reasoning as the SSE
		// error event below.
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return 0, ttft, fmt.Errorf("decode stream frame: %w", err)
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
			// The same four fields the whole-body path reads, and the
			// longest of them, so a gateway that carries a reasoning-only
			// answer in the message object while streaming does not read as
			// an empty stream here and a working one there.
			if reason := longest(c.Delta.Reasoning, c.Delta.ReasoningContent, c.Message.Reasoning, c.Message.ReasoningContent); reason != "" {
				reasoning++
				contentBytes += len(reason)
			}
		}
		if overBudget(tokens+reasoning, contentBytes, frames) { // engine ignored max_tokens: hang up
			hungUp = true
			break
		}
	}
	if err := streamReadErr(ctx, sc.Err(), tokens, hungUp); err != nil {
		return 0, ttft, err
	}
	n, _ := resolveTokens(tokens, reported)
	if n == 0 {
		return 0, ttft, fmt.Errorf("empty stream")
	}
	return n, ttft, nil
}

// longest returns the longest of the values, or "" when every one is empty.
func longest(values ...string) string {
	longest := ""
	for _, v := range values {
		if len(v) > len(longest) {
			longest = v
		}
	}
	return longest
}

// sseErrorMessage extracts an engine-reported failure from a streaming data
// payload. Gateways disagree on the shape: {"error":{"message":…}},
// {"error":"…"}, or other junk; null and absent mean no error. Only the
// unrecognized-junk path gets core.Snippet, and with it terminal-escape
// stripping. The result is clipped to the readout's line width at render
// time.
func sseErrorMessage(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Message != "" {
		return engineErrorText(obj.Message)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return engineErrorText(s)
	}
	return core.Snippet(raw)
}

// openaiShape is one spelling of the chat-completions request, the axes on
// which OpenAI-compatible servers disagree about how a bounded generation is
// asked for.
type openaiShape int

const (
	// shapeBoth names the cap under both fields, and asks for usage in the
	// stream. Every server that ignores an unknown field accepts this.
	shapeBoth openaiShape = iota
	// shapeCompletion drops max_tokens for servers that reject it outright:
	// the reasoning models and their gateways answer 400 "unsupported
	// parameter" to the legacy name.
	shapeCompletion
	// shapeLegacy drops max_completion_tokens and stream_options for older
	// llama.cpp and strict proxies that 400 on either.
	shapeLegacy
	// shapeReasoning drops temperature for the reasoning models (o1, o3,
	// gpt-5 and the gateways fronting them) that answer any other value than
	// their default with a 400 "unsupported_value". It is the last shape
	// because it is the widest concession: a model that rejects a sampling
	// parameter is refusing to be sampled at all, so the probe gives up the
	// temperature it holds the rest of the walk to.
	shapeReasoning
)

// openaiShapes is the order postOpenAI walks. The first shape is a superset of
// the others, so a server that rejects one field it never needed is answered
// by a shape carrying the field it does accept; a server needing both names
// rejected shapeBoth, and gets a shape naming only its own. A server that
// refuses a sampling value as well rejects every shape before this one, and
// without shapeReasoning the walk ended on a 400 for an engine that is
// answering every other request.
var openaiShapes = []openaiShape{shapeBoth, shapeCompletion, shapeLegacy, shapeReasoning}

// shapeMemo remembers, per engine base URL, the last shape that answered.
// The walk is free per shape -- a 400/422 is refused before any generation
// runs -- but it is not free per wave: an engine that needs shapeLegacy costs
// three refused POSTs before every measurement, once per wave, for as long as
// the run lasts. On a billed gateway that is a request multiplier on a probe
// whose whole point is to be small. Recording the shape that answered turns
// every later probe of that engine into a single POST.
//
// The entry is the full base URL, not the model: the refusal is about the
// server's request shape, which does not vary with the model. An engine that
// serves two models only one of which refuses a shape still has to walk to
// find out, and the memo is rewritten from the answer either way.
var shapeMemo sync.Map // base URL -> openaiShape

// memoShape returns the shape that last answered for base, if any.
func memoShape(base string) (openaiShape, bool) {
	v, ok := shapeMemo.Load(base)
	if !ok {
		return shapeBoth, false
	}
	shape, ok := v.(openaiShape)
	return shape, ok
}

// rememberShape records the shape that answered for base. A shape later in the
// walk than the one already recorded is kept: the walk only ever widens its
// concessions, so a later shape is strictly the more compatible one.
func rememberShape(base string, shape openaiShape) {
	if prev, ok := memoShape(base); !ok || shape > prev {
		shapeMemo.Store(base, shape)
	}
}

// forgetShape drops base's memo. A collector run can change the address it
// probes an engine under (one that stops resolving and answers again on a new
// port), and a memo left over from the old endpoint would send the first
// probe of the new one straight into a refusal the walk would have recovered
// from.
func forgetShape(base string) { shapeMemo.Delete(base) }

// postOpenAI POSTs the probe, walking the request shapes on a rejection. Only
// 400 and 422 continue the walk: they are refused before any generation runs,
// so the whole walk is free, and walking it is the only way a probe can reach
// an engine whose cap field is named the other way. 429, 503 and transport
// failures end it, since a retry there multiplies billed generations.
//
// The walk starts from the shape that last answered for this base rather than
// at shapeBoth, so an engine that rejects a field is not made to reject it
// again on every wave for the rest of the run. A refusal at the remembered
// shape does not end the probe: the memo can be stale against an engine that
// changed what it accepts, so the walk resumes where the memo left off, a
// shape that answers rewrites it, and a refusal at or before the memo drops it
// so the next wave starts over rather than paying that refusal every wave.
func postOpenAI(ctx context.Context, url, model, base string) (*http.Response, error) {
	from, memoized := memoShape(base)
	if !memoized {
		from = 0 // an engine with no memo is always walked from the top
	}
	var err error
	for _, shape := range openaiShapes[from:] {
		var resp *http.Response
		if resp, err = postJSON(ctx, url, openaiBody(model, shape)); err == nil {
			rememberShape(base, shape)
			return resp, nil
		}
		var se *httpStatusError
		if !errors.As(err, &se) || (se.status != http.StatusBadRequest && se.status != http.StatusUnprocessableEntity) {
			return nil, err
		}
		if memoized && shape <= from {
			forgetShape(base)
		}
	}
	return nil, err
}

// openaiBody is the chat-completions probe in one of its shapes. Every shape
// caps the generation and asks for a single choice; what differs is the field
// naming that cap and, on shapeReasoning, whether a sampling value is sent at
// all.
func openaiBody(model string, shape openaiShape) []byte {
	m := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": promptText},
		},
		"n":           1, // some engines default n>1; that is n times the budget
		"temperature": 0.2,
		"stream":      true,
	}
	switch shape {
	case shapeCompletion:
		m["max_completion_tokens"] = probeTokens
		m["stream_options"] = map[string]bool{"include_usage": true}
	case shapeLegacy:
		m["max_tokens"] = probeTokens
	case shapeReasoning:
		m["max_completion_tokens"] = probeTokens
		m["stream_options"] = map[string]bool{"include_usage": true}
		// The model runs at whatever temperature it defaults to, so the probe
		// reports the engine's own decode rate rather than a rate this request
		// asked for. There is no field to drop and put back: a reasoning model
		// that 400s on one value 400s on the default spelled out, and the walk
		// has no shape left to fall back on.
		delete(m, "temperature")
	default:
		m["max_tokens"] = probeTokens
		m["max_completion_tokens"] = probeTokens
		m["stream_options"] = map[string]bool{"include_usage": true}
	}
	body, _ := json.Marshal(m)
	return body
}

type openaiChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		Message struct {
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// openaiFrame pulls a JSON payload out of one stream line. SSE `data:` is
// the OpenAI shape; a bare `{...}` line is what some proxies emit instead. A
// data field with nothing after it carries no payload, so it is not a frame:
// reporting one would hand the caller an empty string, which it decodes and
// fails the whole probe over.
func openaiFrame(line string) (payload string, ok bool) {
	if strings.HasPrefix(line, "data:") {
		payload = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	} else if strings.HasPrefix(line, "{") {
		payload = line
	} else {
		return "", false
	}
	return payload, payload != ""
}

// jsonNotStream reports whether a Content-Type names a plain JSON body rather
// than an SSE or NDJSON stream. The header value is a protocol token, so it
// is folded with core.FoldASCII: an ASCII media type must not be satisfied by
// a spelling strings.ToLower invents out of U+0130 or U+212A, and the two
// answers here decide whether the body is parsed as a stream at all.
func jsonNotStream(ct string) bool {
	ct = core.FoldASCII(ct)
	if strings.Contains(ct, "event-stream") || strings.Contains(ct, "ndjson") {
		return false
	}
	return strings.Contains(ct, "json")
}

// readOpenAIJSON parses a whole-body completion, the shape an engine returns
// when it ignores stream:true. It always reports a ttft of 0: there is no
// first-token instant in a body that arrives in one piece, and Run accounts
// for that by measuring throughput over the full exchange.
func readOpenAIJSON(body io.Reader) (tokens int, ttft time.Duration, err error) {
	b, err := io.ReadAll(io.LimitReader(body, probeLineMax+1))
	if err != nil {
		return 0, 0, fmt.Errorf("read response: %w", err)
	}
	if len(b) > probeLineMax {
		return 0, 0, fmt.Errorf("response too large")
	}
	var chunk openaiChunk
	// A body that is not the expected JSON is a different failure from a
	// well-formed response with no content. Reporting both as "empty stream"
	// sends the operator to inspect a model that never answered.
	if err := json.Unmarshal(b, &chunk); err != nil {
		return 0, 0, fmt.Errorf("decode response: %w", err)
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
		// A thinking model that answers in one piece carries its whole trace
		// in message.reasoning_content and can leave content empty, so a
		// count that read content alone called a working engine an empty
		// stream. The same four fields the streaming path reads.
		if text != "" || c.Message.Reasoning != "" || c.Message.ReasoningContent != "" ||
			c.Delta.Reasoning != "" || c.Delta.ReasoningContent != "" {
			n++
		}
	}
	tokens, _ = resolveTokens(n, reported)
	if tokens == 0 {
		return 0, 0, fmt.Errorf("empty stream")
	}
	// Sampling the whole exchange here and returning it as the time to first
	// token left total-ttft as the gap between two nanosecond-scale reads
	// around the same call, so the rate became the token count over tens of
	// nanoseconds. A 0 sends Run to the whole exchange instead.
	return tokens, 0, nil
}
