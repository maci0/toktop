package probe

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rivo/uniseg"

	"github.com/maci0/toktop/internal/core"
)

// fuzzBodyCap bounds what a single iteration serves. Bodies past
// probeStreamMax exercise the reader cap, so anything well beyond it only
// costs copy time.
const fuzzBodyCap = 1 << 20

// FuzzProbeResponse drives both probe readers (Ollama NDJSON and OpenAI
// SSE/JSON) with an arbitrary engine body. Discovery probes every candidate
// localhost port, so whatever answers there is untrusted: a hostile or merely
// broken engine must not panic the collector, hang the reader, or report a
// measurement the boundary guarantees forbid. The engine-reported token
// counts are summed straight into displayed tok/s, so a body that decodes
// must still produce a bounded count, a finite rate, a model id inside the
// cap, and the same count on a second identical probe.
func FuzzProbeResponse(f *testing.F) {
	for _, seed := range []struct {
		body string
		ct   string
	}{
		{`{"response":"one","done":false}` + "\n" + `{"response":"two","done":true,"eval_count":9,"eval_duration":3000000000}`, "application/x-ndjson"},
		{"data: {\"choices\":[{\"delta\":{\"content\":\"He\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\ndata: [DONE]\n", "text/event-stream"},
		{`{"choices":[{"message":{"content":"hi"}}],"usage":{"completion_tokens":42}}`, "application/json"},
		{`{"error":"model \"x\" not found"}`, "application/x-ndjson"},
		{`data: {"error":{"message":"context length exceeded"}}` + "\n\n", "text/event-stream"},
		{`{"choices":[{"delta":{"reasoning":"thinking hard"}}]}`, "text/event-stream"},
		{`{"response":"x","done":true,"eval_count":999999999,"eval_duration":1}`, "application/x-ndjson"},
		{`{"choices":[{"delta":{"content":"x"}}],"usage":{"completion_tokens":-5}}`, "application/json"},
		{"data: [DONE]\n", "text/event-stream"},
		{"not json at all\n", "application/x-ndjson"},
		{"{\"choices\":null}", "application/json"},
		{"", "text/event-stream"},
		{"", "text/plain"},
		{"data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}", "text/plain"},
		{strings.Repeat("{\"response\":\"aaaa\"}\n", 5000), "application/x-ndjson"},
		{strings.Repeat("x", 1<<17), "text/event-stream"},
		{"data: {\"choices\":[{\"delta\":{\"content\":\"é\"}}]}\n", "text/event-stream; charset=utf-8"},
		{"{\"response\":\"\xff\xfe\"}", "application/x-ndjson"},
	} {
		f.Add([]byte(seed.body), seed.ct)
	}
	f.Fuzz(func(t *testing.T, body []byte, ct string) {
		if len(body) > fuzzBodyCap {
			t.Skip()
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			w.WriteHeader(http.StatusOK)
			w.Write(body)
		}))
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, kind := range []string{core.KindOllama, core.KindVLLM} {
			run := func() core.ProbeSample {
				return Run(ctx, Request{Kind: kind, Base: srv.URL, Model: "fuzz-model"})
			}
			s := run()
			if n := uniseg.GraphemeClusterCount(s.Model); n > ModelNameMax {
				t.Fatalf("%s: model id = %d characters, cap %d", kind, n, ModelNameMax)
			}
			switch {
			case s.OK:
				if s.Err != "" {
					t.Fatalf("%s: OK sample carries Err %q", kind, s.Err)
				}
				if s.Tokens <= 0 || s.Tokens > probeTokenTrust {
					t.Fatalf("%s: tokens = %d, want 1..%d", kind, s.Tokens, probeTokenTrust)
				}
				if !(s.TTFTms >= 0) || math.IsInf(s.TTFTms, 0) || math.IsNaN(s.TTFTms) {
					t.Fatalf("%s: ttft = %v", kind, s.TTFTms)
				}
				if !(s.TokPS >= 0) || math.IsInf(s.TokPS, 0) || math.IsNaN(s.TokPS) {
					t.Fatalf("%s: tokps = %v", kind, s.TokPS)
				}
			default:
				if s.Err == "" {
					t.Fatalf("%s: failed probe with no error", kind)
				}
				if s.Tokens != 0 {
					t.Fatalf("%s: failed probe kept %d tokens", kind, s.Tokens)
				}
			}
			if s.RetryAfter != 0 && (s.RetryAfter < retryAfterDefault || s.RetryAfter > retryAfterMax) {
				t.Fatalf("%s: RetryAfter = %v outside %v..%v", kind, s.RetryAfter, retryAfterDefault, retryAfterMax)
			}
			// Content counting is a pure function of the body; the clock is
			// the only thing allowed to differ between two identical probes.
			if again := run(); again.Tokens != s.Tokens || again.OK != s.OK {
				t.Fatalf("%s: not deterministic: %+v then %+v", kind, s, again)
			}
		}
	})
}
