package core

import (
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"
)

// AgentRates drives every per-agent number the dashboard prints. A known
// event sequence must produce exactly the rate math the UI renders: tokens
// summed in the window, rate over the first-to-last span, single events
// reported without a rate, and via-engine rows kept in the list.
func TestAgentRates(t *testing.T) {
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
	rates := AgentRates(events, now)
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
func TestAgentOwnTokPS(t *testing.T) {
	now := time.Unix(1_700_000_100, 0)
	events := []AgentEvent{
		{At: now.Add(-2 * time.Second), Agent: "claude", OutputTokens: 40, PromptTokens: 10},
		{At: now.Add(-1 * time.Second), Agent: "claude", OutputTokens: 40, PromptTokens: 10},
		{At: now.Add(-500 * time.Millisecond), Agent: "claude", ViaEngine: "127.0.0.1:11434"},
		{At: now.Add(-2 * time.Second), Agent: "codex", OutputTokens: 30, PromptTokens: 60},
		{At: now.Add(-1 * time.Second), Agent: "codex", OutputTokens: 30, PromptTokens: 60},
	}
	out, in := AgentOwnTokPS(events, now)
	// claude own events end at -1s (the via event is excluded): 80 out / 20
	// in over 1s. codex: 60 out / 120 in over 1s.
	if math.Abs(out-140) > 1e-9 || math.Abs(in-140) > 1e-9 {
		t.Errorf("own rates = %v out / %v in, want 140/140", out, in)
	}
}

// Summarize replaces the pair of independent walks the frame used to run,
// so it has to agree with them exactly on both halves, and a frame must be
// able to account the whole feed in a single pass.
func TestSummarizeMatchesSeparateWalks(t *testing.T) {
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

	wantRates := AgentRates(events, now)
	if !reflect.DeepEqual(sum.Rates, wantRates) {
		t.Errorf("Summarize.Rates = %+v, AgentRates = %+v", sum.Rates, wantRates)
	}
	wantOut, wantIn := AgentOwnTokPS(events, now)
	var out, in float64
	for _, r := range sum.Own {
		out += r.TokPS
		in += r.PromptPS
	}
	if math.Abs(out-wantOut) > 1e-9 || math.Abs(in-wantIn) > 1e-9 {
		t.Errorf("Summarize.Own totals = %v out / %v in, AgentOwnTokPS = %v/%v", out, in, wantOut, wantIn)
	}
	if math.Abs(out-140) > 1e-9 || math.Abs(in-140) > 1e-9 {
		t.Errorf("own rates = %v out / %v in, want 140/140", out, in)
	}
	for _, r := range sum.Own {
		if r.Agent == "routed" {
			t.Error("Own lists an agent whose every event went through an engine")
		}
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
