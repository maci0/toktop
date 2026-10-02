package probe

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// streamBenchSample is a probe sample with a start instant already set, so
// the benchmark's ttft measurement costs what a real probe's costs.
func streamBenchSample() *core.ProbeSample {
	return &core.ProbeSample{At: time.Now()}
}

func newBenchScanner(body string) *bufio.Scanner {
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, probeBufInit), probeLineMax)
	return sc
}

// BenchmarkOpenAIStreamFrames measures the per-frame cost of parsing an
// OpenAI-compatible SSE completion: one frame per generated token, up to
// probeFrameMax of them per probe.
func BenchmarkOpenAIStreamFrames(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < 256; i++ {
		fmt.Fprintf(&sb, "data: {\"choices\":[{\"delta\":{\"content\":\"tok%d \"}}]}\n\n", i)
	}
	sb.WriteString("data: [DONE]\n\n")
	body := sb.String()

	b.ReportAllocs()
	for b.Loop() {
		n, _, err := readOpenAIStream(context.Background(), streamBenchSample(), newBenchScanner(body))
		if err != nil {
			b.Fatalf("read stream: %v", err)
		}
		if n != 256 {
			b.Fatalf("tokens = %d, want 256", n)
		}
	}
}

// BenchmarkOpenAIStreamFramesReasoning measures the same loop on frames
// carrying a reasoning run alongside the content, the shape a reasoning model
// streams.
func BenchmarkOpenAIStreamFramesReasoning(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&sb, "data: {\"choices\":[{\"delta\":{\"content\":\"tok%d\",\"reasoning_content\":\"a longer reasoning run \"}}]}\n\n", i)
	}
	sb.WriteString("data: {\"choices\":[],\"usage\":{\"completion_tokens\":200}}\n\n")
	sb.WriteString("data: [DONE]\n\n")
	body := sb.String()

	b.ReportAllocs()
	for b.Loop() {
		n, _, err := readOpenAIStream(context.Background(), streamBenchSample(), newBenchScanner(body))
		if err != nil {
			b.Fatalf("read stream: %v", err)
		}
		if n != 200 {
			b.Fatalf("tokens = %d, want 200", n)
		}
	}
}