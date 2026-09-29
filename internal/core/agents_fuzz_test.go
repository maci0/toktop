// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"math"
	"testing"
	"time"
)

// fuzzNow is the instant the summary is taken at. The events are placed
// relative to it, so a stamp far outside AgentRateWindow is a stale event
// rather than an event dated in the year the fuzzer's clock reads.
var fuzzNow = time.Unix(1_700_000_000, 0).UTC()

// FuzzSummarize drives the agent summary with arbitrary feeds. The events come
// off the unauthenticated ingest endpoint and out of the locally watched
// agents, and every field of one is a number or an instant a sender chose, so
// the summary is where a hostile event becomes a displayed rate: tokens over a
// span, tokens over a gap between events, and two views of the same totals.
//
// The invariants are what a frame draws. A rate is finite and never negative,
// since a NaN or a negative tok/s is painted into a cell the operator reads; the
// unattributed half is never larger than the whole it was taken from, since
// that double is the one that would count tokens twice; the two views agree
// about who is reporting; the rows are ordered, because the pane reads them in
// order; and the same feed always summarizes to the same numbers, since a map
// range decides the order the rows come out in.
func FuzzSummarize(f *testing.F) {
	for _, seed := range []struct {
		agent, via string
		off        int64
		prompt     int64
		output     int64
		thinking   int64
		span       int64
	}{
		{agent: "coder", output: 310, prompt: 4200},
		{agent: "coder", output: 310, prompt: 4200, off: 1}, // a second event, a gap apart
		{agent: "coder", output: 310, span: 1448, off: 1},
		{agent: "coder", output: 310, span: 1448, off: 2}, // one event with a span, one without
		{agent: "coder", via: "127.0.0.1:11434", output: 50},
		{agent: "coder", via: "127.0.0.1:11434", output: 50, off: 1},
		{agent: "coder", via: "engine-a", output: 1, off: 1},
		{agent: "coder", via: "engine-b", output: 1, off: 1}, // two engines: no label
		{agent: "coder", via: "engine-a", output: 1},
		{agent: "coder", via: "engine-a", output: 1, off: 1},
		{agent: "caf\u00e9", output: 10, off: 1},  // one name, two spellings
		{agent: "cafe\u0301", output: 10, off: 2}, // the same name decomposed
		{agent: "silent"},
		{agent: "stale", off: -int64(AgentRateWindow) - 1, output: 9999},
		{agent: "boundary", off: -int64(AgentRateWindow), output: 1},
		{agent: "huge", prompt: math.MaxInt64, output: math.MaxInt64, thinking: math.MaxInt64},
		{agent: "huge", prompt: math.MaxInt64, output: math.MaxInt64, thinking: math.MaxInt64, off: 1},
		{agent: "negative", prompt: -500, output: -999, thinking: -1, span: -5},
		{agent: "gapped", output: 7, off: math.MaxInt64},
		{agent: "gapped", output: 7, off: math.MinInt64},
		{agent: "spanned", output: 3, span: 1},
		{agent: "spanned", output: 3, span: math.MaxInt64},
		{agent: "spanned", output: 3, span: math.MinInt64},
	} {
		f.Add(seed.agent, seed.via, seed.off, seed.prompt, seed.output, seed.thinking, seed.span)
	}

	f.Fuzz(func(t *testing.T, agent, via string, off, prompt, output, thinking, span int64) {
		events := []AgentEvent{{
			At:             fuzzNow,
			Agent:          agent,
			PromptTokens:   prompt,
			OutputTokens:   output,
			ThinkingTokens: thinking,
			ViaEngine:      via,
			Span:           time.Duration(span),
		}, {
			// The second event is what makes a rate exist at all: a feed of
			// one event reports how much, not how fast.
			At:             fuzzNow.Add(time.Duration(off)),
			Agent:          agent,
			PromptTokens:   prompt,
			OutputTokens:   output,
			ThinkingTokens: thinking,
			ViaEngine:      via,
			Span:           time.Duration(span),
		}}

		sum := Summarize(events, fuzzNow)
		assertSummaryShape(t, "Rates", sum.Rates)
		assertSummaryShape(t, "Own", sum.Own)
		assertOwnWithinRates(t, sum)

		// The unattributed view exists only for an agent that reported
		// directly. One whose every in-window event went through an engine
		// contributes no own row, since the engine already counts those
		// tokens and a row would count them twice.
		if ownRow(sum.Own, agent) && directCount(events, agent) == 0 {
			t.Fatalf("%q has an unattributed row with no direct event in the window", agent)
		}

		// The engine label is claimed only when one engine named every
		// in-window event of that agent and none of them went direct.
		for _, r := range sum.Rates {
			if r.ViaEngine == "" {
				continue
			}
			if engines(events, agent) != r.ViaEngine {
				t.Fatalf("%q is labelled %q but its window names %q", agent, r.ViaEngine, engines(events, agent))
			}
		}

		again := Summarize(events, fuzzNow)
		if len(again.Rates) != len(sum.Rates) || len(again.Own) != len(sum.Own) {
			t.Fatalf("Summarize is not deterministic: %d/%d then %d/%d",
				len(sum.Rates), len(sum.Own), len(again.Rates), len(again.Own))
		}
		for i := range sum.Rates {
			if again.Rates[i] != sum.Rates[i] {
				t.Fatalf("row %d changed between two summaries of one feed:\n%+v\n%+v", i, sum.Rates[i], again.Rates[i])
			}
		}
		for i := range sum.Own {
			if again.Own[i] != sum.Own[i] {
				t.Fatalf("own row %d changed between two summaries:\n%+v\n%+v", i, sum.Own[i], again.Own[i])
			}
		}
	})
}

// FuzzSummarizeOrdering drives the retained feed itself, which the collector
// keeps newest-last and every consumer reads in that order, with events whose
// timestamps arrive in any order. The rows have to come out ordered by rate and
// then by name: a map range over the agents would otherwise shuffle the pane
// every frame, and a feed that is not sorted leaves the window trim keeping the
// wrong end of it.
func FuzzSummarizeOrdering(f *testing.F) {
	f.Add("a", int64(0), int64(0))
	f.Add("", int64(1), int64(-1))
	f.Add("café", int64(2), int64(1))

	f.Fuzz(func(t *testing.T, name string, step, tokens int64) {
		var feed []AgentEvent
		// Descending timestamps, so the feed the caller hands over is the
		// reverse of the order the events were produced in.
		const n = 6
		for i := range n {
			feed = append(feed, AgentEvent{
				At:           fuzzNow.Add(-time.Duration(i) * time.Duration(step)),
				Agent:        name,
				OutputTokens: tokens + int64(i),
				ViaEngine:    "",
			})
		}
		sum := Summarize(feed, fuzzNow)
		assertSummaryShape(t, "Rates", sum.Rates)
		assertSummaryShape(t, "Own", sum.Own)
		for i := 1; i < len(sum.Rates); i++ {
			prev, cur := sum.Rates[i-1], sum.Rates[i]
			if cur.TokPS > prev.TokPS || (cur.TokPS == prev.TokPS && cur.Agent > prev.Agent) {
				t.Fatalf("rates are out of order at %d: %+v then %+v", i, prev, cur)
			}
		}
	})
}

func assertSummaryShape(t *testing.T, which string, rows []AgentRate) {
	t.Helper()
	for i, r := range rows {
		for name, v := range map[string]float64{"TokPS": r.TokPS, "PromptPS": r.PromptPS} {
			if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("%s[%d] (%q) %s = %v, want a finite non-negative rate", which, i, r.Agent, name, v)
			}
		}
		if r.Tokens < 0 || r.Prompt < 0 || r.Thinking < 0 {
			t.Fatalf("%s[%d] (%q) holds a negative total: %+v", which, i, r.Agent, r)
		}
	}
}

func assertOwnWithinRates(t *testing.T, sum AgentSummary) {
	t.Helper()
	for _, o := range sum.Own {
		for _, r := range sum.Rates {
			if r.Agent != o.Agent {
				continue
			}
			if o.Tokens > r.Tokens || o.Prompt > r.Prompt || o.Thinking > r.Thinking {
				t.Fatalf("unattributed totals exceed the whole for %q: %+v against %+v", o.Agent, o, r)
			}
		}
	}
}

func ownRow(rows []AgentRate, agent string) bool {
	for _, r := range rows {
		if r.Agent == agent {
			return true
		}
	}
	return false
}

// directCount is how many in-window events of one agent named no engine.
func directCount(events []AgentEvent, agent string) int {
	canonical := CanonicalAgent(agent)
	n := 0
	for _, ev := range events {
		if ev.At.Before(fuzzNow.Add(-AgentRateWindow)) {
			continue
		}
		if CanonicalAgent(ev.Agent) != canonical {
			continue
		}
		if ev.ViaEngine == "" {
			n++
		}
	}
	return n
}

// engines is the set of engines named by one agent's in-window events, joined
// so a row labelled by it can be checked against it: one engine is the label's
// precondition, and two or more are the split that withdraws it.
func engines(events []AgentEvent, agent string) string {
	canonical := CanonicalAgent(agent)
	var found string
	for _, ev := range events {
		if ev.At.Before(fuzzNow.Add(-AgentRateWindow)) {
			continue
		}
		if CanonicalAgent(ev.Agent) != canonical || ev.ViaEngine == "" {
			continue
		}
		if found == "" {
			found = ev.ViaEngine
			continue
		}
		if found != ev.ViaEngine {
			return "two engines"
		}
	}
	return found
}
