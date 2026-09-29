package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// firstTokenDelay is one Windows clock tick plus slack.
const firstTokenDelay = 25 * time.Millisecond

func TestSelectModel(t *testing.T) {
	cases := []struct {
		name   string
		models []core.ModelInfo
		want   string
	}{
		{name: "empty"},
		{
			name:   "non-generation models",
			models: []core.ModelInfo{{Name: "  "}, {Name: "EMBED-large", SizeVRAM: 1}, {Name: "ReRank-v2", SizeVRAM: 1}},
		},
		{
			name:   "first catalog fallback",
			models: []core.ModelInfo{{Name: "embed"}, {Name: " first "}, {Name: "second"}},
			want:   "first",
		},
		{
			name:   "first loaded generation model",
			models: []core.ModelInfo{{Name: "catalog"}, {Name: "embed", SizeVRAM: 1}, {Name: " loaded ", SizeVRAM: 1}, {Name: "other", SizeVRAM: 2}},
			want:   "loaded",
		},
		{
			// core.ModelName composes to NFC before it caps, so a decomposed id
			// is measured and returned in the composed form: 129 decomposed
			// accents are 129 clusters either way, and the cap still lands
			// between clusters rather than splitting one.
			name:   "whole grapheme cap",
			models: []core.ModelInfo{{Name: " " + strings.Repeat("e\u0301", core.ModelNameMax+1) + " "}},
			want:   strings.Repeat("\u00e9", core.ModelNameMax),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SelectModel(tc.models); got != tc.want {
				t.Fatalf("SelectModel() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunOpenAIStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := http.NewResponseController(w)
		// Windows' clock ticks at 15.6ms; an instant reply lands the whole
		// exchange inside one tick and TTFT reads as 0. Outlast a tick so
		// the timing assertions below measure something.
		time.Sleep(firstTokenDelay)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"He\"}}]}\n\n"))
		f.Flush()
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n"))
		f.Flush()
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"!\"}}]}\n\n"))
		f.Flush()
		w.Write([]byte("data: {\"usage\":{\"completion_tokens\":42}}\n\n"))
		f.Flush()
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
	if !s.OK || s.Err != "" {
		t.Fatalf("probe failed: %+v", s)
	}
	if s.Tokens != 42 { // usage wins over delta counting
		t.Errorf("tokens = %d, want 42", s.Tokens)
	}
	if s.TTFTms <= 0 {
		t.Errorf("ttft not measured: %v", s.TTFTms)
	}
	if s.TokPS <= 0 {
		t.Errorf("tokps not derived: %v", s.TokPS)
	}
}

func TestRunOllamaStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write([]byte(
			`{"response":"one","done":false}` + "\n" +
				`{"response":"two","done":false}` + "\n" +
				`{"response":"","done":true,"eval_count":9,"eval_duration":3000000000}` + "\n"))
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindOllama, Base: srv.URL, Model: "m"})
	if !s.OK {
		t.Fatalf("probe failed: %+v", s)
	}
	if s.Tokens != 9 {
		t.Errorf("tokens = %d, want eval_count 9", s.Tokens)
	}
	want := float64(9) / 3.0
	if diff := s.TokPS - want; diff > 0.01 || diff < -0.01 {
		t.Errorf("tokps = %v, want ~%v", s.TokPS, want)
	}
}

func TestRunRejectsUsageWithoutContent(t *testing.T) {
	for _, tc := range []struct {
		name        string
		kind        string
		contentType string
		body        string
		wantErr     string
	}{
		{
			name:        "stream usage only",
			kind:        core.KindVLLM,
			contentType: "text/event-stream",
			body:        "data: {\"usage\":{\"completion_tokens\":32}}\n\ndata: [DONE]\n\n",
		},
		{
			name:        "stream malformed content",
			kind:        core.KindVLLM,
			contentType: "text/event-stream",
			body:        "data: {\"choices\":[{\"delta\":{\"content\":42}}]}\n\ndata: {\"usage\":{\"completion_tokens\":32}}\n\ndata: [DONE]\n\n",
			// The frame is a decode failure, not an absent one: skipping it
			// would have counted the frames after it as a whole generation.
			wantErr: "decode stream frame:",
		},
		{
			name:        "json usage only",
			kind:        core.KindVLLM,
			contentType: "application/json",
			body:        `{"usage":{"completion_tokens":32}}`,
		},
		{
			name:        "json refusal",
			kind:        core.KindVLLM,
			contentType: "application/json",
			body:        `{"choices":[{"message":{"content":null,"refusal":"declined"}}],"usage":{"completion_tokens":32}}`,
		},
		{
			name:        "ollama usage only",
			kind:        core.KindOllama,
			contentType: "application/x-ndjson",
			body:        `{"response":"","done":true,"eval_count":32,"eval_duration":1000000000}` + "\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			wantErr := tc.wantErr
			if wantErr == "" {
				wantErr = "empty stream"
			}
			s := Run(context.Background(), Request{Kind: tc.kind, Base: srv.URL, Model: "m"})
			if s.OK || !strings.HasPrefix(s.Err, wantErr) || s.Tokens != 0 || s.TTFTms != 0 || s.TokPS != 0 {
				t.Fatalf("usage without content must not produce a measurement: %+v", s)
			}
		})
	}
}

func TestRunDoesNotReplayRedirects(t *testing.T) {
	for _, kind := range []string{core.KindVLLM, core.KindOllama} {
		for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
			t.Run(fmt.Sprintf("%s/%d", kind, status), func(t *testing.T) {
				var requests atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					http.Redirect(w, r, r.URL.Path, status)
				}))
				defer srv.Close()

				s := Run(context.Background(), Request{Kind: kind, Base: srv.URL, Model: "m"})
				if got := requests.Load(); got != 1 {
					t.Fatalf("generation request sent %d times, want 1", got)
				}
				if s.OK || !strings.Contains(s.Err, fmt.Sprintf("http %d", status)) {
					t.Fatalf("redirect must surface its HTTP status: %+v", s)
				}
			})
		}
	}
}

func TestRunHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
	if s.OK || s.Err == "" {
		t.Fatalf("expected failure sample, got %+v", s)
	}
}

// Engines explain rejections in the error body ("model not found", bad api
// key, OOM); the surfaced Err must carry that text, not just the status.
func TestRunHTTPErrorCarriesEngineBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "{\"error\":\"model 'm' not found, try pulling it first\"}")
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
	if s.OK || s.Err == "" {
		t.Fatalf("expected failure sample, got %+v", s)
	}
	if !strings.Contains(s.Err, "400") {
		t.Errorf("err missing status: %q", s.Err)
	}
	if !strings.Contains(s.Err, "not found") {
		t.Errorf("err missing engine explanation: %q", s.Err)
	}
}

func TestRunEmptyStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
	if s.OK || s.Err == "" {
		t.Fatalf("empty stream should fail, got %+v", s)
	}
}

// An Ollama engine closing the stream without tokens (crashed model, OOM)
// must surface as a failed probe, not a silent zero-throughput success.
func TestRunOllamaEmptyStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"response":"","done":true}` + "\n"))
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindOllama, Base: srv.URL, Model: "m"})
	if s.OK || s.Err == "" {
		t.Fatalf("empty ollama stream should fail, got %+v", s)
	}
}

// Ollama streams mid-stream failures as {"error":…} NDJSON lines with HTTP
// 200. Counting them as tokens reported a green probe with invented TTFT and
// throughput; the engine's own explanation must surface as Err instead.
func TestRunOllamaStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"error":"model 'm' requires more system memory (18 GiB) than is available"}` + "\n"))
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindOllama, Base: srv.URL, Model: "m"})
	if s.OK || s.Err == "" {
		t.Fatalf("streamed engine error should fail the probe, got %+v", s)
	}
	if !strings.Contains(s.Err, "more system memory") {
		t.Errorf("err missing engine explanation: %q", s.Err)
	}
}

// Keep-alive frames without content carry no tokens: counting every non-done
// line fabricated throughput out of empty chunks.
func TestRunOllamaEmptyChunksAreNotTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(
			`{"response":"","done":false}` + "\n" +
				`{"response":"","done":true}` + "\n"))
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindOllama, Base: srv.URL, Model: "m"})
	if s.OK || s.Err == "" {
		t.Fatalf("contentless stream should fail, got %+v", s)
	}
}

// OpenAI-compatible gateways (llama.cpp, LiteLLM) emit SSE error events with
// HTTP 200. Arriving after real tokens they previously passed as a partial
// success; the generation failed and must be reported as such.
func TestRunOpenAIStreamErrorObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"He\"}}]}\n\n" +
			"data: {\"error\":{\"message\":\"context length exceeded\"}}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
	if s.OK || s.Err == "" {
		t.Fatalf("streamed error event should fail the probe, got %+v", s)
	}
	if !strings.Contains(s.Err, "context length exceeded") {
		t.Errorf("err missing engine explanation: %q", s.Err)
	}
}

// The string flavor of SSE error events must be recognized too.
func TestRunOpenAIStreamErrorString(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("data: {\"error\":\"quota exceeded\"}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
	if s.OK || s.Err == "" {
		t.Fatalf("streamed error event should fail the probe, got %+v", s)
	}
	if !strings.Contains(s.Err, "quota exceeded") {
		t.Errorf("err missing engine explanation: %q", s.Err)
	}
}

// A usage-bearing chunk with an explicit null error member is a normal final
// frame, not a failure.
func TestRunOpenAINullErrorIsNotFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"error\":null}\n\n" +
			"data: {\"usage\":{\"completion_tokens\":2}}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
	if !s.OK || s.Tokens != 2 {
		t.Fatalf("null error must not fail the probe, got %+v", s)
	}
}

// An empty or whitespace model id must not POST: some engines treat "" as
// "load the default", which is VRAM and (on a billed gateway) tokens.
func TestRunEmptyModelDoesNotPost(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits++
	}))
	defer srv.Close()

	for _, model := range []string{"", "  ", "\t"} {
		s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: model})
		if s.OK || s.Err == "" {
			t.Fatalf("model %q: expected failure sample, got %+v", model, s)
		}
		if !strings.Contains(s.Err, "no model") {
			t.Errorf("model %q err = %q, want no model", model, s.Err)
		}
	}
	if hits != 0 {
		t.Fatalf("engine was hit %d times, want 0", hits)
	}
}

// Engine-supplied model ids are untrusted and can be megabytes from
// /v1/models. The request must carry a capped id, never the raw string.
func TestRunCapsModelName(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	huge := strings.Repeat("m", core.ModelNameMax+64)
	Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: huge})
	name, _ := got["model"].(string)
	if name != strings.Repeat("m", core.ModelNameMax) {
		t.Errorf("model id len = %d, want %d", len(name), core.ModelNameMax)
	}
}

func TestRunRequestsBoundedGeneration(t *testing.T) {
	var gotOpenAI map[string]any
	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotOpenAI)
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer openai.Close()
	Run(context.Background(), Request{Kind: core.KindVLLM, Base: openai.URL, Model: "m"})
	if n := gotOpenAI["max_tokens"]; n != float64(probeTokens) {
		t.Errorf("openai max_tokens = %v, want %d", n, probeTokens)
	}
	if n := gotOpenAI["max_completion_tokens"]; n != float64(probeTokens) {
		t.Errorf("openai max_completion_tokens = %v, want %d", n, probeTokens)
	}
	if n := gotOpenAI["n"]; n != float64(1) {
		t.Errorf("openai n = %v, want 1", n)
	}

	var gotOllama map[string]any
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotOllama)
		w.Write([]byte(`{"response":"","done":true}` + "\n"))
	}))
	defer ollama.Close()
	Run(context.Background(), Request{Kind: core.KindOllama, Base: ollama.URL, Model: "m"})
	opts := gotOllama["options"].(map[string]any)
	if n := opts["num_predict"]; n != float64(probeTokens) {
		t.Errorf("ollama num_predict = %v, want %d", n, probeTokens)
	}
	think, ok := gotOllama["think"].(bool)
	if !ok || think {
		t.Errorf("ollama think = %v (%v), want false", gotOllama["think"], ok)
	}
}

// Engines that ignore max_tokens keep streaming; the client must hang up
// after probeTokens content frames so a billed gateway cannot run until the
// HTTP timeout.
//
// The server parks after the cap and the probe has to return anyway. Asserting
// only on s.Tokens cannot see the hang-up: resolveTokens clamps whatever it
// observed back to probeTokens, so a client that drained every frame and
// reported afterwards would produce the same number.
func TestRunOpenAIStopsAfterProbeTokens(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := http.NewResponseController(w)
		for range probeTokens + 1 {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
			f.Flush()
		}
		<-release // the cap has to be reached without this ever being closed
	}))
	t.Cleanup(srv.Close)

	done := make(chan core.ProbeSample, 1)
	go func() { done <- Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"}) }()

	var s core.ProbeSample
	select {
	case s = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("probe kept reading past the cap instead of hanging up")
	}
	unblock()

	if !s.OK {
		t.Fatalf("probe failed: %+v", s)
	}
	if s.Tokens != probeTokens {
		t.Errorf("tokens = %d, want client cap %d", s.Tokens, probeTokens)
	}
}

// A single huge content delta counts as one frame, so the frame cap would
// not hang up. Byte budget must.
func TestRunOpenAIStopsOnContentBytes(t *testing.T) {
	payload := strings.Repeat("x", probeContentBytes+8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := http.NewResponseController(w)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", payload)
		f.Flush()
		for range 8 {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"more\"}}]}\n\n")
			f.Flush()
		}
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
	if !s.OK {
		t.Fatalf("probe failed: %+v", s)
	}
	if s.Tokens != 1 {
		t.Errorf("tokens = %d, want hang-up after the first oversized frame", s.Tokens)
	}
}

func TestRunOllamaStopsOnContentBytes(t *testing.T) {
	payload := strings.Repeat("x", probeContentBytes+8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		f := http.NewResponseController(w)
		fmt.Fprintf(w, `{"response":%q,"done":false}`+"\n", payload)
		f.Flush()
		for range 8 {
			fmt.Fprintf(w, `{"response":"more","done":false}`+"\n")
			f.Flush()
		}
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindOllama, Base: srv.URL, Model: "m"})
	if !s.OK {
		t.Fatalf("probe failed: %+v", s)
	}
	if s.Tokens != 1 {
		t.Errorf("tokens = %d, want hang-up after the first oversized frame", s.Tokens)
	}
}

// The Ollama twin of the hang-up test, and it needs the same shape: the
// engine-reported eval_count here exceeds probeTokenTrust, so resolveTokens
// falls back to the cap whether the client stopped reading or not.
func TestRunOllamaStopsAfterProbeTokens(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		f := http.NewResponseController(w)
		for range probeTokens + 1 {
			fmt.Fprintf(w, `{"response":"x","done":false}`+"\n")
			f.Flush()
		}
		<-release
		fmt.Fprintf(w, `{"response":"","done":true,"eval_count":999,"eval_duration":1000000}`+"\n")
	}))
	t.Cleanup(srv.Close)

	done := make(chan core.ProbeSample, 1)
	go func() { done <- Run(context.Background(), Request{Kind: core.KindOllama, Base: srv.URL, Model: "m"}) }()

	var s core.ProbeSample
	select {
	case s = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("probe kept reading past the cap instead of hanging up")
	}
	unblock()

	if !s.OK {
		t.Fatalf("probe failed: %+v", s)
	}
	if s.Tokens != probeTokens {
		t.Errorf("tokens = %d, want client cap %d", s.Tokens, probeTokens)
	}
}

func TestRunStopsOnReasoningBudget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  string
		frame string
		late  string
	}{
		{"reasoning_content", core.KindVLLM, `data: {"choices":[{"delta":{"reasoning_content":%q}}]}` + "\n\n", `data: {"choices":[{"delta":{"content":"late"}}]}` + "\n\n"},
		{"reasoning", core.KindVLLM, `data: {"choices":[{"delta":{"reasoning":%q}}]}` + "\n\n", `data: {"choices":[{"delta":{"content":"late"}}]}` + "\n\n"},
		{"thinking", core.KindOllama, `{"thinking":%q}` + "\n", `{"response":"late","done":true}` + "\n"},
	} {
		for _, budget := range []struct {
			name    string
			payload string
			frames  int
		}{
			{"bytes", strings.Repeat("r", probeContentBytes), 1},
			{"cumulative_bytes", strings.Repeat("r", probeContentBytes/2), 2},
			{"frames", "r", probeTokens},
		} {
			t.Run(tc.name+"/"+budget.name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					for range budget.frames {
						fmt.Fprintf(w, tc.frame, budget.payload)
					}
					fmt.Fprint(w, tc.late)
				}))
				defer srv.Close()
				s := Run(context.Background(), Request{Kind: tc.kind, Base: srv.URL, Model: "m"})
				if s.OK || !strings.Contains(s.Err, "empty stream") {
					t.Fatalf("reasoning budget must stop before answer content: %+v", s)
				}
			})
		}
	}
}

// A stream whose frames carry no content and no reasoning trips neither the
// token budget nor the byte budget: a keepalive comment, a role-only opening
// frame, or a gateway replaying empty choices. The probe has to hang up on the
// frame count or the generation stays open, and billed, until the 30s client
// timeout. The handler stays blocked after the last frame it will write, so a
// probe that only ends because the stream closed cannot pass this.
func TestRunStopsOnFrameBudget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kind     string
		ct       string
		keepaliv string
	}{
		{"openai_sse_comment", core.KindVLLM, "text/event-stream", ": ping\n\n"},
		{"openai_empty_choices", core.KindVLLM, "text/event-stream", `data: {"choices":[]}` + "\n\n"},
		{"ollama_no_content", core.KindOllama, "application/x-ndjson", `{"done":false}` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The handler parks until the client hangs up. It has to be
			// released before Close, which blocks on outstanding requests.
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.ct)
				flusher, _ := w.(http.Flusher)
				for range probeFrameMax + 1 {
					fmt.Fprint(w, tc.keepaliv)
					if flusher != nil {
						flusher.Flush()
					}
				}
				<-release // only the client hanging up ends this handler
			}))
			defer srv.Close()
			defer unblock()

			done := make(chan core.ProbeSample, 1)
			go func() { done <- Run(context.Background(), Request{Kind: tc.kind, Base: srv.URL, Model: "m"}) }()

			select {
			case s := <-done:
				if s.OK || !strings.Contains(s.Err, "empty stream") {
					t.Fatalf("a tokenless stream must not report a measurement: %+v", s)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("probe did not hang up on an endless tokenless stream")
			}
		})
	}
}

// The frame budget is a hang-up, not a token count: a frame that carries
// content is still worth probeTokens of it, and one that carries none must not
// push a real generation past the cap the token budget sets.
func TestOverBudgetFrameTerm(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		tokens, content, frames int
		want                    bool
	}{
		{"under_every_budget", probeTokens - 1, probeContentBytes - 1, probeFrameMax - 1, false},
		{"frame_budget", 0, 0, probeFrameMax, true},
		{"token_budget", probeTokens, 0, 0, true},
		{"byte_budget", 0, probeContentBytes, 0, true},
	} {
		if got := overBudget(tc.tokens, tc.content, tc.frames); got != tc.want {
			t.Errorf("%s: overBudget(%d, %d, %d) = %v, want %v",
				tc.name, tc.tokens, tc.content, tc.frames, got, tc.want)
		}
	}
}

func TestRunOpenAIStopsOnStreamBytes(t *testing.T) {
	frame := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"r\"}}]}\n\n"
	prefix := strings.Repeat(frame, 2*probeStreamMax/len(frame)+1)
	if len(prefix) <= probeStreamMax {
		t.Fatal("test prefix must exceed the stream byte limit")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, prefix+"data: {\"choices\":[{\"delta\":{\"content\":\"late\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
	if s.OK {
		t.Fatalf("reasoning-only flood should fail, got %+v", s)
	}
	if !strings.Contains(s.Err, "empty stream") {
		t.Errorf("err = %q, want empty stream", s.Err)
	}
}

func TestRunOllamaStopsOnStreamBytes(t *testing.T) {
	frame := `{"thinking":"r"}` + "\n"
	prefix := strings.Repeat(frame, 2*probeStreamMax/len(frame)+1)
	if len(prefix) <= probeStreamMax {
		t.Fatal("test prefix must exceed the stream byte limit")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, prefix+`{"response":"late","done":true}`+"\n")
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindOllama, Base: srv.URL, Model: "m"})
	if s.OK {
		t.Fatalf("thinking-only flood should fail, got %+v", s)
	}
	if !strings.Contains(s.Err, "empty stream") {
		t.Errorf("err = %q, want empty stream", s.Err)
	}
}

// A usage field far past the requested generation is engine junk, not a
// measurement. Fall back to the content frames actually observed.
func TestRunOpenAIRejectsUnboundedUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: {\"usage\":{\"completion_tokens\":1000000000}}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
	if !s.OK || s.Tokens != 1 {
		t.Fatalf("unbounded usage must not win, got %+v", s)
	}
}

func TestRunOllamaRejectsUnboundedEvalCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(
			`{"response":"one","done":false}` + "\n" +
				`{"response":"two","done":false}` + "\n" +
				`{"response":"","done":true,"eval_count":1000000000,"eval_duration":1}` + "\n"))
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindOllama, Base: srv.URL, Model: "m"})
	if !s.OK || s.Tokens != 2 {
		t.Fatalf("unbounded eval_count must not win, got %+v", s)
	}
}

// Usage on the last content chunk must replace the delta count, not add to it.
func TestRunOpenAIUsageNotStackedOnContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"completion_tokens\":4}}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
	if !s.OK || s.Tokens != 4 {
		t.Fatalf("usage must replace delta count, got %+v", s)
	}
}

// A mid-stream transport drop after the first token still yields a sample:
// throwing it away would hide TTFT already measured.
func TestRunOpenAIKeepsPartialOnReadError(t *testing.T) {
	old := client.Timeout
	client.Timeout = 200 * time.Millisecond
	defer func() { client.Timeout = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := http.NewResponseController(w)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		f.Flush()
		time.Sleep(time.Second)
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
	if !s.OK || s.Tokens < 1 {
		t.Fatalf("partial stream should still sample, got %+v", s)
	}
}

// Shutdown must not mint a success from a stream cancelled under us.
func TestRunCanceledContextFails(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := http.NewResponseController(w)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		f.Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	s := Run(ctx, Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
	if s.OK {
		t.Fatalf("canceled probe should fail, got %+v", s)
	}
}

// Older OpenAI-compat servers 400 on stream_options / max_completion_tokens.
// One retry without those fields must land; 400 is not a spend multiplier
// the way retrying 429 would be.
func TestRunOpenAIRetriesWithoutExtraFields(t *testing.T) {
	var n int
	var retry map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if n == 1 {
			if _, ok := body["stream_options"]; !ok {
				t.Error("first request missing stream_options")
			}
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"unknown field stream_options"}`)
			return
		}
		retry = body
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
	if n != 2 {
		t.Fatalf("POSTs = %d, want 2 (one retry)", n)
	}
	if !s.OK {
		t.Fatalf("retry should succeed, got %+v", s)
	}
	if _, ok := retry["stream_options"]; ok {
		t.Error("retry still sent stream_options")
	}
	if _, ok := retry["max_completion_tokens"]; ok {
		t.Error("retry still sent max_completion_tokens")
	}
	if retry["max_tokens"] != float64(probeTokens) {
		t.Errorf("retry max_tokens = %v, want %d", retry["max_tokens"], probeTokens)
	}
}

// 429 is a spend signal: retrying the same generation would multiply cost.
func TestRunOpenAIDoesNotRetry429(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n++
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"rate limited"}`)
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
	if n != 1 {
		t.Fatalf("POSTs = %d, want 1 (no retry on 429)", n)
	}
	if s.OK || s.RetryAfter != 30*time.Second {
		t.Fatalf("429 sample = %+v, want RetryAfter 30s", s)
	}
	if !strings.Contains(s.Err, "429") {
		t.Errorf("err missing 429: %q", s.Err)
	}
}

func TestRetryAfterLargeDeltaSeconds(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{"9223372036", retryAfterMax},
		{"9223372037", retryAfterMax},
		{"9223372036854775807", retryAfterMax},
		{"-9223372037", retryAfterDefault},
		{"-9223372036854775808", retryAfterDefault},
		{"0", retryAfterDefault},
		{"15", retryAfterDefault},
		{"30", 30 * time.Second},
		{"300", retryAfterMax},
		{"301", retryAfterMax},
	} {
		t.Run(tc.header, func(t *testing.T) {
			resp := &http.Response{Header: make(http.Header)}
			resp.Header.Set("Retry-After", tc.header)
			if got := parseRetryAfter(resp); got != tc.want {
				t.Errorf("Retry-After %q = %s, want %s", tc.header, got, tc.want)
			}
		})
	}
}

func TestRunOpenAIRetryAfterFloorAndCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
	if s.RetryAfter != retryAfterDefault {
		t.Errorf("tiny Retry-After = %s, want floor %s", s.RetryAfter, retryAfterDefault)
	}

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "99999")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv2.Close()
	s = Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv2.URL, Model: "m"})
	if s.RetryAfter != retryAfterMax {
		t.Errorf("huge Retry-After = %s, want cap %s", s.RetryAfter, retryAfterMax)
	}
}

// Engines that ignore stream:true return a JSON object with HTTP 200.
// That used to scan as an empty SSE stream and hide the engine error.
func TestRunOpenAIJSONErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"error":{"message":"model overloaded"}}`)
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
	if s.OK || !strings.Contains(s.Err, "model overloaded") {
		t.Fatalf("json error body should surface, got %+v", s)
	}
}

func TestRunOpenAIJSONResponseLimit(t *testing.T) {
	completion := `{"choices":[{"message":{"content":"one"}}],"usage":{"completion_tokens":1}}`
	atLimit := completion + strings.Repeat(" ", probeLineMax-len(completion))
	for _, tc := range []struct {
		name string
		body string
		ok   bool
	}{
		{"below limit", atLimit[:len(atLimit)-1], true},
		{"at limit", atLimit, true},
		{"over limit", atLimit + " ", false},
		{"hidden trailing object", atLimit + `{"error":"failed"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
			if tc.ok {
				if !s.OK || s.Tokens != 1 || s.Err != "" {
					t.Fatalf("bounded completion rejected: %+v", s)
				}
			} else if s.OK || s.Err != "response too large" || s.Tokens != 0 || s.TTFTms != 0 || s.TokPS != 0 {
				t.Fatalf("oversized response must not produce a measurement: %+v", s)
			}
		})
	}
}

func TestRunOpenAINonStreamCompletion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"one two three"}}],"usage":{"completion_tokens":8}}`)
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
	if !s.OK || s.Tokens != 8 {
		t.Fatalf("non-stream completion = %+v, want 8 tokens", s)
	}
	// A whole-body response has no first-token instant, so throughput is
	// measured over the full exchange. Dividing by the nanosecond-scale gap
	// between the two clock reads instead reported tens of millions of tokens
	// per second for an 8-token answer, varying with scheduler noise.
	if s.TokPS <= 0 || s.TokPS > maxNonStreamRateForTest {
		t.Fatalf("non-stream rate = %v tok/s, want a whole-exchange rate; a decode window was inferred from a body with no first-token instant", s.TokPS)
	}
}

// maxNonStreamRateForTest bounds the rate a non-stream probe may report. Even
// an instant local engine decoding 8 tokens takes far longer than a nanosecond
// of wall clock, so any rate past this came from a denominator of measurement
// jitter rather than from the exchange.
const maxNonStreamRateForTest = 1e6

// Some proxies emit NDJSON without the SSE data: prefix.
func TestRunOpenAIBareJSONLine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"choices":[{"delta":{"content":"hi"}}]}` + "\n" +
			`{"usage":{"completion_tokens":3}}` + "\n"))
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
	if !s.OK || s.Tokens != 3 {
		t.Fatalf("bare json line = %+v, want usage 3", s)
	}
}

// Non-stream Ollama puts the whole reply on the done frame. Ignoring that
// content reported an empty stream when eval_count was absent.
func TestRunOllamaNonStreamDoneWithResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"response":"one two three","done":true}` + "\n"))
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindOllama, Base: srv.URL, Model: "m"})
	if !s.OK || s.Tokens != 1 {
		t.Fatalf("done-frame content = %+v, want 1 token", s)
	}
}

func TestHttpStatusErrorUnwrap(t *testing.T) {
	baseErr := io.ErrUnexpectedEOF
	err := &httpStatusError{status: 500, err: baseErr}
	if !errors.Is(err, baseErr) {
		t.Errorf("errors.Is(err, baseErr) = false, want true")
	}
	if err.Unwrap() != baseErr {
		t.Errorf("err.Unwrap() = %v, want %v", err.Unwrap(), baseErr)
	}
	if err.Error() != baseErr.Error() {
		t.Errorf("err.Error() = %q, want %q", err.Error(), baseErr.Error())
	}
}

func FuzzReadEngineJSON(f *testing.F) {
	for _, seed := range []string{
		`{"choices":[{"message":{"content":"one two three"}}],"usage":{"completion_tokens":3}}`,
		`{"choices":[{"delta":{"content":"a"}}]}`,
		`{"error":"quota exceeded"}`,
		`{"error":{"message":"model 'x' is busy"}}`,
		`{"error":{"message":"` + strings.Repeat("x", 600) + `"}}`,
		`data: {"choices":[{"delta":{"content":"He"}}]}`,
		"{\"usage\":{\"completion_tokens\":9999999999999}}",
		"{\"usage\":{\"completion_tokens\":-5}}",
		"{\"choices\":[" + strings.Repeat(`{"delta":{"content":"x"}},`, 40) + `{"delta":{"content":"x"}}]}`,
		`{"choices":[{"message":{"content":"ok"}}],"error":null}`,
		"{",
		"not json",
		"",
		"\n\n\n",
		"{\"choices\":[{\"message\":{\"content\":\"" + strings.Repeat("\xf0\x9f\x8c\x8d", 40) + "\"}}]}",
		`{"choices":[{"message":{"content":42}}]}`,
		`{"choices":[{"message":{"content":null,"refusal":"declined"}}]}`,
		`{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":128}}`,
		`{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":129}}`,
		`{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":-1}}`,
		`{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":1.5}}`,
		`{"choices":[{"message":{"content":"ok"}}],"error":[{"message":"busy"}]}`,
		"{\"choices\":[{\"message\":{\"content\":\"\xff\x00\"}}]}",
		strings.Repeat(" ", probeLineMax-2) + `{}`,
		strings.Repeat(" ", probeLineMax-1) + `{}`,
		`{"choices":[{"message":{"content":"ok"}}]}` + strings.Repeat(" ", probeLineMax),
		strings.Repeat("[", 64) + `null` + strings.Repeat("]", 64),
		"[[[[[[[\"deep\"]]]]]]]",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		sample := core.ProbeSample{At: time.Now()}
		before := sample
		reader := bytes.NewReader(body)
		tokens, ttft, err := readOpenAIJSON(reader, &sample)
		if len(body)-reader.Len() > probeLineMax+1 {
			t.Fatal("decoder exceeded the response byte limit and overflow check")
		}
		if len(body) > probeLineMax && err == nil {
			t.Fatal("decoder accepted an oversized response")
		}
		if sample != before {
			t.Fatal("decoder modified the input sample")
		}
		if err == nil {
			if tokens <= 0 || tokens > probeTokenTrust || ttft < 0 {
				t.Fatalf("invalid successful result: tokens=%d ttft=%v", tokens, ttft)
			}
		} else if tokens != 0 || ttft != 0 {
			t.Fatalf("failed decode returned a measurement: tokens=%d ttft=%v", tokens, ttft)
		}
		fragmented := iotest.OneByteReader(bytes.NewReader(body))
		tokens2, _, err2 := readOpenAIJSON(fragmented, &sample)
		if tokens2 != tokens || fmt.Sprint(err2) != fmt.Sprint(err) {
			t.Fatalf("fragmentation changed result: %d/%v then %d/%v", tokens, err, tokens2, err2)
		}
	})
}

func TestFitEvalDuration(t *testing.T) {
	const measured = 3 * time.Second
	tests := []struct {
		name     string
		reported time.Duration
		want     time.Duration
	}{
		{"nanoseconds fit", 2 * time.Second, 2 * time.Second},
		{"microseconds rescaled", 2000000, 2 * time.Second}, // 2e6 us = 2 s
		{"milliseconds rescaled", 2000, 2 * time.Second},    // 2000 ms = 2 s
		// A fast local engine can report longer than the HTTP round trip we
		// measured; the raw reading stands rather than being scaled away.
		{"nothing fits, keep raw", 10 * time.Second, 10 * time.Second},
		// Short of the band in every unit: refused, so the caller falls back
		// to the wall-clock measurement instead of dividing by zero.
		{"too small in every unit", 100 * time.Millisecond, 0},
	}
	for _, tc := range tests {
		if got := fitEvalDuration(tc.reported, measured); got != tc.want {
			t.Errorf("%s: fitEvalDuration(%v) = %v, want %v", tc.name, tc.reported, got, tc.want)
		}
	}
}

// A gateway reporting eval_duration in microseconds must not read 1000x slow.
func TestRunOllamaEvalDurationMicroseconds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		time.Sleep(200 * time.Millisecond)
		w.Write([]byte(
			`{"response":"one","done":false}` + "\n" +
				`{"response":"","done":true,"eval_count":6,"eval_duration":200000}` + "\n"))
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindOllama, Base: srv.URL, Model: "m"})
	if !s.OK {
		t.Fatalf("probe failed: %+v", s)
	}
	// 200000 as nanoseconds is 200us and would read ~30000 tok/s; read as
	// microseconds it is 200ms, and 6 tokens over 200ms is 30 tok/s exactly.
	// The rate comes from the reported duration, not the wall clock, so the
	// band is tight: a ceiling alone would wave through a millisecond reading
	// and a division by the whole exchange.
	if s.TokPS < 25 || s.TokPS > 35 {
		t.Errorf("tokps = %v, want ~30 from the microsecond reading, not %v", s.TokPS, 6.0/0.2)
	}
}

// The Ollama terminal frame carries done, eval_count, eval_duration and a
// final content delta. When that frame is also the one that trips the
// content budget, the budget break used to fire first and discard the
// engine's own numbers, leaving the sample with frame-counted tokens and
// no decode duration to derive a rate from.
func TestRunOllamaTerminalFrameKeepsEngineCounts(t *testing.T) {
	// 1 + probeContentBytes worth of content in the final frame: past the cap.
	big := strings.Repeat("x", probeContentBytes)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"response":"a","done":false}`+"\n")
		fmt.Fprintf(w, `{"response":%q,"done":true,"eval_count":9,"eval_duration":900000000}`+"\n", big)
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindOllama, Base: srv.URL, Model: "m"})
	if !s.OK {
		t.Fatalf("probe failed: %+v", s)
	}
	if s.Tokens != 9 {
		t.Errorf("tokens = %d, want the engine's eval_count 9", s.Tokens)
	}
	if s.TokPS <= 0 {
		t.Errorf("tok/s = %v, want a rate from the engine's eval_duration", s.TokPS)
	}
}

// An eval_duration no unit scaling can place in the band is refused, and the
// rate then comes from the measured round trip. Dividing the token count by
// that zero stored +Inf as the sample's tok/s.
func TestRunOllamaRefusedEvalDurationUsesWallClock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, _ := w.(http.Flusher)
		// The first token goes out before the sleep, so the measured decode
		// window is the sleep and not the whole exchange.
		_, _ = w.Write([]byte(`{"response":"one","done":false}` + "\n"))
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{"response":"","done":true,"eval_count":6,"eval_duration":1000000}` + "\n"))
	}))
	defer srv.Close()

	start := time.Now()
	s := Run(context.Background(), Request{Kind: core.KindOllama, Base: srv.URL, Model: "m"})
	elapsed := time.Since(start)
	if !s.OK {
		t.Fatalf("probe failed: %+v", s)
	}
	if math.IsInf(s.TokPS, 0) || math.IsNaN(s.TokPS) {
		t.Fatalf("tokps = %v, want the wall-clock rate", s.TokPS)
	}
	// 6 tokens over the measured decode window, not the 1ms the refused
	// reading would have claimed. The window is a subset of the whole call,
	// so a runner that stretches the 200ms sleep stretches the rate with it:
	// the floor scales with the elapsed time this call actually took, and
	// the ceiling is what separates the measured window (200ms, ~30 tok/s)
	// from the refused one. 100 is well under the refused reading's rate and
	// well over the measured one, so a decode window measured from the wrong
	// end of the exchange cannot hide inside the band.
	if lo := max(float64(6)/elapsed.Seconds()/2, 20); s.TokPS < lo || s.TokPS > 100 {
		t.Errorf("tokps = %v over a %v call, want a rate from the measured round trip", s.TokPS, elapsed)
	}
}

// An eval_duration past the int64 nanosecond range, and a negative one, are
// not durations. Scaling them by time.Nanosecond wraps into a small positive
// value that fitEvalDuration then places in the band, so the sample reported a
// rate from a number the engine never sent.
func TestRunOllamaOutOfRangeEvalDurationUsesWallClock(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"overflows nanoseconds", "9223372036854775807"},
		{"negative", "-1000000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				flusher, _ := w.(http.Flusher)
				_, _ = w.Write([]byte(`{"response":"one","done":false}` + "\n"))
				if flusher != nil {
					flusher.Flush()
				}
				time.Sleep(200 * time.Millisecond)
				_, _ = w.Write([]byte(`{"response":"","done":true,"eval_count":6,"eval_duration":` + tc.raw + "}\n"))
			}))
			defer srv.Close()

			start := time.Now()
			s := Run(context.Background(), Request{Kind: core.KindOllama, Base: srv.URL, Model: "m"})
			elapsed := time.Since(start)
			if !s.OK {
				t.Fatalf("probe failed: %+v", s)
			}
			// Same bound as the refused-reading case: the rate has to come
			// from the measured decode window, not from the wrapped value.
			if lo := max(float64(6)/elapsed.Seconds()/2, 20); s.TokPS < lo || s.TokPS > 100 {
				t.Errorf("tokps = %v over a %v call, want a rate from the measured round trip", s.TokPS, elapsed)
			}
		})
	}
}

// A thinking model that answers in one piece carries its trace in
// message.reasoning_content and can leave content empty. A non-stream parse
// that read content alone called a working engine an empty stream, which
// reads as a broken backend and latches a "probe failed" audit line.
func TestRunOpenAINonStreamReasoningOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"reasoning_content", `{"choices":[{"message":{"reasoning_content":"one two three"}}],"usage":{"completion_tokens":3}}`},
		{"reasoning", `{"choices":[{"message":{"reasoning":"one two three"}}],"usage":{"completion_tokens":3}}`},
		{"delta reasoning", `{"choices":[{"delta":{"reasoning_content":"one two three"}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			s := Run(context.Background(), Request{Kind: core.KindOpenAI, Base: srv.URL, Model: "m"})
			if !s.OK {
				t.Fatalf("reasoning-only completion = %+v, want a timed sample", s)
			}
			if s.Tokens == 0 {
				t.Fatalf("reasoning-only completion reported %d tokens, want the generated frames", s.Tokens)
			}
		})
	}
}

// An engine's own error text reaches the frame and both reports, which are
// meant to be pasteable, and a model or config path is usually under the
// operator's home. The sample folds it there, once, so every renderer
// inherits the fold instead of remembering to apply it.
func TestRunFoldsHomeOutOfEngineError(t *testing.T) {
	home := filepath.Join(t.TempDir(), "private-user")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("home", home)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintf(w, `{"error":"model not found: %s/models/m.gguf"}`+"\n", home)
	}))
	defer srv.Close()

	s := Run(context.Background(), Request{Kind: core.KindOllama, Base: srv.URL, Model: "m"})
	if s.OK || s.Err == "" {
		t.Fatalf("engine error = %+v, want a failed sample carrying the message", s)
	}
	if strings.Contains(s.Err, "private-user") || strings.Contains(s.Err, home) {
		t.Errorf("probe error carries the home directory: %q", s.Err)
	}
	if !strings.Contains(s.Err, filepath.Join("~", "models", "m.gguf")) {
		t.Errorf("probe error = %q, want the model path with the home folded to ~", s.Err)
	}
}
