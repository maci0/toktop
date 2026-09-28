// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// The envelopes below follow the dialects the supported agents emit:
// Anthropic-shaped (claude, cursor-agent), Gemini-shaped (gemini, qwen),
// OpenAI-shaped, and Kimi Code CLI's own canonical usage fields. The parser is
// deliberately envelope-agnostic, so these double as a statement of what it
// must survive.

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

// Kimi Code CLI records usage in its own canonical form: the uncached prompt
// under inputOther, the cached shares beside it, and everything generated under
// output. The generic walker reads it by key like any other envelope.
func TestKimiShapedLine(t *testing.T) {
	line := `{"type":"usage.record","agentId":"main","model":"kimi/k2",
		"usage":{"inputOther":900,"output":120,"inputCacheRead":300,"inputCacheCreation":40},
		"usageScope":"turn"}`
	ev, ok := parseJSON([]byte(line))
	if !ok {
		t.Fatal("valid JSON was rejected")
	}
	if ev.Usage.Output != 120 || ev.Usage.Input != 900 {
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
	// The ceiling is the same one counter enforces on a single record, so a
	// running total never reports a magnitude this package would reject.
	if got := satAdd(maxSaneTokens, 4); got != maxSaneTokens {
		t.Fatalf("satAdd past maxSaneTokens = %d, want %d", got, maxSaneTokens)
	}
	if got := satAdd(math.MaxInt-1, 2); got != maxSaneTokens {
		t.Fatalf("satAdd overflow = %d, want %d", got, maxSaneTokens)
	}
}

func TestSatAddSpanSaturates(t *testing.T) {
	if got := satAddSpan(3*time.Second, 4*time.Second); got != 7*time.Second {
		t.Fatalf("satAddSpan(3s, 4s) = %v", got)
	}
	// A single grok record may carry close to the whole int64 nanosecond
	// range, so the pair summed with a plain + wrapped negative and the
	// sample reported a negative span for work it had counted.
	if got := satAddSpan(math.MaxInt64, 4*time.Second); got != maxSaneSpan {
		t.Fatalf("satAddSpan past maxSaneSpan = %v, want %v", got, maxSaneSpan)
	}
	if got := satAddSpan(maxSaneSpan-1, 10*time.Second); got != maxSaneSpan {
		t.Fatalf("satAddSpan crossing the ceiling = %v, want %v", got, maxSaneSpan)
	}
	// A negative or zero operand is dropped, not summed into the result, so a
	// corrupt span cannot subtract model time that was already accumulated.
	if got := satAddSpan(5*time.Second, -time.Hour); got != 5*time.Second {
		t.Fatalf("satAddSpan with a negative operand = %v, want 5s", got)
	}
	if got := satAddSpan(0, 0); got != 0 {
		t.Fatalf("satAddSpan(0, 0) = %v, want 0", got)
	}
}

// A per-message read that folds several records into one must grow the span
// with the tokens. Folding two 4-token, 10s turns into a single record while
// keeping the first record's span divides 8 tokens by 10s instead of 20s.
func TestPerMessageFoldAccumulatesSpan(t *testing.T) {
	w := &Watcher{}
	w.ad = adapter{kind: perMessage}
	recs := w.collectValue(nil, values{output: 4, span: 10 * time.Second}, "")
	recs = w.collectValue(recs, values{output: 4, span: 10 * time.Second}, "")
	if got := recs[0].output; got != 8 {
		t.Fatalf("folded output = %d, want 8", got)
	}
	if got := recs[0].span; got != 20*time.Second {
		t.Fatalf("folded span = %v, want 20s: the tokens of both records are measured over both spans", got)
	}
}

func TestCounter64RejectsOverflow(t *testing.T) {
	if got := counter64(math.MaxInt64); got != 0 {
		t.Fatalf("counter64(MaxInt64) = %d, want 0", got)
	}
	if got := counter64(-1); got != 0 {
		t.Fatalf("counter64(-1) = %d, want 0", got)
	}
	if got := counter(3_000_000_000); got != 3_000_000_000 {
		t.Fatalf("counter(3e9) = %d, want 3e9", got)
	}
	if got := counter(math.MaxInt64); got != 0 {
		t.Fatalf("counter(MaxInt64) = %d, want 0", got)
	}
}

// A tool result can nest arbitrarily; the walk must stop and stay quiet. The
// nesting sits under a non-payload key on purpose: walk skips payload-keyed
// subtrees without descending, so nesting under "content" would never reach
// the depth guard and the cap would go untested.
func TestDeeplyNestedRecordDoesNotRunAway(t *testing.T) {
	line := `{"type":"tool_result","node":` + strings.Repeat(`{"a":`, 40) + `"deep"` + strings.Repeat(`}`, 40) + `}`
	ev, ok := parseJSON([]byte(line))
	if !ok {
		t.Fatal("valid JSON was rejected")
	}
	if ev.Usage.Has() || ev.Cwd != "" {
		t.Fatal("walked past the depth limit")
	}
}

// The guard bounds the descent; it does not blind the walk to everything below
// it. Both sides of the boundary are pinned: the deepest wrapper still read
// contributes its counters, and the next one down contributes nothing.
func TestNestingBoundaryAtMaxDepth(t *testing.T) {
	nested := func(deep int) string {
		return `{"type":"tool_result","node":` + strings.Repeat(`{"a":`, deep) +
			`{"usage":{"input_tokens":11,"output_tokens":22}` + strings.Repeat(`}`, deep+1) + `}`
	}
	for _, tc := range []struct {
		name string
		deep int
		want bool
	}{
		{"deepest level still read", maxDepth - 2, true},
		{"one level past the cap", maxDepth - 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := parseJSON([]byte(nested(tc.deep)))
			if !ok {
				t.Fatal("valid JSON was rejected")
			}
			if ev.Usage.Has() != tc.want {
				t.Fatalf("%d wrappers: usage collected = %v, want %v", tc.deep, ev.Usage.Has(), tc.want)
			}
		})
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

func TestParseGenericFoldsTheCachedPromptShares(t *testing.T) {
	// The three Anthropic shares are one billable prompt, so the generic
	// walker adds them. Reading them as three competing values reported the
	// largest alone, and a line carrying only the cache shares reported no
	// prompt at all, where the bespoke claude parser read the whole bill.
	v, _, ok := parseGeneric([]byte(`{"usage":{"input_tokens":900,"cache_read_input_tokens":50000,` +
		`"cache_creation_input_tokens":2000,"output_tokens":120}}`))
	if !ok {
		t.Fatal("a line carrying the three prompt shares was read as no usage")
	}
	if want := 52900; v.input != want {
		t.Fatalf("input %d, want %d (900 + 50000 + 2000)", v.input, want)
	}
	if v.total != 53020 {
		t.Fatalf("total %d, want 53020", v.total)
	}
	if _, _, ok := parseGeneric([]byte(`{"usage":{"cache_creation_input_tokens":7}}`)); !ok {
		t.Fatal("a prompt carried entirely by the cache-write share read as no usage")
	}
	kv, _, kok := parseKimi([]byte(`{"type":"usage.record","usage":{"inputOther":100,"inputCacheRead":5000,"inputCacheCreation":200,"output":300}}`))
	gv, _, gok := parseGeneric([]byte(`{"type":"usage.record","usage":{"inputOther":100,"inputCacheRead":5000,"inputCacheCreation":200,"output":300}}`))
	if !kok || !gok || kv.input != gv.input || kv.total != gv.total {
		t.Fatalf("generic %+v (ok=%v) differs from the kimi adapter %+v (ok=%v)", gv, gok, kv, kok)
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

// Two working-directory keys on one line must not pick a winner at random.
// Map ranges are randomized, so without a sorted walk the same unchanged
// record could be attributed to a different directory on each poll, and
// ownership is judged per record.
func TestParseCwdWithTwoKeysIsDeterministic(t *testing.T) {
	line := []byte(`{"cwd":"/home/one","working_directory":"/home/two",` +
		`"usage":{"output_tokens":5}}`)
	first := ""
	for i := 0; i < 50; i++ {
		ev, ok := parseJSON(line)
		if !ok {
			t.Fatal("line stopped parsing")
		}
		if first == "" {
			first = ev.Cwd
			continue
		}
		if ev.Cwd != first {
			t.Fatalf("cwd flipped between polls: %q then %q", first, ev.Cwd)
		}
	}
	if first == "" {
		t.Fatal("no cwd taken from a line carrying two")
	}
}

func TestKeyFoldIsASCIIOnly(t *testing.T) {
	// Runes whose Unicode lowercase form is ASCII must not satisfy an ASCII
	// key: U+0130 folds to "i" and U+212A to "k" under strings.ToLower, so
	// a key spelled in either would forge a counter the agent never wrote.
	line := `{"usage":{"İnput_tokens":1000,"Kelvin_output_tokens":2000,"input_tokens":3}}`
	ev, ok := parseJSON([]byte(line))
	if !ok {
		t.Fatal("valid JSON was rejected")
	}
	if ev.Usage.Input != 3 || ev.Usage.Output != 0 {
		t.Fatalf("lookalike key was read as a counter: %+v", ev.Usage)
	}
}

func TestCwdChoiceIsDeterministic(t *testing.T) {
	// A record naming two directories resolves to the same one every time.
	// Go map order is randomized per range, so an unsorted walk made
	// ownership a coin flip that transcript.go then cached for the session.
	line := `{"context":{"cwd":"/home/u/other"},"cwd":"/home/u/proj"}`
	first, ok := parseJSON([]byte(line))
	if !ok {
		t.Fatal("valid JSON was rejected")
	}
	for i := range 50 {
		ev, ok := parseJSON([]byte(line))
		if !ok || ev != first {
			t.Fatalf("run %d disagreed: %q vs %q", i, ev.Cwd, first.Cwd)
		}
	}
	if first.Cwd != "/home/u/proj" {
		t.Fatalf("cwd = %q, want /home/u/proj (keys are visited in sorted order)", first.Cwd)
	}
}

func TestSplitASCIISpaceKeepsUnicodeSpaces(t *testing.T) {
	// A path argument may hold U+3000 or U+00A0. strings.Fields splits there
	// and hands agentName a fragment it cannot match.
	got := splitASCIISpace("  /opt/My\u00a0Agents/claude-code   /bin/sh ")
	want := []string{"/opt/My\u00a0Agents/claude-code", "/bin/sh"}
	if len(got) != len(want) {
		t.Fatalf("splitASCIISpace = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitASCIISpace[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if got := splitASCIISpace("   "); len(got) != 0 {
		t.Fatalf("blank input produced %q", got)
	}
}

func TestWalkCoversEverySubtreePastTheInlineScratch(t *testing.T) {
	// walk holds the children it still has to descend into in an array that
	// fits most envelopes without allocating. A record with more non-scalar
	// members than that array holds must still be walked whole, and the
	// working directory must still resolve the way a sorted visit would:
	// the smallest key name at the shallowest level wins.
	var b strings.Builder
	b.WriteString(`{"cwd":"/home/u/proj","zz":{"total_tokens":1}`)
	for i := range 12 {
		fmt.Fprintf(&b, `,"k%02d":{"usage":{"output_tokens":%d}}`, i, i+1)
	}
	b.WriteString(`,"aa":{"cwd":"/home/u/other","output_tokens":7}}`)
	ev, ok := parseJSON([]byte(b.String()))
	if !ok {
		t.Fatal("valid JSON was rejected")
	}
	if ev.Usage.Output != 12 { // k11, the largest of the sibling subtrees
		t.Fatalf("output = %d, want 12 (a member past the inline scratch was skipped)", ev.Usage.Output)
	}
	if ev.Usage.Total != 1 {
		t.Fatalf("total = %d, want 1 (nested under the last key in map order)", ev.Usage.Total)
	}
	if ev.Cwd != "/home/u/proj" {
		t.Fatalf("cwd = %q, want /home/u/proj (this level outranks any subtree)", ev.Cwd)
	}
}

func TestCwdPicksTheSmallestKeyName(t *testing.T) {
	// Two working directories at the same level: the smaller key wins, which
	// is what visiting the level in sorted order decided. Go's map order is
	// randomized per range, so an unguarded first-wins would disagree with
	// itself between polls on the very line transcript.go caches.
	line := []byte(`{"workdir":"/home/u/second","cwd":"/home/u/first"}`)
	first, ok := parseJSON(line)
	if !ok {
		t.Fatal("valid JSON was rejected")
	}
	for i := range 50 {
		ev, ok := parseJSON(line)
		if !ok || ev != first {
			t.Fatalf("run %d disagreed: %q vs %q", i, ev.Cwd, first.Cwd)
		}
	}
	if first.Cwd != "/home/u/first" {
		t.Fatalf("cwd = %q, want /home/u/first", first.Cwd)
	}
}

func TestCwdSkipsAnEmptyValue(t *testing.T) {
	// The smaller key wins, but an empty value names no directory at all: a
	// record spelling an empty cwd beside a real one reports the real one, or
	// the record carries no working directory and ownership falls through to
	// the launch path alone.
	line := []byte(`{"cwd":"","project_dir":"/home/u/proj"}`)
	first, ok := parseJSON(line)
	if !ok {
		t.Fatal("valid JSON was rejected")
	}
	for i := range 50 {
		ev, ok := parseJSON(line)
		if !ok || ev != first {
			t.Fatalf("run %d disagreed: %q vs %q", i, ev.Cwd, first.Cwd)
		}
	}
	if first.Cwd != "/home/u/proj" {
		t.Fatalf("cwd = %q, want /home/u/proj", first.Cwd)
	}
}

func TestCwdIsDeterministicAcrossSiblingSubtrees(t *testing.T) {
	// Two subtrees, each naming one working directory, neither at the top of
	// the record: subtrees are descended in key order, so "message" is read
	// before "metadata" and its directory is the one the record reports. Map
	// iteration order would otherwise pick a winner per line, and the same
	// line would attribute itself to a different directory on each poll.
	line := []byte(`{"metadata":{"cwd":"/home/u/second"},"message":{"project_dir":"/home/u/first"}}`)
	first, ok := parseJSON(line)
	if !ok {
		t.Fatal("valid JSON was rejected")
	}
	for i := range 50 {
		ev, ok := parseJSON(line)
		if !ok || ev != first {
			t.Fatalf("run %d disagreed: %q vs %q", i, ev.Cwd, first.Cwd)
		}
	}
	if first.Cwd != "/home/u/first" {
		t.Fatalf("cwd = %q, want /home/u/first", first.Cwd)
	}
}

func TestMixedCaseKeysReachTheCounters(t *testing.T) {
	// The tables hold lowercase ASCII names, so a key spelled in any other
	// case still has to reach them, and a rune whose lowercase form is ASCII
	// must not: "K" (U+212A KELVIN SIGN) is not a producer's spelling of "k".
	ev, ok := parseJSON([]byte(`{"usage":{"Output_Tokens":7,"Kelvin_output_tokens":999}}`))
	if !ok {
		t.Fatal("valid JSON was rejected")
	}
	if ev.Usage.Output != 7 {
		t.Fatalf("Output_Tokens read as %d, want 7", ev.Usage.Output)
	}
}
