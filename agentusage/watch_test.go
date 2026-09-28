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
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
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

// withStore points an adapter at a temporary transcript directory. The
// registry is shared with watcher goroutines, so the row is read and restored
// under its lock: a watcher re-resolves its adapter on every poll.
func withStore(t *testing.T, tool string) string {
	t.Helper()
	dir := t.TempDir()
	adaptersMu.Lock()
	orig := adapters[tool]
	patched := orig
	patched.roots = func(string, time.Time) []string { return []string{dir} }
	adapters[tool] = patched
	adaptersMu.Unlock()
	t.Cleanup(func() {
		adaptersMu.Lock()
		adapters[tool] = orig
		adaptersMu.Unlock()
	})
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
			adaptersMu.Lock()
			ad := adapters[tc.tool]
			ad.parse, ad.sessionCwd = parseGeneric, nil
			adapters[tc.tool] = ad
			adaptersMu.Unlock()
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

// The owner scan buffers a line the record path accepts. With a smaller cap
// bufio.Scanner stopped on ErrTooLong, owns() saw fewer than ownerScanLines
// and returned "undecided", so readNew bailed on every poll and the session
// was never read again.
func TestOwnerScanAcceptsARecordSizedHeaderLine(t *testing.T) {
	store := withStore(t, "codex")
	work := t.TempDir()
	path := filepath.Join(store, "rollout-1.jsonl")

	append_(t, path, strings.Repeat("x", maxLineBytes-1))
	append_(t, path, codexMeta(work))
	append_(t, path, codexTokens(120, 1300))

	w := Watch("codex", work, time.Now())
	w.poll(nil)
	if mine, decided := w.owns(path); !decided || !mine {
		t.Fatalf("owns = (%v, %v), want (true, true) for a header the record path accepts", mine, decided)
	}
	append_(t, path, codexTokens(220, 1400))
	w.poll(nil)
	// The first poll seeds the cumulative baseline at 120; 220 is the growth
	// this poll reports. Before the cap fix the file was never read at all.
	if got := w.Sample().Output; got != 100 {
		t.Fatalf("output tokens %d, want 100: the oversized leading line blocked the read", got)
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
	// A name this package does not know is not readable.
	if Supported("notepad") {
		t.Fatal("Supported(notepad) = true")
	}
	if w := Watch("notepad", t.TempDir(), time.Now()); w != nil {
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

// A transcript that records how long the model spent is reporting the
// generation interval, and that is the interval a rate is over. The wall gap
// between two readings is a different one: a grok turn's counts arrive when
// the turn ends, so the gap since the previous turn covers the turn's whole
// wall time and the tool calls in it.
func TestRateUsesRecordedSpanOverTheWallGap(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	prev := Sample{Output: 100, Input: 400, Thinking: 20, Span: 4 * time.Second, At: t0}
	cur := Sample{
		Output: 700, Input: 1000, Thinking: 70,
		Span: 14 * time.Second, At: t0.Add(time.Minute),
	}
	if r, ok := Rate(prev, cur); !ok || r != 60 {
		t.Errorf("Rate over the recorded span = %v,%v want 60,true", r, ok)
	}
	if r, ok := InputRate(prev, cur); !ok || r != 60 {
		t.Errorf("InputRate over the recorded span = %v,%v want 60,true", r, ok)
	}
	if r, ok := ThinkingRate(prev, cur); !ok || r != 5 {
		t.Errorf("ThinkingRate over the recorded span = %v,%v want 5,true", r, ok)
	}
	// A transcript that recorded no span for the interval still rates over
	// the wall gap, which is the only interval it leaves a caller.
	if r, ok := Rate(prev, Sample{Output: 700, At: t0.Add(time.Second)}); !ok || r != 600 {
		t.Errorf("Rate with no recorded span = %v,%v want 600,true", r, ok)
	}
	// Growth with neither interval is silence, not a rate.
	if r, ok := Rate(prev, Sample{Output: 700}); ok || r != 0 {
		t.Errorf("Rate with no interval at all = %v,%v want 0,false", r, ok)
	}
}

// A delta is the interval a caller reports, so a rewrite under the watcher
// (counts that went down) must not read as growth, and reasoning on its own
// is growth a caller would otherwise drop.
func TestDeltaReportsGrowthAndRefusesARewrite(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	prev := Sample{Output: 100, Thinking: 10, Input: 80, Total: 4000, At: t0}
	for _, tc := range []struct {
		name string
		cur  Sample
		want Delta
		ok   bool
	}{
		{
			name: "growth in every counter",
			cur:  Sample{Output: 350, Thinking: 25, Input: 200, Total: 6000, At: t0.Add(time.Second)},
			want: Delta{Output: 250, Thinking: 15, Input: 120, At: t0.Add(time.Second)},
			ok:   true,
		},
		{
			name: "nothing new",
			cur:  Sample{Output: 100, Thinking: 10, Input: 80, Total: 4000, At: t0.Add(time.Second)},
			want: Delta{At: t0.Add(time.Second)},
		},
		{
			name: "reasoning without billed output is growth",
			cur:  Sample{Output: 100, Thinking: 30, Input: 80, Total: 4000, At: t0.Add(time.Second)},
			want: Delta{Thinking: 20, At: t0.Add(time.Second)},
			ok:   true,
		},
		{
			name: "a rewritten transcript is not negative growth",
			cur:  Sample{Output: 40, Input: 20, At: t0.Add(time.Second)},
			want: Delta{At: t0.Add(time.Second)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, ok := tc.cur.Delta(prev)
			if ok != tc.ok || d != tc.want {
				t.Fatalf("Delta(%+v, %+v) = %+v,%v want %+v,%v",
					prev, tc.cur, d, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestRatesRequireTwoReadings(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	for name, rate := range map[string]func(Sample, Sample) (float64, bool){
		"output": Rate,
		"input":  InputRate,
	} {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				prev Sample
				cur  Sample
				want float64
				ok   bool
			}{
				{"no previous reading", Sample{}, Sample{Output: 120, Input: 120, At: t0}, 0, false},
				{"previous timestamp missing", Sample{Output: 20, Input: 20}, Sample{Output: 120, Input: 120, At: t0}, 0, false},
				{"current timestamp missing", Sample{At: time.Time{}.Add(-time.Second)}, Sample{Output: 120, Input: 120}, 0, false},
				{"explicit zero baseline", Sample{At: t0}, Sample{Output: 120, Input: 120, At: t0.Add(time.Second)}, 120, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if got, ok := rate(tc.prev, tc.cur); got != tc.want || ok != tc.ok {
						t.Fatalf("rate = %v,%v, want %v,%v", got, ok, tc.want, tc.ok)
					}
				})
			}
		})
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
	t.Cleanup(func() { UnregisterSpec("piclone") })
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
	t.Cleanup(func() { UnregisterSpec("piclone2") })
	w := Watch("piclone2", work, time.Now())
	append_(t, filepath.Join(store, "s.jsonl"),
		`{"role":"assistant","cwd":`+jsonPath(other)+`,"usage":{"output_tokens":5000}}`)
	w.poll(nil)
	if got := w.Sample().Output; got != 0 {
		t.Fatalf("another directory's usage leaked in: %d", got)
	}
}

// A defined agent that writes more than one extension (compressed by
// default, plain when compression is off) is read from both, and a blank
// entry matches nothing rather than every file under the root.
func TestSpecSuffixesMatchEveryExtension(t *testing.T) {
	store := t.TempDir()
	work := t.TempDir()
	if err := RegisterSpec("multisuffix", Spec{
		Roots:    []string{store},
		Suffixes: []string{".jsonl.zst", "  ", ".jsonl"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		adaptersMu.Lock()
		delete(adapters, "multisuffix")
		adaptersMu.Unlock()
	})
	w := Watch("multisuffix", work, time.Now())
	append_(t, filepath.Join(store, "a.jsonl"),
		`{"role":"assistant","cwd":`+jsonPath(work)+`,"usage":{"output_tokens":140}}`)
	append_(t, filepath.Join(store, "b.jsonl.zst"),
		`{"role":"assistant","cwd":`+jsonPath(work)+`,"usage":{"output_tokens":60}}`)
	append_(t, filepath.Join(store, "config.json"),
		`{"role":"assistant","cwd":`+jsonPath(work)+`,"usage":{"output_tokens":5000}}`)
	w.poll(nil)
	if got := w.Sample().Output; got != 200 {
		t.Fatalf("output %d, want 200: every listed suffix should match, and nothing else", got)
	}
}

// A single suffix that is blank once trimmed is no suffix at all, so the spec
// falls back to the default rather than matching nothing, and a padded value
// names the same extension as an unpadded one.
func TestSpecSuffixIsTrimmed(t *testing.T) {
	store, work := t.TempDir(), t.TempDir()
	if err := RegisterSpec("padsuffix", Spec{Roots: []string{store}, Suffix: "  .jsonl  "}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { UnregisterSpec("padsuffix") })
	w := Watch("padsuffix", work, time.Now())
	append_(t, filepath.Join(store, "a.jsonl"),
		`{"role":"assistant","cwd":`+jsonPath(work)+`,"usage":{"output_tokens":90}}`)
	w.poll(nil)
	if got := w.Sample().Output; got != 90 {
		t.Fatalf("output %d, want 90: a padded suffix should still name .jsonl", got)
	}

	if err := RegisterSpec("blanksuffix", Spec{Roots: []string{store}, Suffix: "   "}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { UnregisterSpec("blanksuffix") })
	ad, ok := adapterFor("blanksuffix")
	if !ok {
		t.Fatal("a spec with roots should resolve to an adapter")
	}
	if got := ad.fileSuffixes(); len(got) != 1 || got[0] != DefaultSuffix {
		t.Fatalf("suffixes %q, want the default %q", got, DefaultSuffix)
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

func TestUnregisterSpecRemovesARegistration(t *testing.T) {
	store := t.TempDir()
	if err := RegisterSpec("unreg", Spec{Roots: []string{store}}); err != nil {
		t.Fatal(err)
	}
	if !Supported("unreg") {
		t.Fatal("a registered spec should make the agent supported")
	}
	if !UnregisterSpec("  unreg  ") {
		t.Fatal("UnregisterSpec should canonicalize the name like RegisterSpec")
	}
	if Supported("unreg") {
		t.Fatal("the agent should be unreadable again once unregistered")
	}
	if w := Watch("unreg", t.TempDir(), time.Now()); w != nil || !errors.Is(w.Err(), ErrUnsupportedTool) {
		t.Fatalf("Watch after unregister = %v, %v; want a nil watcher reporting ErrUnsupportedTool", w, w.Err())
	}
	if UnregisterSpec("unreg") {
		t.Fatal("a second UnregisterSpec should report nothing removed")
	}
	if UnregisterSpec("") || UnregisterSpec("   ") {
		t.Fatal("a blank name cannot name a registration")
	}
}

func TestUnregisterSpecRestoresTheDisplacedAdapter(t *testing.T) {
	store := t.TempDir()
	builtin, ok := adapterFor("codex")
	if !ok {
		t.Fatal("codex should be a built-in adapter")
	}
	if err := RegisterSpec("codex", Spec{Roots: []string{store}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { UnregisterSpec("codex") })
	got, ok := adapterFor("codex")
	if !ok || !slices.Equal(got.roots("/work", time.Time{}), []string{store}) {
		t.Fatalf("registered roots = %v, want the spec's %q", got.roots("/work", time.Time{}), store)
	}
	if !UnregisterSpec("codex") {
		t.Fatal("UnregisterSpec should report the registration it removed")
	}
	got, ok = adapterFor("codex")
	if !ok || got.suffix != builtin.suffix || !slices.Equal(got.roots("/work", time.Time{}), builtin.roots("/work", time.Time{})) {
		t.Fatalf("after unregister roots = %v, want the built-in %v", got.roots("/work", time.Time{}), builtin.roots("/work", time.Time{}))
	}
}

func TestWatcherErr(t *testing.T) {
	if err := (*Watcher)(nil).Err(); !errors.Is(err, ErrUnsupportedTool) {
		t.Fatalf("nil watcher Err = %v, want ErrUnsupportedTool", err)
	}
	if err := Watch("no-such-agent-anywhere", t.TempDir(), time.Now()).Err(); !errors.Is(err, ErrUnsupportedTool) {
		t.Fatalf("unreadable agent Err = %v, want ErrUnsupportedTool", err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	w := Watch("claude", t.TempDir(), time.Now())
	if w == nil {
		t.Fatal("claude should be watchable")
	}
	if err := w.Err(); err != nil {
		t.Fatalf("live watcher Err = %v, want nil", err)
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
	t.Cleanup(func() { UnregisterSpec("spaced") })
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
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
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
	if got := (Process{Tool: "notepad"}).Watch(since); got != nil {
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
	got := core.ExpandHome("~/rel")
	want := filepath.Join(home, "rel")
	if got != want {
		t.Errorf("core.ExpandHome(~/rel) = %q, want %q", got, want)
	}
	got = core.ExpandHome(`~\rel`)
	if got != want {
		t.Errorf(`core.ExpandHome(~\rel) = %q, want %q`, got, want)
	}
	if got := core.ExpandHome("~"); got != home {
		t.Errorf("core.ExpandHome(~) = %q, want %q", got, home)
	}
	if got := core.ExpandHome("rel"); got != "rel" {
		t.Errorf("core.ExpandHome(rel) = %q, want unchanged", got)
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
	t.Cleanup(func() { UnregisterSpec("inproject") })
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

// A transcript rewritten to a shorter length is re-read from its start, and
// the records already counted from those bytes must not be counted again.
func TestRewrittenTranscriptIsNotCountedTwice(t *testing.T) {
	store := withStore(t, "claude")
	work := t.TempDir()
	path := filepath.Join(store, "session.jsonl")

	w := Watch("claude", work, time.Now())
	append_(t, path, claudeLine(work, 100), claudeLine(work, 200))
	w.poll(nil)
	if got := w.Sample().Output; got != 300 {
		t.Fatalf("output tokens %d, want 300", got)
	}
	if err := os.WriteFile(path, []byte(claudeLine(work, 100)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.poll(nil)
	if got := w.Sample().Output; got != 100 {
		t.Fatalf("output tokens %d, want 100: a rewritten transcript was billed twice", got)
	}
}

// A cumulative transcript that existed at attach and is then rotated to a
// shorter file is a different session, one that began after this watch. Its
// first reading is real output, not a baseline, so it must be reported.
func TestRotatedCumulativeSessionIsNotBaselinedAway(t *testing.T) {
	store := withStore(t, "codex")
	work := t.TempDir()
	path := filepath.Join(store, "session.jsonl")

	// On disk at attach, so its first reading would be a baseline.
	append_(t, path, codexMeta(work), codexTokens(1000, 5000))
	w := Watch("codex", work, time.Now())
	w.poll(nil)

	// Rotated: shorter than what was there at attach, and a new session that
	// began after this watch.
	if err := os.WriteFile(path, []byte(codexMeta(work)+"\n"+codexTokens(40, 400)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.poll(nil)
	if got := w.Sample().Output; got != 40 {
		t.Fatalf("output tokens %d, want 40: a session created after attach was baselined away", got)
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
func TestFinalLineAtReaderBoundary(t *testing.T) {
	for _, size := range []int{appendReaderBytes - 1, appendReaderBytes, appendReaderBytes + 1, 2 * appendReaderBytes, maxLineBytes} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			store := withStore(t, "claude")
			work := t.TempDir()
			path := filepath.Join(store, "session.jsonl")
			w := Watch("claude", work, time.Now())
			line := claudeLine(work, 100)
			appendRaw(t, path, strings.Repeat(" ", size-len(line))+line)
			if got := w.Poll().Output; got != 100 {
				t.Fatalf("output tokens %d, want 100", got)
			}
			if got := w.Poll().Output; got != 100 {
				t.Fatalf("idle output tokens %d, want 100", got)
			}
			appendRaw(t, path, "\n"+claudeLine(work, 250))
			if got := w.Poll().Output; got != 350 {
				t.Fatalf("output tokens after append %d, want 350", got)
			}
		})
	}
}

func TestFinalLineParsedOnce(t *testing.T) {
	for _, mine := range []bool{true, false} {
		t.Run(strconv.FormatBool(mine), func(t *testing.T) {
			store := withStore(t, "claude")
			work := t.TempDir()
			cwd := work
			if !mine {
				cwd = t.TempDir()
			}
			path := filepath.Join(store, "session.jsonl")
			w := Watch("claude", work, time.Now())
			parse := w.ad.parse
			calls := 0
			w.ad.parse = func(line []byte) (values, string, bool) {
				calls++
				return parse(line)
			}
			line := claudeLine(cwd, 100)
			appendRaw(t, path, line)
			want := 0
			if mine {
				want = 100
			}
			for range 2 {
				if got := w.Poll().Output; got != want {
					t.Fatalf("output = %d, want %d", got, want)
				}
			}
			if calls != 1 {
				t.Fatalf("parsed one final record %d times", calls)
			}
			if w.offsets[path] != int64(len(line)) {
				t.Fatalf("offset = %d, want %d", w.offsets[path], len(line))
			}
		})
	}
}

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

// A transcript that was already on disk but had gone idle before the watcher
// attached is outside the recency window, so attach must still record where it
// ends. Otherwise the next append is read from byte zero and the whole earlier
// session is credited to this attach.
func TestIdleAtAttachTranscriptIsStillSkipped(t *testing.T) {
	store := withStore(t, "claude")
	work := t.TempDir()
	path := filepath.Join(store, "session.jsonl")
	append_(t, path, claudeLine(work, 100))

	old := time.Now().Add(-recencyWindow - time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	w := Watch("claude", work, time.Now())
	if _, tracked := w.offsets[path]; !tracked {
		t.Fatal("a transcript idle at attach was not seeded with its end offset")
	}

	append_(t, path, claudeLine(work, 50))
	w.poll(nil)
	if got := w.Sample().Output; got != 50 {
		t.Fatalf("output tokens %d, want 50: pre-attach history counted as this attach's usage", got)
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

// dropFile is the one place a path leaves every per-file map. A zstd carry it
// left behind would pin a tail buffer (up to maxLineBytes) for the rest of the
// run, and a transcript that reappeared at the same path would have bytes it
// never wrote prepended to its first record.
func TestDropFileReleasesZstdCarry(t *testing.T) {
	store := withStore(t, "dsh")
	work := t.TempDir()
	path := filepath.Join(store, "session.jsonl.zstd")

	w := Watch("dsh", work, time.Now())
	appendBytes(t, path, zstdFrame(t, dshHeader(work)+"\n"))
	appendBytes(t, path, zstdFrame(t, `{"type":"assistant/mes`)) // torn, unterminated
	w.poll(nil)
	if len(w.zstdCarry[path]) == 0 {
		t.Fatal("an unterminated tail was not carried over, so the drop below proves nothing")
	}

	w.dropFile(path)
	if carry, ok := w.zstdCarry[path]; ok {
		t.Fatalf("dropFile left a %d byte carry behind", len(carry))
	}
}

// A transcript that never opened leaves no stamp and no offset, so ageing the
// walk out had nothing to key the read-failure latch on: one entry per file a
// long --agents run failed to read, held for the rest of the run.
func TestReadFailureLatchAgesOut(t *testing.T) {
	store := withStore(t, "dsh")
	work := t.TempDir()

	w := Watch("dsh", work, time.Now())
	gone := filepath.Join(store, "unreadable.jsonl.z64")
	w.readFailed[gone] = true
	// A live transcript alongside it: ageing one out must not disturb the other.
	live := filepath.Join(store, "session.jsonl.zstd")
	appendBytes(t, live, zstdFrame(t, dshHeader(work)+"\n"))

	w.forgetIdle([]string{live})
	if _, latched := w.readFailed[gone]; latched {
		t.Fatal("the read-failure latch outlived the transcript it named")
	}
}

// The read-failure latch only clears on a read that commits, so a transcript
// that failed once and then aged out kept its path for the life of the process.
func TestDropFileReleasesReadFailedLatch(t *testing.T) {
	w := Watch("dsh", t.TempDir(), time.Now())
	path := filepath.Join(t.TempDir(), "session.jsonl")

	w.auditRead(path, errors.New("input/output error"))
	if !w.readFailed[path] {
		t.Fatal("the failure was not latched, so the drop below proves nothing")
	}

	w.dropFile(path)
	if latched, ok := w.readFailed[path]; ok {
		t.Fatalf("dropFile left the read-failure latch set: %v", latched)
	}
}

// A frame the zstd reader rejects carries no error of its own, and the audit
// reads a nil error as "the read committed". Left that way the same broken
// frame is reread on every poll and the session's usage is silently
// uncounted, which is indistinguishable from an idle session.
func TestZstdDecodeFailureLatchesReadFailed(t *testing.T) {
	store := withStore(t, "dsh")
	work := t.TempDir()
	path := filepath.Join(store, "session.jsonl.zstd")

	w := Watch("dsh", work, time.Now())
	appendBytes(t, path, zstdFrame(t, dshHeader(work)+"\n"))
	w.poll(nil)
	if w.offsets[path] == 0 {
		t.Fatal("the header frame was not read, so the decode failure below proves nothing")
	}

	// A well-formed frame whose content checksum no longer matches: the
	// reader walks the frame, then rejects it.
	broken := zstdFrame(t, dshUsageChunk(10, 0, 0)+"\n")
	broken[len(broken)-1] ^= 0xFF
	appendBytes(t, path, broken)
	w.poll(nil)

	if !w.readFailed[path] {
		t.Fatal("a rejected zstd frame cleared the read-failure latch instead of setting it")
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
	now := time.Now()
	cutoff := now.Add(-time.Hour)
	_, _ = listTranscripts(stale, []string{".jsonl"}, cutoff, now, true)
	key := rootListKey(stale, []string{".jsonl"})
	rootListMu.Lock()
	c, ok := rootLists[key]
	if !ok {
		rootListMu.Unlock()
		t.Fatal("expected the listing to be cached after a walk")
	}
	c.at = now.Add(-recencyWindow - time.Second)
	rootLists[key] = c
	rootListMu.Unlock()

	_, _ = listTranscripts(t.TempDir(), []string{".jsonl"}, cutoff, now, true)
	rootListMu.Lock()
	_, still := rootLists[key]
	rootListMu.Unlock()
	if still {
		t.Fatal("stale root listing still cached after another walk")
	}
}

// A walk in flight is the claim on its root, and a claim that outlives
// rescanEvery still holds it. Pruning the placeholder would let a second
// caller walk the same root concurrently, and the slower of the two would
// then publish over the newer listing.
func TestRootListPruneKeepsInflightClaim(t *testing.T) {
	rootListMu.Lock()
	saved := rootLists
	rootLists = map[string]rootListing{}
	rootListMu.Unlock()
	t.Cleanup(func() {
		rootListMu.Lock()
		rootLists = saved
		rootListMu.Unlock()
	})

	key := rootListKey(t.TempDir(), []string{".jsonl"})
	walk := make(chan struct{})
	rootListMu.Lock()
	rootLists[key] = rootListing{at: time.Now().Add(-2 * rescanEvery), walk: walk}
	pruneRootListsLocked(time.Now(), rescanEvery)
	_, still := rootLists[key]
	rootListMu.Unlock()

	if !still {
		t.Fatal("prune dropped a root listing whose walk is still in flight")
	}
}

// A parser that panics on a transcript must not strand the watcher. Both
// locks in read are released by defer, so a panic part-way through the file
// walk cannot leave pollMu held with no goroutine left to unlock it: the next
// Poll would block forever, and with it every caller sharing the tracker.
func TestPanickingParserDoesNotWedgeWatcher(t *testing.T) {
	store := withStore(t, "codex")
	adaptersMu.Lock()
	orig := adapters["codex"]
	ad := orig
	ad.sessionCwd = nil
	ad.parse = func([]byte) (values, string, bool) { panic("malformed record") }
	adapters["codex"] = ad
	adaptersMu.Unlock()
	t.Cleanup(func() {
		adaptersMu.Lock()
		adapters["codex"] = orig
		adaptersMu.Unlock()
	})

	w := Watch("codex", t.TempDir(), time.Now())
	path := filepath.Join(store, "session.jsonl")
	appendRaw(t, path, codexTokens(30, 60))

	func() {
		defer func() {
			if recover() == nil {
				t.Error("panicking parser did not reach the caller")
			}
		}()
		w.poll(nil)
	}()

	// The watcher is still usable: the next read has to take pollMu, walk the
	// store, and return. Before, it blocked forever on a lock the panic left
	// held, taking the tracker that owned it down with it.
	w.ad = orig
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Poll()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher wedged after a parser panic: pollMu never released")
	}
}

// A published sample is stamped from the injected clock, so a frozen or
// simulated run replays the same readings at the same instants. Callers build
// event ids from that stamp (see agentwatch.sampleID): a wall-clock one makes
// every replay look like fresh activity.
func TestSampleStampFollowsInjectedClock(t *testing.T) {
	store := withStore(t, "claude")
	work := t.TempDir()
	frozen := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	w := Watch("claude", work, time.Now())
	w.SetNow(func() time.Time { return frozen })

	append_(t, filepath.Join(store, "session.jsonl"), claudeLine(work, 21))
	if s := w.Poll(); s.At != frozen {
		t.Fatalf("sample At = %v, want injected %v", s.At, frozen)
	}

	// A nil clock restores the wall clock rather than panicking on every read.
	w.SetNow(nil)
	if s := w.Poll(); s.At.Before(frozen) {
		t.Fatalf("sample At = %v after SetNow(nil), want wall time at or after %v", s.At, frozen)
	}
}

// A definitions reload reaches a running watcher, which is the whole point of
// the generation counter: the adapter is re-derived, and the listing naming
// the previous generation's roots is dropped with it. Withdrawing the
// definition is the same event seen from the other side, and it used to be the
// one case nothing handled. refreshAdapter returned on an undefined spec, so
// the watcher kept the adapter the last load gave it and went on walking and
// billing a transcript store no spec claimed any more, for the life of the
// process. ResetDefinitions and a file that stops listing the agent both reach
// this path.
func TestWithdrawnDefinitionStopsTheWatcherReading(t *testing.T) {
	store := t.TempDir()
	work := t.TempDir()
	// A definition-derived watcher parses generically, so the record is the
	// bare shape parseGeneric reads: a cwd beside the counters.
	line := func(out int) string {
		return `{"cwd":` + jsonPath(work) + `,"output_tokens":` + strconv.Itoa(out) + `}`
	}
	addDef(t, "withdrawn", Spec{Roots: []string{store}})

	w := Watch("withdrawn", work, time.Now())
	if w == nil {
		t.Fatal("a defined agent with a root is readable")
	}
	append_(t, filepath.Join(store, "session.jsonl"), line(5))
	if s := w.Poll(); s.Output != 5 {
		t.Fatalf("poll before the withdrawal = %d, want 5", s.Output)
	}

	defsMu.Lock()
	delete(defs, "withdrawn")
	bumpDefsGen()
	defsMu.Unlock()

	// Growth after the withdrawal must not be read: the store is disowned.
	append_(t, filepath.Join(store, "session.jsonl"), line(9))
	after := w.Poll()
	if after.Output != 5 {
		t.Fatalf("poll after the withdrawal = %d, want the last published 5", after.Output)
	}
	if w.candidates() != nil {
		t.Fatal("a watcher whose definition was withdrawn still lists candidates")
	}

	// Reinstating the definition re-derives and reading resumes from the
	// retained read position, so the 9 written while it was withdrawn is
	// picked up along with the new 4: 5 + 9 + 4, not a restart at zero.
	addDef(t, "withdrawn", Spec{Roots: []string{store}})
	append_(t, filepath.Join(store, "session.jsonl"), line(4))
	if s := w.Poll(); s.Output != 18 {
		t.Fatalf("poll after the definition returned = %d, want 18", s.Output)
	}
}

// A watcher left running against a long-lived agent would otherwise hold
// per-file bookkeeping for every session file the agent ever wrote. The
// recency window releases a counted file once its last write is older than
// the window, and the cap releases the least recently written of the rest, so
// a store that keeps writing inside the window still cannot grow the maps
// without bound. A released transcript keeps its read position: the offset is
// seeded to the file's end, so a later append is growth and not a re-read.
func TestCountedFilesAreCapped(t *testing.T) {
	store := withStore(t, "claude")
	work := t.TempDir()
	w := Watch("claude", work, time.Now())
	if w == nil {
		t.Fatal("no claude adapter")
	}

	n := countedCap + 8
	for i := range n {
		append_(t, filepath.Join(store, "session"+strconv.Itoa(i)+".jsonl"), claudeLine(work, i+1))
	}
	w.cached, w.scanned = nil, time.Time{}
	w.Poll()
	if got := len(w.seen); got != n {
		t.Fatalf("%d files counted, want %d", got, n)
	}

	stepped := time.Now().Add(recencyWindow + time.Minute)
	w.SetNow(func() time.Time { return stepped })
	w.cached, w.scanned = nil, time.Time{}
	w.Poll()
	if got := len(w.seen); got > countedCap {
		t.Fatalf("%d counted files retained after ageing, want at most %d", got, countedCap)
	}

	var held string
	for i := range n {
		p := filepath.Join(store, "session"+strconv.Itoa(i)+".jsonl")
		if _, ok := w.seen[p]; !ok {
			held = p
			break
		}
	}
	if held == "" {
		t.Fatal("nothing was released, so the cap is not exercised")
	}
	w.SetNow(time.Now)
	before := w.Sample().Output
	append_(t, held, claudeLine(work, 5000))
	w.cached, w.scanned = nil, time.Time{}
	w.Poll()
	if got := w.Sample().Output; got < before+5000 {
		t.Fatalf("appending to a released transcript reported %d, want at least %d", got, before+5000)
	}
}

// A per-file-owner adapter judges a transcript by the working directory
// recorded in its header, and an own verdict outlives the recency window so a
// session that comes back is read as growth rather than from byte zero. Left
// unbounded that is one entry per session file ever judged ours, in five maps,
// for the life of the run, and a machine-wide watch sees every session on the
// host. The cap releases the oldest, and a released transcript keeps its skip
// position, so the release costs a session's idle remainder and not a
// double-counted history.
func TestOwnedIdleTranscriptsAreCapped(t *testing.T) {
	store := withStore(t, "copilot")
	work := t.TempDir()
	w := Watch("copilot", work, time.Now())
	if w == nil {
		t.Fatal("no copilot adapter")
	}

	n := stateCap + 8
	for i := range n {
		// No usage records, so a session judged ours carries no counts and
		// never draws on countedCap's budget: this is the bookkeeping the
		// owner cap alone bounds.
		copilotSession(t, store, "session"+strconv.Itoa(i), work)
	}
	w.cached, w.scanned = nil, time.Time{}
	w.Poll()
	if got := len(w.owner); got != n {
		t.Fatalf("%d own verdicts recorded, want %d", got, n)
	}
	if len(w.seen) != 0 {
		t.Fatalf("%d counted files for sessions with no records, want 0", len(w.seen))
	}

	old := time.Now().Add(-recencyWindow - time.Minute)
	for i := range n {
		p := filepath.Join(store, "session"+strconv.Itoa(i), "events.jsonl")
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	w.cached, w.scanned = nil, time.Time{}
	w.Poll()
	if got := len(w.owner); got > stateCap {
		t.Fatalf("%d own verdicts retained after ageing, want at most %d", got, stateCap)
	}
	if got := len(w.offsets); got > stateCap {
		t.Fatalf("%d skip positions retained after ageing, want at most %d", got, stateCap)
	}
	if got := len(w.stamps); got > stateCap {
		t.Fatalf("%d stamps retained after ageing, want at most %d", got, stateCap)
	}
	// A session still inside the cap keeps its skip position at the end the
	// read reached, so an append to it is growth and not a re-read.
	for path, off := range w.offsets {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("skip position kept for a transcript that is gone: %s", err)
		}
		if off != fi.Size() {
			t.Fatalf("skip position for %s is %d, want the file's end %d", filepath.Base(filepath.Dir(path)), off, fi.Size())
		}
	}
}
