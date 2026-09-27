package core

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// AgentNameField is the identity boundary: both producers of an agent event
// run it, so the feed cannot hold a name only one of them approved. Each of
// the four outcomes it decides has to be pinned separately, because the
// failure mode is a name that looks real and is not.
func TestAgentNameField(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"an ordinary name", "claude", "claude"},
		{"a hyphenated name", "prime-agent", "prime-agent"},
		{"a non-latin name", "модель", "модель"},
		{"a version suffix", "claude-4.5", "claude-4.5"},
		// Everything that sanitizing removes and leaves nothing behind.
		{"empty", "", AgentAnonymous},
		{"only control characters", "\x00\x1b\x7f", AgentAnonymous},
		{"only a bidi override", "\u202e", AgentAnonymous},
		// Stripped, not replaced: the clean part of the name survives, so a
		// display-name escape cannot erase a real agent from the feed.
		{"a CSI wrapper", "\x1b[1mclaude\x1b[0m", "claude"},
		// The feed renders a name into one cell of a row (ui/feed.go), and
		// a newline in a cell becomes a row of its own, so a name is
		// folded to one line here: "clau\nde" would print as two lines,
		// the second of which the dashboard reads as its own output. A
		// name of blanks is no name at all.
		{"whitespace only", "   ", AgentAnonymous},
		{"an embedded newline", "clau\nde", "clau de"},
		// A spoof: a name that is byte-for-byte one agent and reads as
		// another. There is no honest version of this string.
		{"a cyrillic es", "сlaude", AgentAnonymous},
		{"a greek omicron", "cοdex", AgentAnonymous},
		// Clamped, then judged: the cap is applied before the script check,
		// so a spoof padded past AgentNameMax is still caught.
		{"a long ordinary name", strings.Repeat("a", AgentNameMax+20), strings.Repeat("a", AgentNameMax)},
		{"a long spoofed name", "с" + strings.Repeat("a", AgentNameMax+20), AgentAnonymous},
		{"a name exactly at the cap", strings.Repeat("a", AgentNameMax), strings.Repeat("a", AgentNameMax)},
		{"a name one past the cap", strings.Repeat("a", AgentNameMax+1), strings.Repeat("a", AgentNameMax)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AgentNameField(tc.in)
			if got != tc.want {
				t.Fatalf("AgentNameField(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if got == "" {
				t.Error("AgentNameField returned an empty name; the feed would key on nothing")
			}
		})
	}
}

// ClampEventTokens is the single ceiling both event producers route through,
// so the boundaries are the test: the value at the ceiling, one past it, and
// the negative that a subtraction of two counters can produce.
func TestClampEventTokens(t *testing.T) {
	cases := []struct {
		name string
		in   int64
		want int64
	}{
		{"zero", 0, 0},
		{"an ordinary count", 1234, 1234},
		{"one below the ceiling", MaxEventTokens - 1, MaxEventTokens - 1},
		{"exactly the ceiling", MaxEventTokens, MaxEventTokens},
		{"one above the ceiling", MaxEventTokens + 1, 0},
		{"far above the ceiling", math.MaxInt64, 0},
		{"a negative count", -1, 0},
		{"the most negative count", math.MinInt64, 0},
	}
	for _, tc := range cases {
		if got := ClampEventTokens(tc.in); got != tc.want {
			t.Errorf("ClampEventTokens(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// Summarize.Rates drives every per-agent number the dashboard prints. A known
// event sequence must produce exactly the rate math the UI renders: tokens
// summed in the window, rate over the first-to-last span, single events
// reported without a rate, and via-engine rows kept in the list.
func TestSummarizeRates(t *testing.T) {
	now := time.Unix(1_700_000_100, 0)
	events := []AgentEvent{
		// Two-turn span: 80 out, 200 prompt and 25 thinking over one second.
		{At: now.Add(-2 * time.Second), Agent: "claude", OutputTokens: 40, PromptTokens: 100, ThinkingTokens: 10},
		{At: now.Add(-1 * time.Second), Agent: "claude", OutputTokens: 40, PromptTokens: 100, ThinkingTokens: 15},
		// Single event: tokens but no rate (no span to measure).
		{At: now.Add(-1 * time.Second), Agent: "codex", OutputTokens: 30},
		// Outside the window: excluded entirely.
		{At: now.Add(-2 * AgentRateWindow), Agent: "stale", OutputTokens: 9999},
		// Two events sharing one instant: a zero-length span, so there is
		// still no rate to report (and no division by zero).
		{At: now.Add(-1 * time.Second), Agent: "coincident", OutputTokens: 6},
		{At: now.Add(-1 * time.Second), Agent: "coincident", OutputTokens: 4},
		// Via-engine row with no tokens of its own: kept so the list shows
		// who is working.
		{At: now.Add(-1 * time.Second), Agent: "opencode", ViaEngine: "127.0.0.1:11434"},
		// No tokens and no via: not activity, dropped.
		{At: now.Add(-1 * time.Second), Agent: "idle"},
	}
	rates := Summarize(events, now).Rates
	if len(rates) != 4 {
		t.Fatalf("rates = %d entries, want 4 (claude, codex, coincident, opencode)", len(rates))
	}
	// Busiest first: claude (80 tok/s) before the three rows with no rate.
	if rates[0].Agent != "claude" {
		t.Fatalf("first = %q, want claude", rates[0].Agent)
	}
	if rates[0].TokPS != 80 || rates[0].PromptPS != 200 {
		t.Errorf("claude = %v/%v tok/s, want 80/200", rates[0].TokPS, rates[0].PromptPS)
	}
	if rates[0].Tokens != 80 || rates[0].Prompt != 200 {
		t.Errorf("claude totals = %v/%v, want 80/200", rates[0].Tokens, rates[0].Prompt)
	}
	if rates[0].Thinking != 25 {
		t.Errorf("claude thinking = %v, want 25", rates[0].Thinking)
	}
	for _, r := range rates[1:] {
		if r.TokPS != 0 || r.PromptPS != 0 {
			t.Errorf("%s got a rate with no measurable span: %v", r.Agent, r.TokPS)
		}
	}
	codex := rates[1]
	if codex.Agent != "codex" || codex.Tokens != 30 {
		t.Errorf("codex row = %+v, want 30 output tokens", codex)
	}
	// A zero-length span leaves the rate at zero, so the row sorts with the
	// other unrated agents (by name) rather than jumping the queue on the
	// strength of its token total.
	coincident := rates[2]
	if coincident.Agent != "coincident" || coincident.TokPS != 0 || coincident.Tokens != 10 {
		t.Errorf("coincident row = %+v, want 10 tokens at 0 tok/s", coincident)
	}
	if rates[3].Agent != "opencode" || rates[3].ViaEngine != "127.0.0.1:11434" {
		t.Errorf("via-engine row = %+v, want opencode via 127.0.0.1:11434", rates[3])
	}
}

// Header and chart totals must not count tokens an agent spent through a
// monitored engine (the engine already reports them), while unattributed
// agents still add in. Per event, not per agent: a switch mid-window keeps
// the unattributed slice.
func TestSummarizeUnattributedTotals(t *testing.T) {
	now := time.Unix(1_700_000_100, 0)
	events := []AgentEvent{
		{At: now.Add(-2 * time.Second), Agent: "claude", OutputTokens: 40, PromptTokens: 10},
		{At: now.Add(-1 * time.Second), Agent: "claude", OutputTokens: 40, PromptTokens: 10},
		{At: now.Add(-500 * time.Millisecond), Agent: "claude", ViaEngine: "127.0.0.1:11434"},
		{At: now.Add(-2 * time.Second), Agent: "codex", OutputTokens: 30, PromptTokens: 60},
		{At: now.Add(-1 * time.Second), Agent: "codex", OutputTokens: 30, PromptTokens: 60},
	}
	var out, in float64
	for _, r := range Summarize(events, now).Own {
		out += r.TokPS
		in += r.PromptPS
	}
	// claude own events end at -1s (the via event is excluded): 80 out / 20
	// in over 1s. codex: 60 out / 120 in over 1s.
	if math.Abs(out-140) > 1e-9 || math.Abs(in-140) > 1e-9 {
		t.Errorf("own rates = %v out / %v in, want 140/140", out, in)
	}
}

// The via label on a rate row is a claim that the engine already counts that
// row's tokens, so it holds only when every in-window event of the agent went
// through the same engine. Two cases withdraw it, and both used to be decided
// by whichever event the walk reached last, which the feed's lack of ordering
// made arbitrary: an agent that spent some tokens direct, and an agent that
// named two engines. Neither is walked twice, so an order-dependence cannot be
// pinned by running the fixture in two orders.
func TestSummarizeViaLabelNeedsTheWholeRow(t *testing.T) {
	now := time.Unix(1_700_000_100, 0)
	cases := []struct {
		name   string
		events []AgentEvent
		want   string
	}{
		{
			"every event through one engine",
			[]AgentEvent{
				{At: now.Add(-2 * time.Second), Agent: "routed", ViaEngine: "127.0.0.1:8000"},
				{At: now.Add(-1 * time.Second), Agent: "routed", ViaEngine: "127.0.0.1:8000"},
			},
			"127.0.0.1:8000",
		},
		{
			// The direct event is walked last here, so the label came out
			// right by accident; walked first it did not. The totals count
			// these tokens, so the row may not claim the engine did.
			"one event direct, direct walked first",
			[]AgentEvent{
				{At: now.Add(-2 * time.Second), Agent: "mixed", OutputTokens: 40},
				{At: now.Add(-1 * time.Second), Agent: "mixed", ViaEngine: "127.0.0.1:8000"},
			},
			"",
		},
		{
			"one event direct, direct walked last",
			[]AgentEvent{
				{At: now.Add(-2 * time.Second), Agent: "mixed", ViaEngine: "127.0.0.1:8000"},
				{At: now.Add(-1 * time.Second), Agent: "mixed", OutputTokens: 40},
			},
			"",
		},
		{
			"two engines named in one window",
			[]AgentEvent{
				{At: now.Add(-2 * time.Second), Agent: "hopped", ViaEngine: "127.0.0.1:8000"},
				{At: now.Add(-1 * time.Second), Agent: "hopped", ViaEngine: "127.0.0.1:11434"},
			},
			"",
		},
		{
			"no engine at all",
			[]AgentEvent{
				{At: now.Add(-2 * time.Second), Agent: "direct", OutputTokens: 40},
				{At: now.Add(-1 * time.Second), Agent: "direct", OutputTokens: 40},
			},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rates := Summarize(tc.events, now).Rates
			if len(rates) != 1 {
				t.Fatalf("rates = %+v, want one row", rates)
			}
			if got := rates[0].ViaEngine; got != tc.want {
				t.Errorf("via = %q, want %q", got, tc.want)
			}
		})
	}
}

// Summarize returns both views in one walk, and a frame must be able to
// account the whole feed in a single pass. The expected numbers here are
// derived by hand from the fixture, not from another Summarize call:
// comparing the two views against each other would compare Summarize with
// itself and pass for any implementation of it.
func TestSummarizeAccountsEveryAgentInOnePass(t *testing.T) {
	now := time.Unix(1_700_000_100, 0)
	events := []AgentEvent{
		{At: now.Add(-2 * time.Second), Agent: "claude", OutputTokens: 40, PromptTokens: 10},
		{At: now.Add(-1 * time.Second), Agent: "claude", OutputTokens: 40, PromptTokens: 10},
		{At: now.Add(-500 * time.Millisecond), Agent: "claude", ViaEngine: "127.0.0.1:11434"},
		{At: now.Add(-2 * time.Second), Agent: "codex", OutputTokens: 30, PromptTokens: 60},
		{At: now.Add(-1 * time.Second), Agent: "codex", OutputTokens: 30, PromptTokens: 60},
		// One event only: reports tokens, no rate.
		{At: now, Agent: "dsh", OutputTokens: 7, PromptTokens: 3},
		// Outside the window entirely.
		{At: now.Add(-2 * AgentRateWindow), Agent: "stale", OutputTokens: 999},
		// An agent whose every in-window event went through an engine has no
		// unattributed slice at all, so it must not appear in Own.
		{At: now.Add(-2 * time.Second), Agent: "routed", ViaEngine: "127.0.0.1:8000", OutputTokens: 11},
		{At: now.Add(-1 * time.Second), Agent: "routed", ViaEngine: "127.0.0.1:8000", OutputTokens: 11},
	}

	sum := Summarize(events, now)

	// Rates spans first to last event, not the window: claude runs from -2s
	// to -500ms, so 80 out over 1.5s. routed counts its 11+11 over 1s.
	//
	// claude carries no via label: two of its three events went direct, so the
	// engine did not see those tokens and the totals do count them. routed,
	// whose every event went through 127.0.0.1:8000, is the row that may name
	// one.
	wantRates := []struct {
		agent     string
		tokPS     float64
		promptPS  float64
		tokens    int64
		last      time.Duration
		viaEngine string
	}{
		{"codex", 60, 120, 60, -time.Second, ""},
		{"claude", 80 / 1.5, 20 / 1.5, 80, -500 * time.Millisecond, ""},
		{"routed", 22, 0, 22, -time.Second, "127.0.0.1:8000"},
		// A single event has no span to divide by, so it reports tokens only.
		{"dsh", 0, 0, 7, 0, ""},
	}
	if len(sum.Rates) != len(wantRates) {
		t.Fatalf("Rates = %+v, want %d entries", sum.Rates, len(wantRates))
	}
	for i, w := range wantRates {
		got := sum.Rates[i]
		if got.Agent != w.agent {
			t.Fatalf("Rates[%d] = %q, want %q (order is busiest first)", i, got.Agent, w.agent)
		}
		if math.Abs(got.TokPS-w.tokPS) > 1e-9 || math.Abs(got.PromptPS-w.promptPS) > 1e-9 {
			t.Errorf("Rates[%d] %q = %v tok/s, %v prompt/s; want %v, %v",
				i, w.agent, got.TokPS, got.PromptPS, w.tokPS, w.promptPS)
		}
		if got.Tokens != w.tokens {
			t.Errorf("Rates[%d] %q tokens = %d, want %d", i, w.agent, got.Tokens, w.tokens)
		}
		if want := now.Add(w.last); !got.Last.Equal(want) {
			t.Errorf("Rates[%d] %q last = %v, want %v", i, w.agent, got.Last, want)
		}
		if got.ViaEngine != w.viaEngine {
			t.Errorf("Rates[%d] %q via = %q, want %q", i, w.agent, got.ViaEngine, w.viaEngine)
		}
	}

	// Own counts only the events with no engine, so claude's own span is the
	// two direct turns at -2s and -1s (80 out over 1s), not the -500ms routed
	// event that widened the Rates span. routed has no direct event at all.
	wantOwn := []struct {
		agent    string
		tokPS    float64
		promptPS float64
		tokens   int64
	}{
		{"claude", 80, 20, 80},
		{"codex", 60, 120, 60},
		{"dsh", 0, 0, 7},
	}
	if len(sum.Own) != len(wantOwn) {
		t.Fatalf("Own = %+v, want %d entries (routed has no direct events)", sum.Own, len(wantOwn))
	}
	var out, in float64
	for i, w := range wantOwn {
		got := sum.Own[i]
		if got.Agent != w.agent {
			t.Fatalf("Own[%d] = %q, want %q", i, got.Agent, w.agent)
		}
		if math.Abs(got.TokPS-w.tokPS) > 1e-9 || math.Abs(got.PromptPS-w.promptPS) > 1e-9 {
			t.Errorf("Own[%d] %q = %v tok/s, %v prompt/s; want %v, %v",
				i, w.agent, got.TokPS, got.PromptPS, w.tokPS, w.promptPS)
		}
		if got.Tokens != w.tokens {
			t.Errorf("Own[%d] %q tokens = %d, want %d", i, w.agent, got.Tokens, w.tokens)
		}
		if got.ViaEngine != "" {
			t.Errorf("Own[%d] %q carries a via engine %q; an unattributed rate has none", i, w.agent, got.ViaEngine)
		}
		out += got.TokPS
		in += got.PromptPS
	}
	if math.Abs(out-140) > 1e-9 || math.Abs(in-140) > 1e-9 {
		t.Errorf("own totals = %v out / %v in, want 140/140", out, in)
	}
}

// A frame redraws once a second against a feed that retains AgentHistoryLen
// events, so the whole accounting has to be one walk. Two walks of the same
// slice cost two maps and two sorts; the UI reuses a single summary.
func TestSummarizeAllocBudget(t *testing.T) {
	now := time.Unix(1_700_000_100, 0)
	events := make([]AgentEvent, AgentHistoryLen)
	for i := range events {
		events[i] = AgentEvent{
			At: now.Add(time.Duration(i) * time.Second), ID: fmt.Sprint(i),
			Agent:        []string{"claude", "codex", "dsh", "aider"}[i%4],
			PromptTokens: int64(100 + i), OutputTokens: int64(20 + i%97),
		}
	}
	// One pass needs: the accumulator map, one acc per distinct agent, and
	// the two result slices. Measured at 6 on a 4-agent feed; the ceiling
	// catches a regression back to one walk per consumer.
	const budget = 12
	if got := testing.AllocsPerRun(50, func() { _ = Summarize(events, now) }); got > budget {
		t.Errorf("Summarize allocates %.0f objects over a %d-event feed, budget %d; "+
			"the frame must account the feed in one walk", got, len(events), budget)
	}
}

func BenchmarkSummarize(b *testing.B) {
	now := time.Unix(1_700_000_100, 0)
	events := make([]AgentEvent, AgentHistoryLen)
	for i := range events {
		events[i] = AgentEvent{
			At: now.Add(time.Duration(i) * time.Second), ID: fmt.Sprint(i),
			Agent:        []string{"claude", "codex", "dsh", "aider"}[i%4],
			PromptTokens: int64(100 + i), OutputTokens: int64(20 + i%97),
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = Summarize(events, now)
	}
}

// The feed is not time-ordered: the ingest endpoint accepts any ts, and a
// producer's clock can step. A rate's span runs first to last event, so
// taking the last event walked instead of the latest in time turns one
// out-of-order stamp into a span that is short, or negative and dropped.
func TestSummarizeSpanIsFirstToLastInTimeNotWalkOrder(t *testing.T) {
	now := time.Unix(1_700_000_100, 0)
	// Same three events as the ordered fixture, delivered newest first. The
	// span is 2s either way, so the rate is 80 out / 200 prompt tok/s.
	events := []AgentEvent{
		{At: now.Add(-1 * time.Second), Agent: "claude", OutputTokens: 40, PromptTokens: 100, ThinkingTokens: 15},
		{At: now.Add(-2 * time.Second), Agent: "claude", OutputTokens: 40, PromptTokens: 100, ThinkingTokens: 10},
		{At: now.Add(-500 * time.Millisecond), Agent: "claude", OutputTokens: 0, PromptTokens: 0, ViaEngine: "127.0.0.1:11434"},
	}
	sum := Summarize(events, now)
	if len(sum.Rates) != 1 {
		t.Fatalf("rates = %d entries, want 1", len(sum.Rates))
	}
	r := sum.Rates[0]
	// Every event counts toward the span, so it runs -2s to -0.5s: 1.5s.
	if math.Abs(r.TokPS-80/1.5) > 1e-9 || math.Abs(r.PromptPS-200/1.5) > 1e-9 {
		t.Errorf("claude = %v/%v tok/s, want %v/%v", r.TokPS, r.PromptPS, 80/1.5, 200/1.5)
	}
	// Last is the latest event in time (the via event at -0.5s), not the
	// last one walked (-2s).
	if want := now.Add(-500 * time.Millisecond); !r.Last.Equal(want) {
		t.Errorf("claude Last = %v, want the latest event at %v", r.Last, want)
	}
	// Unattributed only: the via event is excluded from Own, leaving the
	// same two events and the same 2s span.
	if len(sum.Own) != 1 {
		t.Fatalf("own = %d entries, want 1", len(sum.Own))
	}
	if o := sum.Own[0]; o.TokPS != 80 || o.PromptPS != 200 {
		t.Errorf("claude own = %v/%v tok/s, want 80/200", o.TokPS, o.PromptPS)
	}
}
