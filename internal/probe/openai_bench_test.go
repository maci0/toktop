package probe

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maci0/toktop/internal/core"
)

// benchStreamServer serves body as an SSE stream, which is the only way to
// reach the per-frame parsing loop: it lives inside probeOpenAI, which reads
// the response of a real POST. The benchmark therefore measures the loop with
// a loopback round trip under it, which is constant across iterations and far
// below the per-frame cost being measured.
func benchStreamServer(b *testing.B, body string) string {
	b.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	b.Cleanup(srv.Close)
	return srv.URL
}

// benchStreamTokens runs one probe against a server streaming body and
// reports the token count the stream reader derived.
func benchStreamTokens(b *testing.B, url string, want int) {
	b.Helper()
	s := Run(b.Context(), Request{Kind: core.KindVLLM, Base: url, Model: "bench"})
	if !s.OK {
		b.Fatalf("probe: %s", s.Err)
	}
	if s.Tokens != want {
		b.Fatalf("tokens = %d, want %d", s.Tokens, want)
	}
}

// benchFrames is how many content frames each streamed body carries. It is
// probeTokens (32), the budget at which the reader hangs up on an engine that
// ignores max_tokens: a body longer than that stops being parsed early, so the
// benchmark would measure the hang-up rather than the per-frame cost. The
// per-frame payloads here are sized so the whole stream also stays under
// probeContentBytes (probeTokens*32), the other budget that ends a generation
// early.
const benchFrames = 32

// benchTokens is what the probe reports for a benchFrames stream: one token
// per content frame, with no engine-reported usage to override it.
const benchTokens = benchFrames

// benchReasoning is the reasoning payload each frame of the reasoning stream
// carries. Content and reasoning bytes share probeContentBytes, so a payload
// this size is what leaves room for all benchFrames frames rather than
// hanging up partway and benchmarking the hang-up.
const benchReasoning = "t"

// benchReasoningFrames is how many frames the reasoning stream carries. A
// reasoning frame counts as two against probeTokens (one for the content, one
// for the reasoning), so a stream of this length reaches the same budget at
// half the frames and the reader parses every one of them.
const benchReasoningFrames = benchTokens / 2

// BenchmarkOpenAIStreamFrames measures the per-frame cost of parsing an
// OpenAI-compatible SSE completion: one frame per generated token, up to
// probeFrameMax of them per probe.
func BenchmarkOpenAIStreamFrames(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < benchFrames; i++ {
		fmt.Fprintf(&sb, "data: {\"choices\":[{\"delta\":{\"content\":\"%d \"}}]}\n\n", i%10)
	}
	sb.WriteString("data: [DONE]\n\n")
	url := benchStreamServer(b, sb.String())

	b.ReportAllocs()
	for b.Loop() {
		benchStreamTokens(b, url, benchTokens)
	}
}

// BenchmarkOpenAIStreamFramesReasoning measures the same loop on frames
// carrying a reasoning run alongside the content, the shape a reasoning model
// streams.
func BenchmarkOpenAIStreamFramesReasoning(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < benchReasoningFrames; i++ {
		fmt.Fprintf(&sb, "data: {\"choices\":[{\"delta\":{\"content\":\"%d \",\"reasoning_content\":%q}}]}\n\n", i%10, benchReasoning)
	}
	sb.WriteString("data: [DONE]\n\n")
	url := benchStreamServer(b, sb.String())

	b.ReportAllocs()
	for b.Loop() {
		benchStreamTokens(b, url, benchReasoningFrames)
	}
}
