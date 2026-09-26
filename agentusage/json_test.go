// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// The envelopes below follow the three API dialects the supported agents emit:
// Anthropic-shaped (claude, cursor-agent, kimi), Gemini-shaped (gemini, qwen),
// and OpenAI-shaped. The parser is deliberately envelope-agnostic, so these
// double as a statement of what it must survive.

func TestAnthropicShapedLine(t *testing.T) {
	line := `{"type":"assistant","message":{"role":"assistant","content":[
		{"type":"text","text":"Fixed the leak in pool.go"}],
		"usage":{"input_tokens":900,"output_tokens":120,
		"output_tokens_details":{"thinking_tokens":40}}}}`
	ev, ok := parseJSON([]byte(line))
	if !ok {
		t.Fatal("valid JSON was rejected")
	}
	if ev.Usage.Output != 120 || ev.Usage.Thinking != 40 || ev.Usage.Input != 900 {
		t.Fatalf("usage: %+v", ev.Usage)
	}
}

func TestGeminiShapedLine(t *testing.T) {
	line := `{"type":"assistant","content":{"parts":[{"text":"done"}]},
		"usageMetadata":{"promptTokenCount":50,"candidatesTokenCount":17,
		"thoughtsTokenCount":9,"totalTokenCount":76}}`
	ev, ok := parseJSON([]byte(line))
	if !ok {
		t.Fatal("valid JSON was rejected")
	}
	if ev.Usage.Output != 17 || ev.Usage.Thinking != 9 || ev.Usage.Total != 76 {
		t.Fatalf("usage: %+v", ev.Usage)
	}
}

func TestOpenAIShapedLine(t *testing.T) {
	line := `{"choices":[{"delta":{"content":"partial output"}}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	ev, ok := parseJSON([]byte(line))
	if !ok {
		t.Fatal("valid JSON was rejected")
	}
	if ev.Usage.Output != 5 || ev.Usage.Total != 15 {
		t.Fatalf("usage: %+v", ev.Usage)
	}
}

func TestPlainTextIsNotJSON(t *testing.T) {
	for _, line := range []string{
		"Reading src/main.go",
		"",
		"[1, 2, 3]",   // an array is not an event envelope
		`{"broken": `, // truncated
		"⏺ Bash(go test ./...)",
	} {
		if _, ok := parseJSON([]byte(line)); ok {
			t.Errorf("%q should not parse as an event", line)
		}
	}
}

func TestUnknownEnvelopeInventsNoNumbers(t *testing.T) {
	// A shape nobody anticipated must not invent numbers.
	line := `{"kind":"progress","phase":"indexing","files":420}`
	ev, ok := parseJSON([]byte(line))
	if !ok {
		t.Fatal("valid JSON was rejected")
	}
	if ev.Usage.Has() {
		t.Fatalf("invented usage: %+v", ev.Usage)
	}
}

func TestAbsurdCounterIsAbsent(t *testing.T) {
	line := `{"output_tokens":1099511627777}` // maxSaneTokens + 1
	ev, ok := parseJSON([]byte(line))
	if !ok {
		t.Fatal("valid JSON was rejected")
	}
	if ev.Usage.Has() {
		t.Fatalf("invented usage from an absurd counter: %+v", ev.Usage)
	}
}

func TestWalkDoesNotInspectPayloads(t *testing.T) {
	// Counters planted inside content/parts/delta are prompt or tool text,
	// not usage. The outer usage object is what counts.
	line := `{"content":{"usage":{"output_tokens":99999},"parts":[{"usage":{"output_tokens":888}}]},` +
		`"delta":{"usage":{"output_tokens":777}},"usage":{"output_tokens":7}}`
	ev, ok := parseJSON([]byte(line))
	if !ok {
		t.Fatal("valid JSON was rejected")
	}
	if ev.Usage.Output != 7 {
		t.Fatalf("payload usage leaked or outer usage lost: %+v", ev.Usage)
	}
}

func TestWalkDoesNotInspectPromptTrees(t *testing.T) {
	// prompt/messages/choices/system/text are user or model text. A cwd or
	// counter planted there is not the session's. Top-level cwd and usage
	// still count; a session header that names cwd under payload still does.
	line := `{"prompt":{"cwd":"/home/alice/secret","usage":{"output_tokens":99999}},` +
		`"messages":[{"cwd":"/Users/alice/mail","usage":{"output_tokens":888}}],` +
		`"choices":[{"usage":{"output_tokens":777}}],` +
		`"system":{"cwd":"/tmp/x","usage":{"output_tokens":666}},` +
		`"text":{"cwd":"/etc","usage":{"output_tokens":555}},` +
		`"cwd":"/tmp/proj","usage":{"output_tokens":7}}`
	ev, ok := parseJSON([]byte(line))
	if !ok {
		t.Fatal("valid JSON was rejected")
	}
	if ev.Usage.Output != 7 {
		t.Fatalf("prompt-tree usage leaked or outer usage lost: %+v", ev.Usage)
	}
	if ev.Cwd != "/tmp/proj" {
		t.Fatalf("cwd = %q, want /tmp/proj (prompt-tree cwd must not win)", ev.Cwd)
	}

	header := `{"type":"session_meta","payload":{"id":"x","cwd":"/home/dev/project"}}`
	ev, ok = parseJSON([]byte(header))
	if !ok {
		t.Fatal("session header was rejected")
	}
	if ev.Cwd != "/home/dev/project" {
		t.Fatalf("payload cwd = %q, want /home/dev/project", ev.Cwd)
	}
}

func TestSatAddSaturates(t *testing.T) {
	if got := satAdd(3, 4); got != 7 {
		t.Fatalf("satAdd(3, 4) = %d", got)
	}
	if got := satAdd(math.MaxInt-1, 2); got != math.MaxInt {
		t.Fatalf("satAdd overflow = %d, want MaxInt", got)
	}
}

func TestCounter64RejectsOverflow(t *testing.T) {
	if got := counter64(math.MaxInt64); got != 0 {
		t.Fatalf("counter64(MaxInt64) = %d, want 0", got)
	}
	if got := counter64(-1); got != 0 {
		t.Fatalf("counter64(-1) = %d, want 0", got)
	}
	if got := clampSane(3_000_000_000); got != 3_000_000_000 {
		t.Fatalf("clampSane(3e9) = %d, want 3e9", got)
	}
	if got := clampSane(math.MaxInt64); got != 0 {
		t.Fatalf("clampSane(MaxInt64) = %d, want 0", got)
	}
}

func TestDeeplyNestedPayloadDoesNotRunAway(t *testing.T) {
	// A tool result can nest arbitrarily; the walk must stop and stay quiet.
	line := `{"type":"tool_result","content":` + strings.Repeat(`{"a":`, 40) + `"deep"` + strings.Repeat(`}`, 40) + `}`
	ev, ok := parseJSON([]byte(line))
	if !ok {
		t.Fatal("valid JSON was rejected")
	}
	if ev.Usage.Has() || ev.Cwd != "" {
		t.Fatal("walked past the depth limit")
	}
}

func TestResultLineCarriesFinalUsage(t *testing.T) {
	line := `{"type":"result","subtype":"success","total_cost_usd":0.03,
		"usage":{"input_tokens":1000,"output_tokens":250,"total_tokens":1250}}`
	ev, ok := parseJSON([]byte(line))
	if !ok {
		t.Fatal("valid JSON was rejected")
	}
	if ev.Usage.Output != 250 || ev.Usage.Total != 1250 {
		t.Fatalf("usage: %+v", ev.Usage)
	}
}

// The two shapes below were read out of the shipped binaries themselves
// (`strings` on grok and cursor-agent), so they record what those agents
// actually emit rather than what their docs claim.

func TestGrokStreamEventShape(t *testing.T) {
	line := `{"type":"stream_event","event":{"type":"text_delta","text":"patching pool.go"},
		"usage":{"input_tokens":4096,"output_tokens":88,"reasoning_tokens":31,"cached_tokens":2048}}`
	ev, ok := parseJSON([]byte(line))
	if !ok {
		t.Fatal("valid JSON was rejected")
	}
	if ev.Usage.Output != 88 || ev.Usage.Thinking != 31 || ev.Usage.Input != 4096 {
		t.Fatalf("usage: %+v", ev.Usage)
	}
}

func TestCursorAgentResultShape(t *testing.T) {
	line := `{"type":"result","subtype":"success","is_error":false,
		"usage":{"input_tokens":9000,"output_tokens":410}}`
	ev, ok := parseJSON([]byte(line))
	if !ok {
		t.Fatal("valid JSON was rejected")
	}
	if ev.Usage.Output != 410 || ev.Usage.Input != 9000 {
		t.Fatalf("usage: %+v", ev.Usage)
	}
}

func TestDshSessionRecordsCarryUsageAndCwd(t *testing.T) {
	// dsh writes a session header naming the directory. Completed
	// assistant/message records carry the provider's usage; the generic
	// walker still sees those keys (the adapter uses parseDsh).
	header := `{"type":"session","version":1,"id":"abc","cwd":"/home/dev/project","createdAt":1}`
	ev, ok := parseJSON([]byte(header))
	if !ok || ev.Cwd != "/home/dev/project" {
		t.Fatalf("header cwd not found: %+v", ev)
	}
	event := `{"type":"assistant-message","usage":{"prompt_tokens":1200,"completion_tokens":260,` +
		`"reasoning_tokens":90,"cached_tokens":800}}`
	ev, ok = parseJSON([]byte(event))
	if !ok {
		t.Fatal("valid JSON was rejected")
	}
	if ev.Usage.Output != 260 || ev.Usage.Thinking != 90 || ev.Usage.Input != 1200 {
		t.Fatalf("usage: %+v", ev.Usage)
	}
}

func TestAbsurdCountersReportNothing(t *testing.T) {
	// A float outside the range of int is not a measurement, and what a
	// conversion does with one differs by platform. It must read as absent.
	for _, line := range []string{
		`{"usage":{"output_tokens":1e30}}`,
		`{"usage":{"input_tokens":-5}}`,
		`{"usage":{"total_tokens":9223372036854775807}}`,
		`{"usage":{"output_tokens":1e15}}`,
	} {
		ev, ok := parseJSON([]byte(line))
		if !ok {
			t.Fatalf("%s should parse as JSON", line)
		}
		if ev.Usage.Has() {
			t.Fatalf("%s invented usage %+v", line, ev.Usage)
		}
	}
	// Sane large counters still count.
	ev, ok := parseJSON([]byte(`{"usage":{"output_tokens":1e6}}`))
	if !ok || ev.Usage.Output != 1000000 {
		t.Fatalf("large counter lost: ok=%v usage=%+v", ok, ev.Usage)
	}
}

func TestFractionalCounterIsAbsent(t *testing.T) {
	// JSON numbers are float64. 1.5 would become 1 through a truncating
	// conversion, under-counting the line. Whole values still count.
	for _, line := range []string{
		`{"usage":{"output_tokens":1.5}}`,
		`{"usage":{"output_tokens":99.9}}`,
		`{"usage":{"input_tokens":2.1}}`,
	} {
		ev, ok := parseJSON([]byte(line))
		if !ok {
			t.Fatalf("%s should parse as JSON", line)
		}
		if ev.Usage.Has() {
			t.Fatalf("%s truncated a fractional counter to %+v", line, ev.Usage)
		}
	}
	ev, ok := parseJSON([]byte(`{"usage":{"output_tokens":120.0}}`))
	if !ok || ev.Usage.Output != 120 {
		t.Fatalf("whole float lost: ok=%v usage=%+v", ok, ev.Usage)
	}
}

func TestParseGenericKeepsInputWhenTotalIsAbsent(t *testing.T) {
	v, _, ok := parseGeneric([]byte(`{"usage":{"input_tokens":900,"output_tokens":120}}`))
	if !ok || v.output != 120 || v.input != 900 {
		t.Fatalf("got ok=%v %+v, want input 900 output 120", ok, v)
	}
	if v.total != 1020 {
		t.Fatalf("total %d, want 1020 (input+output when total_tokens is absent)", v.total)
	}
}

func TestParseGenericDropsAbsurdCounters(t *testing.T) {
	if _, _, ok := parseGeneric([]byte(`{"usage":{"output_tokens":1e15}}`)); ok {
		t.Fatal("1e15 tokens must not count")
	}
	v, _, ok := parseGeneric([]byte(`{"cwd":"/tmp/x","usage":{"output_tokens":42}}`))
	if !ok || v.output != 42 {
		t.Fatalf("sane generic usage lost: ok=%v %+v", ok, v)
	}
}

// BenchmarkParse measures the per-line cost every transcript record pays.
func BenchmarkParseJSON(b *testing.B) {
	line := []byte(`{"type":"assistant","message":{"role":"assistant","content":[
		{"type":"text","text":"Fixed the leak in pool.go by bounding the ring buffer."}],
		"usage":{"input_tokens":900,"output_tokens":120,
		"output_tokens_details":{"thinking_tokens":40}}}}`)
	b.SetBytes(int64(len(line)))
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := parseJSON(line); !ok {
			b.Fatal("valid JSON was rejected")
		}
	}
}

// FuzzParse feeds the parser arbitrary agent output and pins the invariants
// every caller relies on: a rejected line contributes nothing, no counter is
// ever negative or platform-dependent, parsing is deterministic, and anything
// encoding/json accepts as an object is accepted as an event.
func FuzzParseJSON(f *testing.F) {
	seeds := []string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":10,"output_tokens":2}}}`,
		`{"choices":[{"delta":{"content":"partial"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		`{"content":{"parts":[{"text":"done"}]},"usageMetadata":{"candidatesTokenCount":3}}`,
		`{"type":"tool_result","content":` + strings.Repeat(`{"a":`, 40) + `"deep"` + strings.Repeat(`}`, 40) + `}`,
		`{"usage":{"output_tokens":1e30,"reasoning_tokens":0.5}}`,
		`{"text":"\u0000\u001b[32m","cwd":"/tmp/x","type":"t"}`,
		"{\"broken\": ", ``, `[1,2,3]`, `{"a":`, "null", "42",
		`{"type":"x","message":{"message":{"message":{"text":"nested"}}}}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		ev, ok := parseJSON(line)
		if !ok && ev != (jsonEvent{}) {
			t.Fatalf("rejected line contributed content: %q -> %+v", line, ev)
		}
		if ev.Usage.Output < 0 || ev.Usage.Thinking < 0 || ev.Usage.Total < 0 || ev.Usage.Input < 0 {
			t.Fatalf("negative counter: %q -> %+v", line, ev.Usage)
		}
		if ev2, ok2 := parseJSON(line); ok2 != ok || ev2 != ev {
			t.Fatalf("Parse is not deterministic for %q", line)
		}
		if ok {
			return
		}
		trimmed := strings.TrimSpace(string(line))
		if strings.HasPrefix(trimmed, "{") {
			var doc any
			if json.Unmarshal([]byte(trimmed), &doc) == nil {
				t.Fatalf("rejected a JSON object encoding/json accepts: %q", trimmed)
			}
		}
	})
}
