package core

import (
	"math"
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
		// Two-turn span: 80 out and 200 prompt over one second.
		{At: now.Add(-2 * time.Second), Agent: "claude", OutputTokens: 40, PromptTokens: 100},
		{At: now.Add(-1 * time.Second), Agent: "claude", OutputTokens: 40, PromptTokens: 100},
		// Single event: tokens but no rate (no span to measure).
		{At: now.Add(-1 * time.Second), Agent: "codex", OutputTokens: 30},
		// Outside the window: excluded entirely.
		{At: now.Add(-2 * AgentRateWindow), Agent: "stale", OutputTokens: 9999},
		// Via-engine row with no tokens of its own: kept so the list shows
		// who is working.
		{At: now.Add(-1 * time.Second), Agent: "opencode", ViaEngine: "127.0.0.1:11434"},
		// No tokens and no via: not activity, dropped.
		{At: now.Add(-1 * time.Second), Agent: "idle"},
	}
	rates := AgentRates(events, now)
	if len(rates) != 3 {
		t.Fatalf("rates = %d entries, want 3 (claude, codex, opencode)", len(rates))
	}
	// Busiest first: claude (80 tok/s) before codex and opencode (no rate).
	if rates[0].Agent != "claude" {
		t.Fatalf("first = %q, want claude", rates[0].Agent)
	}
	if rates[0].TokPS != 80 || rates[0].PromptPS != 200 {
		t.Errorf("claude = %v/%v tok/s, want 80/200", rates[0].TokPS, rates[0].PromptPS)
	}
	if rates[0].Tokens != 80 || rates[0].Prompt != 200 {
		t.Errorf("claude totals = %v/%v, want 80/200", rates[0].Tokens, rates[0].Prompt)
	}
	for _, r := range rates[1:] {
		if r.TokPS != 0 || r.PromptPS != 0 {
			t.Errorf("%s got a rate from a single event: %v", r.Agent, r.TokPS)
		}
	}
	codex := rates[1]
	if codex.Agent != "codex" || codex.Tokens != 30 {
		t.Errorf("codex row = %+v, want 30 output tokens", codex)
	}
	if rates[2].Agent != "opencode" || rates[2].ViaEngine != "127.0.0.1:11434" {
		t.Errorf("via-engine row = %+v, want opencode via 127.0.0.1:11434", rates[2])
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
