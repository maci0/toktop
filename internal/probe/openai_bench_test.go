package probe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// benchOpenAIStream serves body as one SSE stream and returns the URL of the
// engine answering it. The benchmark drives probeOpenAI rather than a
// frame-reading helper because the streaming loop is inline in probeOpenAI:
// a benchmark that called its own copy of the loop would measure that copy.
func benchOpenAIStream(b *testing.B, body string) string {
	b.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := http.NewResponseController(w)
		for line := range strings.SplitSeq(body, "\n\n") {
			if line == "" {
				continue
			}
			fmt.Fprint(w, line+"\n\n")
			f.Flush()
		}
	}))
	b.Cleanup(srv.Close)
	return srv.URL
}

// openAISample is a probe sample with a start instant already set, so the
// benchmark's ttft measurement costs what a real probe's costs.
func openAISample() *core.ProbeSample {
	return &core.ProbeSample{At: time.Now()}
}

// BenchmarkOpenAIStreamFrames measures the per-frame cost of parsing an
// OpenAI-compatible SSE completion: one frame per generated token, up to the
// probeTokens budget the loop stops reading at.
func BenchmarkOpenAIStreamFrames(b *testing.B) {
	var sb strings.Builder
	for i := range probeTokens {
		fmt.Fprintf(&sb, "data: {\"choices\":[{\"delta\":{\"content\":\"tok%d \"}}]}\n\n", i)
	}
	sb.WriteString("data: [DONE]\n\n")
	url := benchOpenAIStream(b, sb.String())

	b.ReportAllocs()
	for b.Loop() {
		n, _, err := probeOpenAI(context.Background(), Request{Base: url, Model: "m"}, openAISample())
		if err != nil {
			b.Fatalf("probe stream: %v", err)
		}
		if n != probeTokens {
			b.Fatalf("tokens = %d, want %d", n, probeTokens)
		}
	}
}

// BenchmarkOpenAIStreamFramesReasoning measures the same loop on frames
// carrying a reasoning run alongside the content, the shape a reasoning model
// streams.
func BenchmarkOpenAIStreamFramesReasoning(b *testing.B) {
	// The budget the loop stops reading at counts content and reasoning
	// together (overBudget takes tokens+reasoning), so a frame carrying both
	// spends two units of it and half as many frames fill the budget. The
	// stream is sized to that so the benchmark measures a whole generation
	// rather than the overrun it would otherwise stop at.
	const frames = probeTokens / 2
	var sb strings.Builder
	for i := range frames {
		fmt.Fprintf(&sb, "data: {\"choices\":[{\"delta\":{\"content\":\"tok%d\",\"reasoning_content\":\"a longer reasoning run \"}}]}\n\n", i)
	}
	fmt.Fprintf(&sb, "data: {\"choices\":[],\"usage\":{\"completion_tokens\":%d}}\n\n", frames)
	sb.WriteString("data: [DONE]\n\n")
	url := benchOpenAIStream(b, sb.String())

	b.ReportAllocs()
	for b.Loop() {
		n, _, err := probeOpenAI(context.Background(), Request{Base: url, Model: "m"}, openAISample())
		if err != nil {
			b.Fatalf("probe stream: %v", err)
		}
		// usage wins over the delta count for a stream this short.
		if n != frames {
			b.Fatalf("tokens = %d, want %d", n, frames)
		}
	}
}
