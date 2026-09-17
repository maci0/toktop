// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The fixtures below are the real record shapes, reduced to the fields this
// package reads. They were taken from live transcripts written by the agents
// themselves, so a format change shows up here as a failing test rather than
// as a silently missing number.

// jsonPath quotes a path the way a real transcript does. It matters on
// Windows, whose separator is JSON's escape character.
func jsonPath(p string) string {
	b, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func claudeLine(cwd string, out int) string {
	return `{"type":"assistant","cwd":` + jsonPath(cwd) + `,"timestamp":"2026-08-25T00:00:00.000Z",` +
		`"message":{"role":"assistant","usage":{"input_tokens":2,"cache_creation_input_tokens":100,` +
		`"cache_read_input_tokens":0,"output_tokens":` + strconv.Itoa(out) + `}}}`
}

func codexMeta(cwd string) string {
	return `{"type":"session_meta","payload":{"id":"x","cwd":` + jsonPath(cwd) + `,"cli_version":"1"}}`
}

func codexTokens(out, total int) string {
	return `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":` +
		`{"input_tokens":10,"output_tokens":` + strconv.Itoa(out) + `,"total_tokens":` + strconv.Itoa(total) + `}}}}`
}

// withStore points an adapter at a temporary transcript directory.
func withStore(t *testing.T, tool string) string {
	t.Helper()
	dir := t.TempDir()
	orig := adapters[tool]
	patched := orig
	patched.roots = func(string) []string { return []string{dir} }
	adapters[tool] = patched
	t.Cleanup(func() { adapters[tool] = orig })
	return dir
}

func append_(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCollectedRecordsStayBounded(t *testing.T) {
	for _, kind := range []valueKind{perMessage, cumulative} {
		w := &Watcher{ad: adapter{kind: kind, parse: parseGeneric}}
		var recs []values
		for range 10000 {
			recs = w.collect(recs, []byte(`{"output_tokens":3,"input_tokens":5,"thinking_tokens":2,"total_tokens":9}`))
		}
		if len(recs) > 2 {
			t.Fatalf("kind %d retained %d records", kind, len(recs))
		}
		want := values{output: 30000, input: 50000, thinking: 20000, total: 9}
		if kind == cumulative {
			want = values{output: 3, input: 5, thinking: 2, total: 9}
		}
		if got := recs[len(recs)-1]; got != want {
			t.Fatalf("kind %d: got %+v, want %+v", kind, got, want)
		}
	}
}

func TestFoldedTranscriptCounts(t *testing.T) {
	for _, tc := range []struct {
		name        string
		tool        string
		preexisting bool
		seed        string
		want        values
	}{
		{"per-message", "claude", false, "", values{output: 55, input: 77, thinking: 14, total: 60}},
		{"new-cumulative", "codex", false, "", values{output: 30, input: 50, thinking: 9, total: 60}},
		{"unseeded-cumulative", "codex", true, "", values{output: 20, input: 30, thinking: 6, total: 60}},
		{"seeded-cumulative", "codex", true, `{"output_tokens":5,"input_tokens":6,"thinking_tokens":1}`, values{output: 25, input: 44, thinking: 8, total: 60}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := withStore(t, tc.tool)
			ad := adapters[tc.tool]
			ad.parse, ad.sessionCwd = parseGeneric, nil
			adapters[tc.tool] = ad
			path := filepath.Join(store, "session.jsonl")
			if tc.preexisting {
				appendRaw(t, path, tc.seed)
			}
			w := Watch(tc.tool, t.TempDir(), time.Now())
			appendRaw(t, path, "\n"+
				`{"output_tokens":10,"input_tokens":20,"thinking_tokens":3,"total_tokens":40}`+"\n"+
				`{"output_tokens":30,"input_tokens":7,"thinking_tokens":9,"total_tokens":20}`+"\n"+
				`{"output_tokens":15,"input_tokens":50,"thinking_tokens":2,"total_tokens":60}`)
			for range 2 {
				s := w.Poll()
				got := values{output: s.Output, input: s.Input, thinking: s.Thinking, total: s.Total}
				if got != tc.want {
					t.Fatalf("got %+v, want %+v", got, tc.want)
				}
			}
		})
	}
}

func BenchmarkCollectRecords(b *testing.B) {
	w := &Watcher{ad: adapter{kind: perMessage, parse: parseGeneric}}
	line := []byte(`{"output_tokens":3,"input_tokens":5}`)
	b.ReportAllocs()
	for b.Loop() {
		var recs []values
		for range 10000 {
			recs = w.collect(recs, line)
		}
		if len(recs) == 0 {
			b.Fatal("no records collected")
		}
	}
}

func TestClaudeUsageIsSummedPerMessage(t *testing.T) {
	store := withStore(t, "claude")
	work := t.TempDir()
	path := filepath.Join(store, "session.jsonl")

	w := Watch("claude", work, time.Now())
	if w == nil {
		t.Fatal("claude should be supported")
	}
	append_(t, path, claudeLine(work, 100), claudeLine(work, 250))
	w.poll(nil)
	if got := w.Sample().Output; got != 350 {
		t.Fatalf("output tokens %d, want 350", got)
	}
	// Only the new lines are counted on the next poll.
	append_(t, path, claudeLine(work, 50))
	w.poll(nil)
	if got := w.Sample().Output; got != 400 {
		t.Fatalf("output tokens %d, want 400", got)
	}
	// input_tokens + cache_creation_input_tokens per line, summed: billed
	// prompt, not the max context. Total-minus-output would shrink toward
	// zero as output accrues against a maxed context size.
	if got := w.Sample().Input; got != 306 {
		t.Fatalf("input tokens %d, want 306 (102 per line × 3)", got)
	}
}

func TestClaudeIgnoresOtherProjects(t *testing.T) {
	store := withStore(t, "claude")
	work, other := t.TempDir(), t.TempDir()

	w := Watch("claude", work, time.Now())
	append_(t, filepath.Join(store, "mine.jsonl"), claudeLine(work, 10))
	append_(t, filepath.Join(store, "theirs.jsonl"), claudeLine(other, 9999))
	w.poll(nil)
	if got := w.Sample().Output; got != 10 {
		t.Fatalf("another project's tokens leaked in: %d", got)
	}
}

// A symlink in the store that points outside must not be followed: the
// transcript roots are writable by the agent, so a planted link could
// otherwise pull in another project's session (or any file) as usage.
func TestTranscriptSymlinkOutsideRootIsIgnored(t *testing.T) {
	store := withStore(t, "claude")
	work := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	append_(t, outside, claudeLine(work, 9999))
	link := filepath.Join(store, "session.jsonl")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks:", err)
	}
	w := Watch("claude", work, time.Now())
	w.poll(nil)
	if got := w.Sample().Output; got != 0 {
		t.Fatalf("followed a symlink out of the store: %d", got)
	}
}

func TestExistingContentIsNotCounted(t *testing.T) {
	store := withStore(t, "claude")
	work := t.TempDir()
	path := filepath.Join(store, "resumed.jsonl")
	// A session that already had content when the watcher attached, the way
	// an agent --continue reuses a transcript across runs.
	append_(t, path, claudeLine(work, 5000))

	w := Watch("claude", work, time.Now())
	w.poll(nil)
	if got := w.Sample().Output; got != 0 {
		t.Fatalf("counted tokens from before attach: %d", got)
	}
	append_(t, path, claudeLine(work, 42))
	w.poll(nil)
	if got := w.Sample().Output; got != 42 {
		t.Fatalf("output tokens %d, want 42", got)
	}
}

func TestCodexCumulativeValuesAreRebased(t *testing.T) {
	store := withStore(t, "codex")
	work := t.TempDir()
	path := filepath.Join(store, "rollout-1.jsonl")

	// A session that was already running before the watcher attached: its
	// earlier usage belongs to whoever ran it, so it is the baseline.
	append_(t, path, codexMeta(work), codexTokens(1000, 5000))

	w := Watch("codex", work, time.Now())
	w.poll(nil)
	if got := w.Sample().Output; got != 0 {
		t.Fatalf("baseline was counted as usage: %d", got)
	}
	append_(t, path, codexTokens(1750, 6200))
	w.poll(nil)
	if got := w.Sample().Output; got != 750 {
		t.Fatalf("output tokens %d, want 750 (1750 minus the 1000 baseline)", got)
	}
	// billed prompt is total-output; the attach-time 4000 is the baseline,
	// so only the 450 spent after attach counts.
	if got := w.Sample().Input; got != 450 {
		t.Fatalf("input tokens %d, want 450 (4450 minus the 4000 baseline)", got)
	}
}

// A line past maxLineBytes must not abort the attach seed: bufio.Scanner
// stops there and would keep a too-low baseline, so a later attach-time
// total looks like this attach's growth.
func TestCumulativeBaselineSurvivesOversizedLine(t *testing.T) {
	store := withStore(t, "codex")
	work := t.TempDir()
	path := filepath.Join(store, "rollout-1.jsonl")

	append_(t, path, codexMeta(work), codexTokens(1000, 5000))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(strings.Repeat("x", maxLineBytes+1) + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	append_(t, path, codexTokens(2000, 7000))

	w := Watch("codex", work, time.Now())
	w.poll(nil)
	if got := w.Sample().Output; got != 0 {
		t.Fatalf("baseline was counted as usage: %d", got)
	}
	append_(t, path, codexTokens(2100, 7200))
	w.poll(nil)
	if got := w.Sample().Output; got != 100 {
		t.Fatalf("output tokens %d, want 100 (2100 minus the 2000 baseline)", got)
	}
}

func TestCumulativeSessionStartedDuringTheReviewCountsInFull(t *testing.T) {
	store := withStore(t, "codex")
	work := t.TempDir()

	// Nothing exists yet: the agent creates its session after the watcher
	// attaches, so every token in it belongs to this attach. Baselining it
	// would throw away the first reading, which on a short watch is most of
	// the tokens and all of the early rate.
	w := Watch("codex", work, time.Now())
	path := filepath.Join(store, "rollout-new.jsonl")
	append_(t, path, codexMeta(work), codexTokens(400, 900))
	w.poll(nil)
	if got := w.Sample().Output; got != 400 {
		t.Fatalf("output tokens %d, want 400", got)
	}
	append_(t, path, codexTokens(1300, 2400))
	w.poll(nil)
	if got := w.Sample().Output; got != 1300 {
		t.Fatalf("output tokens %d, want 1300", got)
	}
}

func TestCodexIgnoresSessionsFromOtherDirectories(t *testing.T) {
	store := withStore(t, "codex")
	work, other := t.TempDir(), t.TempDir()

	w := Watch("codex", work, time.Now())
	append_(t, filepath.Join(store, "rollout-other.jsonl"), codexMeta(other), codexTokens(0, 0), codexTokens(9999, 12345))
	w.poll(nil)
	if got := w.Sample().Output; got != 0 {
		t.Fatalf("another directory's session was counted: %d", got)
	}
}

func TestQwenUsageMetadataIsSummed(t *testing.T) {
	store := withStore(t, "qwen")
	work := t.TempDir()
	path := filepath.Join(store, "chat.jsonl")

	w := Watch("qwen", work, time.Now())
	if w == nil {
		t.Fatal("qwen should be supported")
	}
	// Thinking tokens are output tokens too, so both are counted.
	append_(t, path,
		`{"type":"assistant","cwd":`+jsonPath(work)+`,"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":205,"thoughtsTokenCount":82,"totalTokenCount":387}}`,
		`{"type":"user","cwd":`+jsonPath(work)+`,"message":{"role":"user"}}`,
		`{"type":"assistant","cwd":`+jsonPath(work)+`,"usageMetadata":{"candidatesTokenCount":13,"totalTokenCount":420}}`)
	w.poll(nil)
	if got := w.Sample().Output; got != 300 {
		t.Fatalf("output tokens %d, want 300 (205+82+13)", got)
	}
	// totalTokenCount is the context size of each request, not a per-message
	// cost, so the largest is the honest figure. Summing them would count the
	// same prompt once per turn.
	if got := w.Sample().Total; got != 420 {
		t.Fatalf("total tokens %d, want 420 (the largest context, not the sum)", got)
	}
	if got := w.Sample().Input; got != 100 {
		t.Fatalf("input tokens %d, want 100 (promptTokenCount of the first turn)", got)
	}
}

func TestContextMaximumAcrossTranscripts(t *testing.T) {
	store := withStore(t, "claude")
	work, other := t.TempDir(), t.TempDir()
	w := Watch("claude", work, time.Now())
	first := filepath.Join(store, "first.jsonl")
	second := filepath.Join(store, "second.jsonl")

	append_(t, first, claudeLine(work, 100), claudeLine(other, 9000))
	append_(t, second, claudeLine(work, 250), claudeLine(other, 8000))
	s := w.Poll()
	if s.Output != 350 || s.Input != 204 || s.Total != 352 {
		t.Fatalf("sample = %+v, want output=350 input=204 total=352", s)
	}

	append_(t, first, claudeLine(work, 400))
	s = w.Poll()
	if s.Output != 750 || s.Input != 306 || s.Total != 502 {
		t.Fatalf("sample = %+v, want output=750 input=306 total=502", s)
	}
	if got := w.Poll(); got != s {
		t.Fatalf("unchanged transcripts changed sample: %+v -> %+v", s, got)
	}
}

func TestUnsupportedAgentYieldsNoWatcher(t *testing.T) {
	// gemini's transcripts on the machines checked carry no usage records.
	if Supported("gemini") {
		t.Fatal("gemini is not implemented yet and must not claim to be")
	}
	if w := Watch("gemini", t.TempDir(), time.Now()); w != nil {
		t.Fatal("expected no watcher for an unsupported agent")
	}
	// A nil watcher must be safe to use, so callers never branch.
	var nilW *Watcher
	nilW.Run(context.Background(), time.Millisecond, nil)
	if !nilW.Sample().Empty() {
		t.Fatal("nil watcher reported usage")
	}
}

// Rate is what the UI shows as tok/s for an agent. It must refuse to invent
// a number when there is no measurable interval or no growth: a stalled or
// restarted counter is silence, not zero throughput forever.
func TestRateNeedsPositiveSpanAndGrowth(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	cases := []struct {
		name   string
		prev   Sample
		cur    Sample
		want   float64
		wantOK bool
	}{
		{"growth over one second", Sample{Output: 100, At: t0}, Sample{Output: 350, At: t0.Add(time.Second)}, 250, true},
		{"same instant is not an interval", Sample{Output: 100, At: t0}, Sample{Output: 350, At: t0}, 0, false},
		{"clock went backwards", Sample{Output: 100, At: t0}, Sample{Output: 350, At: t0.Add(-time.Second)}, 0, false},
		{"counter did not move", Sample{Output: 100, At: t0}, Sample{Output: 100, At: t0.Add(time.Second)}, 0, false},
		{"counter reset to zero", Sample{Output: 100, At: t0}, Sample{}, 0, false},
	}
	for _, c := range cases {
		got, ok := Rate(c.prev, c.cur)
		if ok != c.wantOK || got != c.want {
			t.Errorf("%s: Rate(%+v, %+v) = %v,%v want %v,%v",
				c.name, c.prev, c.cur, got, ok, c.want, c.wantOK)
		}
	}

	in, inOK := InputRate(Sample{Input: 80, At: t0}, Sample{Input: 200, At: t0.Add(time.Second)})
	if !inOK || in != 120 {
		t.Errorf("InputRate = %v,%v want 120,true", in, inOK)
	}
	if r, ok := InputRate(Sample{Input: 80, At: t0}, Sample{Input: 80, At: t0.Add(time.Second)}); ok || r != 0 {
		t.Errorf("InputRate with no growth = %v,%v want 0,false", r, ok)
	}
}

// onChange may Poll for a final read. Holding pollMu across the callback
// deadlocks that path.
func TestOnChangeMayPoll(t *testing.T) {
	store := withStore(t, "claude")
	work := t.TempDir()
	path := filepath.Join(store, "session.jsonl")
	w := Watch("claude", work, time.Now())
	append_(t, path, claudeLine(work, 100))

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.poll(func(Sample) { w.Poll() })
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("onChange calling Poll deadlocked")
	}
	if got := w.Sample().Output; got != 100 {
		t.Fatalf("output tokens %d, want 100", got)
	}
}

func TestRunReportsGrowth(t *testing.T) {
	store := withStore(t, "claude")
	work := t.TempDir()
	path := filepath.Join(store, "live.jsonl")

	w := Watch("claude", work, time.Now())
	ctx := t.Context()

	got := make(chan Sample, 8)
	go w.Run(ctx, 20*time.Millisecond, func(s Sample) { got <- s })

	append_(t, path, claudeLine(work, 120))
	select {
	case s := <-got:
		if s.Output != 120 {
			t.Fatalf("reported %d, want 120", s.Output)
		}
	// Generous on purpose: this asserts that growth is reported at all, and a
	// loaded CI runner should not turn that into a failure.
	case <-time.After(10 * time.Second):
		t.Fatal("no usage reported")
	}
}

func TestDefinedAgentTranscriptsAreReadGenerically(t *testing.T) {
	// An agent gauntlet was never compiled to know about: its transcript is
	// readable as long as the records carry recognizable counters and a cwd.
	store := t.TempDir()
	work := t.TempDir()
	if err := RegisterSpec("piclone", Spec{Roots: []string{store}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		adaptersMu.Lock()
		delete(adapters, "piclone")
		adaptersMu.Unlock()
	})
	if !Supported("piclone") {
		t.Fatal("a registered spec should make the agent supported")
	}

	w := Watch("piclone", work, time.Now())
	if w == nil {
		t.Fatal("expected a watcher")
	}
	path := filepath.Join(store, "session.jsonl")
	append_(t, path,
		`{"role":"assistant","cwd":`+jsonPath(work)+`,"usage":{"output_tokens":140,"reasoning_tokens":40}}`,
		`{"role":"assistant","cwd":`+jsonPath(work)+`,"usage":{"output_tokens":60}}`)
	w.poll(nil)
	if got := w.Sample(); got.Output != 200 || got.Thinking != 40 {
		t.Fatalf("got %+v, want output 200 and thinking 40", got)
	}
}

func TestDefinedAgentIgnoresOtherDirectories(t *testing.T) {
	store, work, other := t.TempDir(), t.TempDir(), t.TempDir()
	if err := RegisterSpec("piclone2", Spec{Roots: []string{store}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		adaptersMu.Lock()
		delete(adapters, "piclone2")
		adaptersMu.Unlock()
	})
	w := Watch("piclone2", work, time.Now())
	append_(t, filepath.Join(store, "s.jsonl"),
		`{"role":"assistant","cwd":`+jsonPath(other)+`,"usage":{"output_tokens":5000}}`)
	w.poll(nil)
	if got := w.Sample().Output; got != 0 {
		t.Fatalf("another directory's usage leaked in: %d", got)
	}
}

func TestRegisterSpecValidates(t *testing.T) {
	if err := RegisterSpec("", Spec{Roots: []string{"/tmp"}}); !errors.Is(err, ErrEmptyTool) {
		t.Errorf("nameless spec = %v, want ErrEmptyTool", err)
	}
	if err := RegisterSpec("  ", Spec{Roots: []string{"/tmp"}}); !errors.Is(err, ErrEmptyTool) {
		t.Errorf("whitespace name = %v, want ErrEmptyTool", err)
	}
	if err := RegisterSpec("x", Spec{}); !errors.Is(err, ErrNoRoots) {
		t.Errorf("no roots = %v, want ErrNoRoots", err)
	}
	if err := RegisterSpec("x", Spec{Roots: []string{"", "  "}}); !errors.Is(err, ErrNoRoots) {
		t.Errorf("blank roots = %v, want ErrNoRoots", err)
	}
}

func TestParseClaudeThinkingOnly(t *testing.T) {
	line := []byte(`{"type":"assistant","cwd":"/tmp/p","message":{"usage":{"input_tokens":0,"output_tokens":0,"output_tokens_details":{"thinking_tokens":40}}}}`)
	v, cwd, ok := parseClaude(line)
	if !ok {
		t.Fatal("thinking-only assistant record must count")
	}
	if v.thinking != 40 || v.output != 0 || v.input != 0 {
		t.Fatalf("got %+v, want thinking 40", v)
	}
	if cwd != "/tmp/p" {
		t.Fatalf("cwd = %q", cwd)
	}
}

func TestRegisterSpecTrimsToolName(t *testing.T) {
	store := t.TempDir()
	if err := RegisterSpec("  spaced  ", Spec{Roots: []string{store}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		adaptersMu.Lock()
		delete(adapters, "spaced")
		adaptersMu.Unlock()
	})
	if !Supported("spaced") || !Supported("  spaced") {
		t.Fatal("trimmed name should be the lookup key")
	}
	if Watch("spaced", t.TempDir(), time.Now()) == nil {
		t.Fatal("Watch should find the trimmed name")
	}
}

func TestSupportedRejectsBlankRootSpecs(t *testing.T) {
	addDef(t, "blanky", Spec{Roots: []string{"", "  "}})
	if Supported("blanky") {
		t.Fatal("Supported must match RegisterSpec: blank roots are not readable")
	}
	if Watch("blanky", t.TempDir(), time.Now()) != nil {
		t.Fatal("Watch must not build a watcher RegisterSpec would reject")
	}
}

func TestProcessWatchMatchesWatch(t *testing.T) {
	p := Process{Tool: "claude", Dir: t.TempDir()}
	since := time.Now()
	w1 := Watch(p.Tool, p.Dir, since)
	w2 := p.Watch(since)
	if w1 == nil || w2 == nil {
		t.Fatal("claude is a file adapter")
	}
	if w1.Tool() != w2.Tool() || w1.Dir() != w2.Dir() {
		t.Fatalf("Watch and Process.Watch disagreed: %q %q vs %q %q",
			w1.Tool(), w1.Dir(), w2.Tool(), w2.Dir())
	}
	if got := (Process{Tool: "gemini"}).Watch(since); got != nil {
		t.Fatal("unsupported agent must still yield no watcher")
	}
}

func TestSampleEmptyIncludesThinking(t *testing.T) {
	if !(Sample{}).Empty() {
		t.Fatal("zero sample must be empty")
	}
	if (Sample{Thinking: 12}).Empty() {
		t.Fatal("thinking-only sample must not look empty")
	}
	if (Sample{Output: 1}).Empty() || (Sample{Input: 1}).Empty() || (Sample{Total: 1}).Empty() {
		t.Fatal("any counter makes a sample non-empty")
	}
}

func TestClaudeThinkingOnlyIsAReading(t *testing.T) {
	store := withStore(t, "claude")
	work := t.TempDir()
	w := Watch("claude", work, time.Now())
	if w == nil {
		t.Fatal("claude should be supported")
	}
	path := filepath.Join(store, "session.jsonl")
	append_(t, path, `{"type":"assistant","cwd":`+jsonPath(work)+`,"message":{"usage":{"input_tokens":0,"output_tokens":0,"output_tokens_details":{"thinking_tokens":40}}}}`)
	w.poll(nil)
	s := w.Sample()
	if s.Empty() || s.Thinking != 40 {
		t.Fatalf("got %+v, want thinking 40", s)
	}
	if s.Output != 0 || s.Input != 0 {
		t.Fatalf("invented billed tokens: %+v", s)
	}
}

func TestQwenThinkingOnlyIsAReading(t *testing.T) {
	store := withStore(t, "qwen")
	work := t.TempDir()
	w := Watch("qwen", work, time.Now())
	if w == nil {
		t.Fatal("qwen should be supported")
	}
	path := filepath.Join(store, "chat.jsonl")
	append_(t, path, `{"type":"assistant","cwd":`+jsonPath(work)+`,"usageMetadata":{"thoughtsTokenCount":9}}`)
	w.poll(nil)
	s := w.Sample()
	// qwen's thoughts are output tokens too, so both counters move.
	if s.Thinking != 9 || s.Output != 9 {
		t.Fatalf("got %+v, want thinking 9 and output 9", s)
	}
}

func TestCodexThinkingOnlyIsAReading(t *testing.T) {
	store := withStore(t, "codex")
	work := t.TempDir()
	w := Watch("codex", work, time.Now())
	if w == nil {
		t.Fatal("codex should be supported")
	}
	path := filepath.Join(store, "rollout.jsonl")
	append_(t, path, codexMeta(work),
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"reasoning_output_tokens":12}}}}`)
	w.poll(nil)
	s := w.Sample()
	if s.Empty() || s.Thinking != 12 {
		t.Fatalf("got %+v, want thinking 12", s)
	}
	if s.Output != 0 {
		t.Fatalf("invented output tokens: %+v", s)
	}
}

func TestClaudeUsageMatchesFoldedDirectorySpelling(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		t.Skip("directory folding is a Windows/macOS filesystem rule")
	}
	store := withStore(t, "claude")
	work := t.TempDir()
	w := Watch("claude", work, time.Now())
	if w == nil {
		t.Fatal("claude should be supported")
	}
	alt := strings.ToUpper(w.Dir())
	if alt == w.Dir() {
		t.Skip("temp dir has no case to fold")
	}
	path := filepath.Join(store, "session.jsonl")
	append_(t, path, claudeLine(alt, 40))
	w.poll(nil)
	if got := w.Sample().Output; got != 40 {
		t.Fatalf("output tokens %d, want 40: folded cwd %q was not attributed to %q", got, alt, w.Dir())
	}
}

func TestExpandHomeAcceptsBothSeparators(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	got := expandHome("~/rel")
	want := filepath.Join(home, "rel")
	if got != want {
		t.Errorf("expandHome(~/rel) = %q, want %q", got, want)
	}
	got = expandHome(`~\rel`)
	if got != want {
		t.Errorf(`expandHome(~\rel) = %q, want %q`, got, want)
	}
	if got := expandHome("~"); got != home {
		t.Errorf("expandHome(~) = %q, want %q", got, home)
	}
	if got := expandHome("rel"); got != "rel" {
		t.Errorf("expandHome(rel) = %q, want unchanged", got)
	}
}

func TestClankerLogIsReadFromTheProjectItself(t *testing.T) {
	// clanker writes state/token_stats.jsonl inside the repository it runs in,
	// one record per request and no cwd field: the location is the attribution.
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	w := Watch("clanker", work, time.Now())
	if w == nil {
		t.Fatal("clanker should be supported")
	}
	path := filepath.Join(work, "state", "token_stats.jsonl")
	append_(t, path,
		`{"ts":1786923403,"provider":"x","model":"y","prompt_tokens":32027,"completion_tokens":1663,"total_tokens":33690,"ok":true}`,
		`{"ts":1786923500,"provider":"x","model":"y","prompt_tokens":100,"completion_tokens":337,"total_tokens":437,"ok":true}`)
	w.poll(nil)
	s := w.Sample()
	if s.Output != 2000 {
		t.Fatalf("output tokens %d, want 2000 (1663+337)", s.Output)
	}
	if s.Input != 32127 {
		t.Fatalf("input tokens %d, want 32127 (32027+100)", s.Input)
	}
}

func TestDirPlaceholderInDefinedRoots(t *testing.T) {
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, ".logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RegisterSpec("inproject", Spec{Roots: []string{"{dir}/.logs"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		adaptersMu.Lock()
		delete(adapters, "inproject")
		adaptersMu.Unlock()
	})
	w := Watch("inproject", work, time.Now())
	append_(t, filepath.Join(work, ".logs", "usage.jsonl"),
		`{"usage":{"output_tokens":77}}`)
	w.poll(nil)
	if got := w.Sample().Output; got != 77 {
		t.Fatalf("output tokens %d, want 77", got)
	}
}

// GitHub Copilot CLI records the working directory once, in the session.start
// event, and the usage on later assistant.message events.
func TestCopilotCliEventsAreRead(t *testing.T) {
	store := withStore(t, "copilot")
	work := t.TempDir()
	path := copilotSession(t, store, "sess-1", work)

	w := Watch("copilot", work, time.Now())
	if w == nil {
		t.Fatal("copilot should be readable")
	}
	append_(t, path,
		`{"type":"assistant.message","id":"e1","data":{"messageId":"m1","usage":{"prompt_tokens":900,"completion_tokens":120,"total_tokens":1020}}}`,
		`{"type":"assistant.message","id":"e2","data":{"messageId":"m2","usage":{"prompt_tokens":940,"completion_tokens":80,"total_tokens":1100}}}`)
	w.poll(nil)
	s := w.Sample()
	if s.Output != 200 {
		t.Fatalf("output tokens %d, want 200 (120+80)", s.Output)
	}
	if s.Input != 1840 {
		t.Fatalf("input tokens %d, want 1840 (900+940)", s.Input)
	}
}

// Another project's Copilot session must not land in this watcher.
func TestCopilotCliIgnoresOtherProjects(t *testing.T) {
	store := withStore(t, "copilot")
	work, other := t.TempDir(), t.TempDir()
	mine := copilotSession(t, store, "mine", work)
	theirs := copilotSession(t, store, "theirs", other)

	w := Watch("copilot", work, time.Now())
	append_(t, mine, `{"type":"assistant.message","data":{"usage":{"completion_tokens":70}}}`)
	append_(t, theirs, `{"type":"assistant.message","data":{"usage":{"completion_tokens":5000}}}`)
	w.poll(nil)
	if got := w.Sample().Output; got != 70 {
		t.Fatalf("output tokens %d, want 70: another project's session leaked in", got)
	}
}

// A transcript caught between file creation and its header flush has no
// verdict yet: the empty first sight must not harden into a permanent
// "not mine" that silently drops the whole session.
func TestHeaderNotYetWrittenIsRetriedNotCached(t *testing.T) {
	store := withStore(t, "copilot")
	work := t.TempDir()
	path := filepath.Join(store, "sess-late", "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	w := Watch("copilot", work, time.Now())
	append_(t, path) // create the file empty: the agent has not flushed its header
	w.poll(nil)
	if got := w.Sample().Output; got != 0 {
		t.Fatalf("output tokens %d, want 0: an empty undecided file counts for nothing yet", got)
	}

	append_(t, path,
		`{"type":"session.start","id":"e0","data":{"sessionId":"s","context":{"cwd":`+jsonPath(work)+`}}}`,
		`{"type":"assistant.message","data":{"usage":{"completion_tokens":120}}}`)
	w.poll(nil)
	if got := w.Sample().Output; got != 120 {
		t.Fatalf("output tokens %d, want 120: a headerless first sight was cached as not ours", got)
	}
}

// A file deep enough to rule out a pending header is refused for good, even
// when usage records show up later: without a header there is nothing to
// attribute them to.
func TestHeaderlessFilePastScanCapIsRefusedDurably(t *testing.T) {
	store := withStore(t, "copilot")
	work, other := t.TempDir(), t.TempDir()
	path := filepath.Join(store, "sess-orphan", "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	w := Watch("copilot", work, time.Now())
	for range ownerScanLines {
		append_(t, path, `{"type":"other.event"}`)
	}
	append_(t, path, `{"type":"assistant.message","data":{"usage":{"completion_tokens":999}}}`)
	w.poll(nil)

	// A late header naming this directory must not resurrect it either: the
	// refusal was earned, and re-reading history would misattribute it.
	append_(t, path,
		`{"type":"session.start","id":"e0","data":{"sessionId":"s","context":{"cwd":`+jsonPath(other)+`}}}`,
		`{"type":"assistant.message","data":{"usage":{"completion_tokens":40}}}`)
	w.poll(nil)
	if got := w.Sample().Output; got != 0 {
		t.Fatalf("output tokens %d, want 0: a headerless file was credited to this watcher", got)
	}
}

// A session file that shrinks and is rewritten belongs to whoever its new
// header names: the owner verdict from the previous contents must not stick.
func TestTruncatedSessionRechecksOwner(t *testing.T) {
	store := withStore(t, "copilot")
	work, other := t.TempDir(), t.TempDir()
	path := copilotSession(t, store, "reused", other)

	w := Watch("copilot", work, time.Now())
	append_(t, path, `{"type":"assistant.message","data":{"usage":{"completion_tokens":5000}}}`)
	w.poll(nil)
	if got := w.Sample().Output; got != 0 {
		t.Fatalf("output tokens %d, want 0: another project's session leaked in", got)
	}

	if err := os.WriteFile(path, []byte(
		`{"type":"session.start","id":"e0","data":{"sessionId":"s","context":{"cwd":`+jsonPath(work)+`}}}`+"\n"+
			`{"type":"assistant.message","data":{"usage":{"completion_tokens":70}}}`+"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	w.poll(nil)
	if got := w.Sample().Output; got != 70 {
		t.Fatalf("output tokens %d, want 70: a rewritten file kept the previous owner", got)
	}
}

// copilotSession starts one session directory with its session.start header
// and returns the events file to append to.
func copilotSession(t *testing.T, store, name, cwd string) string {
	t.Helper()
	dir := filepath.Join(store, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events.jsonl")
	append_(t, path, `{"type":"session.start","id":"e0","data":{"sessionId":"s","context":{"cwd":`+jsonPath(cwd)+`}}}`)
	return path
}

// appendRaw writes bytes verbatim; unlike append_ it adds no newline, so a
// test can stage torn or unterminated transcript lines.
func appendRaw(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// A record flushed in two chunks must be counted exactly once: the torn
// prefix waits for its remainder instead of being consumed and lost.
func TestTornRecordIsCountedOnceComplete(t *testing.T) {
	store := withStore(t, "claude")
	work := t.TempDir()
	path := filepath.Join(store, "session.jsonl")

	w := Watch("claude", work, time.Now())
	rest := claudeLine(work, 250)
	half := len(rest) / 2
	appendRaw(t, path, claudeLine(work, 100)+"\n"+rest[:half])
	w.poll(nil)
	if got := w.Sample().Output; got != 100 {
		t.Fatalf("output tokens %d, want 100 (a torn record counts for nothing yet)", got)
	}
	appendRaw(t, path, rest[half:]+"\n")
	w.poll(nil)
	if got := w.Sample().Output; got != 350 {
		t.Fatalf("output tokens %d, want 350: completing a torn record lost it", got)
	}
}

// A complete final line without a trailing newline counts now and must not
// misalign the offset against records appended afterwards.
func TestFinalLineWithoutNewlineSurvivesGrowth(t *testing.T) {
	store := withStore(t, "claude")
	work := t.TempDir()
	path := filepath.Join(store, "session.jsonl")

	w := Watch("claude", work, time.Now())
	appendRaw(t, path, claudeLine(work, 100))
	w.poll(nil)
	if got := w.Sample().Output; got != 100 {
		t.Fatalf("output tokens %d, want 100", got)
	}
	appendRaw(t, path, "\n"+claudeLine(work, 250))
	w.poll(nil)
	if got := w.Sample().Output; got != 350 {
		t.Fatalf("output tokens %d, want 350: growth after an unterminated line lost a record", got)
	}
}

// One record over the size cap is junk, but it must not stall every later
// record in the same file behind it.
func TestOversizedLineDoesNotStallFollowingRecords(t *testing.T) {
	store := withStore(t, "claude")
	work := t.TempDir()
	path := filepath.Join(store, "session.jsonl")

	w := Watch("claude", work, time.Now())
	// Longer than one bufio fill past the cap, so consumeAppend actually
	// takes the discard path (a line of maxLineBytes+1 never fills past
	// the cap on a BufferFull: maxLineBytes is a multiple of the fill).
	appendRaw(t, path, strings.Repeat("x", maxLineBytes+appendReaderBytes)+"\n")
	appendRaw(t, path, claudeLine(work, 42))
	w.poll(nil)
	if got := w.Sample().Output; got != 42 {
		t.Fatalf("output tokens %d, want 42: an oversized line stalled the file", got)
	}
}

// A transcript that grows without its mtime moving must still be read. NTFS
// timestamps are coarse and Windows updates last-write lazily for an open
// handle, so two appends can share one stamp; skipping the second would drop
// every record it carried.
func TestGrowthIsReadWhenTheMtimeStandsStill(t *testing.T) {
	store := withStore(t, "claude")
	work := t.TempDir()
	path := filepath.Join(store, "s.jsonl")
	append_(t, path, claudeLine(work, 5))

	w := Watch("claude", work, time.Now())
	w.poll(nil)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	append_(t, path, claudeLine(work, 42))
	// Put the timestamp back where the first read saw it, which is what a
	// coarse clock does on its own.
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	w.poll(nil)
	if got := w.Sample().Output; got != 42 {
		t.Fatalf("output tokens %d, want 42: growth under an unchanged mtime was skipped", got)
	}
}

// A failed open must not mark the current size as processed: restoring
// access and polling again has to see the records that were already there.
func TestReadErrorIsNotCachedAsProcessed(t *testing.T) {
	store := withStore(t, "claude")
	work := t.TempDir()
	path := filepath.Join(store, "session.jsonl")

	w := Watch("claude", work, time.Now())
	append_(t, path, claudeLine(work, 100))
	if err := os.Chmod(path, 0); err != nil {
		t.Skip("chmod not supported")
	}
	f, err := os.Open(path)
	if err == nil {
		f.Close()
		_ = os.Chmod(path, 0o644)
		t.Skip("process can read mode-0 files")
	}
	w.poll(nil)
	if got := w.Sample().Output; got != 0 {
		t.Fatalf("output tokens %d, want 0: an unreadable file counted", got)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	w.poll(nil)
	if got := w.Sample().Output; got != 100 {
		t.Fatalf("output tokens %d, want 100: a failed read was cached as processed", got)
	}
}

// Age a file out of the recency window. The walk must drop it so a long-lived
// watcher does not keep every session file ever written; the counts and the
// offset must survive so a later append is added, not re-read from the start.
func TestIdleTranscriptDropsFromTheWalkWithoutLosingCounts(t *testing.T) {
	store := withStore(t, "claude")
	work := t.TempDir()
	path := filepath.Join(store, "session.jsonl")

	w := Watch("claude", work, time.Now())
	append_(t, path, claudeLine(work, 100))
	w.poll(nil)
	if got := w.Sample().Output; got != 100 {
		t.Fatalf("output tokens %d, want 100", got)
	}

	old := time.Now().Add(-recencyWindow - time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	w.scanned = time.Time{}
	if got := w.candidates(); len(got) != 0 {
		t.Fatalf("idle file still in the walk: %v", got)
	}
	if got := w.Sample().Output; got != 100 {
		t.Fatalf("output tokens %d after idle drop, want 100", got)
	}

	append_(t, path, claudeLine(work, 50))
	w.scanned = time.Time{}
	w.poll(nil)
	if got := w.Sample().Output; got != 150 {
		t.Fatalf("output tokens %d, want 150: re-entry after idle drop lost or double-counted", got)
	}
}

// A foreign transcript that ages out of the walk must leave the owner/offset
// maps, or a dashboard following one agent would pin an entry per session
// file on the machine for the rest of the process.
func TestForeignIdleTranscriptIsForgotten(t *testing.T) {
	store := withStore(t, "copilot")
	work, other := t.TempDir(), t.TempDir()
	mine := copilotSession(t, store, "mine", work)
	theirs := copilotSession(t, store, "theirs", other)

	w := Watch("copilot", work, time.Now())
	append_(t, mine, `{"type":"assistant.message","data":{"usage":{"completion_tokens":10}}}`)
	append_(t, theirs, `{"type":"assistant.message","data":{"usage":{"completion_tokens":999}}}`)
	w.poll(nil)
	if _, tracked := w.owner[theirs]; !tracked {
		t.Fatal("expected the foreign session to be cached as not-ours while it is recent")
	}

	old := time.Now().Add(-recencyWindow - time.Minute)
	if err := os.Chtimes(theirs, old, old); err != nil {
		t.Fatal(err)
	}
	w.scanned = time.Time{}
	_ = w.candidates()
	if _, tracked := w.owner[theirs]; tracked {
		t.Fatal("foreign session still tracked after aging out of the walk")
	}
	if _, tracked := w.offsets[theirs]; tracked {
		t.Fatal("foreign session offset still tracked after aging out of the walk")
	}
	if got := w.Sample().Output; got != 10 {
		t.Fatalf("output tokens %d, want 10 after forgetting the foreign session", got)
	}
}

// A shared transcript listing that no watcher has refreshed must leave the
// process-wide cache. Clanker (and {dir} specs) key it on the project path,
// so a dashboard that follows agents through many trees would otherwise pin
// one slice per directory for the rest of the run.
func TestStaleRootListingIsForgotten(t *testing.T) {
	rootListMu.Lock()
	saved := rootLists
	rootLists = map[string]rootListing{}
	rootListMu.Unlock()
	t.Cleanup(func() {
		rootListMu.Lock()
		rootLists = saved
		rootListMu.Unlock()
	})

	stale := t.TempDir()
	cutoff := time.Now().Add(-time.Hour)
	_ = listTranscripts(stale, ".jsonl", cutoff, true)
	key := rootListKey(stale, ".jsonl")
	rootListMu.Lock()
	c, ok := rootLists[key]
	if !ok {
		rootListMu.Unlock()
		t.Fatal("expected the listing to be cached after a walk")
	}
	c.at = time.Now().Add(-recencyWindow - time.Second)
	rootLists[key] = c
	rootListMu.Unlock()

	_ = listTranscripts(t.TempDir(), ".jsonl", cutoff, true)
	rootListMu.Lock()
	_, still := rootLists[key]
	rootListMu.Unlock()
	if still {
		t.Fatal("stale root listing still cached after another walk")
	}
}
