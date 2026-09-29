// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package collector

import (
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// FuzzRecordAgent drives the collector's agent feed with arbitrary events.
// RecordAgent is the sink every untrusted POST to /v1/events reaches, and the
// one part of that path no fuzzer covers: the ingest harness decodes the wire
// against a recorder that answers from memory, so the clock-offset ledger, the
// id ledger and the retained window are exercised only by hand-written cases.
//
// The invariants are the ones a sender's answer depends on. A true answer must
// name an event the feed really holds, a false one must have changed nothing,
// and a replay of the same id must be refused without disturbing the feed, or
// a retried POST counts its tokens twice. The feed stays inside
// core.AgentHistoryLen and newest-last, the skew ledger stays bounded however
// many distinct agent names arrive, and every rate the summary derives from
// the result is finite and non-negative.
func FuzzRecordAgent(f *testing.F) {
	now := time.Unix(1_700_000_000, 0).UTC()
	for _, seed := range []struct {
		agent, id, via                          string
		atOff, prompt, output, thinking, spanMS int64
	}{
		{agent: "coder"},
		{agent: "coder", id: "turn-1", output: 310, prompt: 4200},
		{agent: "coder", id: "turn-1", output: 310, prompt: 4200}, // the replay
		{agent: "reviewer", id: "t2", output: 12, thinking: 7, spanMS: 1448},
		{agent: "coder", via: "127.0.0.1:11434", output: 50},
		{agent: "a\xffb", id: "\x00id", output: 1},
		{agent: "clau\x200bde", id: "cafe\u0301", output: 5},
		{agent: "x", atOff: math.MaxInt64, output: 1},
		{agent: "x", atOff: math.MinInt64, output: 1},
		{agent: "x", prompt: math.MaxInt64, output: math.MaxInt64, thinking: math.MaxInt64},
		{agent: "x", prompt: -5, output: -9, thinking: -1, spanMS: -3},
		{agent: "x", spanMS: math.MaxInt64, output: 1},
		{agent: "", id: "", via: "", output: 0},
		{agent: "x", atOff: -int64(core.AgentRateWindow) - 1, output: 9999},
	} {
		f.Add(seed.agent, seed.id, seed.via, seed.atOff, seed.prompt, seed.output, seed.thinking, seed.spanMS)
	}

	f.Fuzz(func(t *testing.T, agent, id, via string, atOff, prompt, output, thinking, spanMS int64) {
		ev := core.AgentEvent{
			At:             now.Add(time.Duration(atOff)),
			ID:             id,
			Agent:          agent,
			Kind:           core.AgentKindTool,
			PromptTokens:   prompt,
			OutputTokens:   output,
			ThinkingTokens: thinking,
			ViaEngine:      via,
			Note:           "n",
			Span:           time.Duration(spanMS) * time.Millisecond,
		}

		c := newFuzzCollector(t, now)
		before := c.feed()
		kept := c.RecordAgent(ev)
		after := c.feed()

		assertFeedInvariants(t, after)

		if !kept {
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("a refused event changed the feed: %d -> %d events", len(before), len(after))
			}
		} else if !feedHolds(after, ev) {
			t.Fatalf("RecordAgent answered kept for an event the feed does not hold: %+v", ev)
		}

		// The trust boundary the sender reads back: the same event again is a
		// replay, so it is refused and the feed is left exactly as it was.
		// Without an id the ledger cannot recognize it, and a second id-less
		// event is a second event, so the pair is only meaningful for an id.
		var replayed bool
		if kept && id != "" {
			replayed = c.RecordAgent(ev)
			again := c.feed()
			if replayed {
				t.Fatalf("a replayed id was stored twice: %q", id)
			}
			if !reflect.DeepEqual(after, again) {
				t.Fatal("a refused replay moved the feed")
			}
		}

		// Two collectors fed the same sequence must answer the same and leave
		// the same feed: the skew ledger and the id ledger are maps, and a walk
		// that took map order would read a replay differently on two runs of
		// one sender.
		twin := newFuzzCollector(t, now)
		twinKept := twin.RecordAgent(ev)
		if twinKept != kept {
			t.Fatalf("the same event was answered differently: %v then %v", kept, twinKept)
		}
		if kept && id != "" {
			if twinReplayed := twin.RecordAgent(ev); twinReplayed != replayed {
				t.Fatalf("the same replay was answered differently: %v then %v", replayed, twinReplayed)
			}
		}
		if !reflect.DeepEqual(after, twin.feed()) {
			t.Fatalf("the same events produced two feeds:\n%+v\n%+v", after, twin.feed())
		}

		assertSummaryInvariants(t, c.summary(now), ev)
	})
}

// FuzzRecordAgentFleet drives the skew ledger with a run of distinct agent
// names, which is the shape a peer that can reach --ingest actually sends: one
// POST per name, none of them the same agent. The ledger exists to bound that
// memory, so the count of names held is the property under test, and a feed
// that grows past the retained window is the other half: eviction has to keep
// up with arrival.
func FuzzRecordAgentFleet(f *testing.F) {
	f.Add("agent", int64(1), int64(0))
	f.Add("", int64(-1), int64(1000))
	f.Add("x", int64(0), int64(0))
	f.Add("\x00\xff", int64(math.MaxInt64), int64(math.MinInt64))

	f.Fuzz(func(t *testing.T, name string, skew, tokens int64) {
		now := time.Unix(1_700_000_000, 0).UTC()
		c := newFuzzCollector(t, now)
		// One past the cap, so the bound is reached and the sweep is what
		// keeps it.
		const fleet = maxAgentSkews + 64
		for i := range fleet {
			c.RecordAgent(core.AgentEvent{
				At:           now.Add(time.Duration(skew) * time.Millisecond),
				ID:           fmt.Sprintf("%s-%d", name, i),
				Agent:        fmt.Sprintf("%s#%d", name, i),
				OutputTokens: tokens,
			})
		}
		events := c.feed()
		assertFeedInvariants(t, events)
		if len(events) > core.AgentHistoryLen {
			t.Fatalf("fleet left %d events, cap %d", len(events), core.AgentHistoryLen)
		}
		// One row is appended per accepted reading, after the sweep has run,
		// so the ledger sits one past its cap at worst. Unbounded is what this
		// rules out: the endpoint authenticates nobody.
		if n := len(c.agentSkews); n > maxAgentSkews+1 {
			t.Fatalf("skew ledger holds %d names, cap %d", n, maxAgentSkews)
		}
		if n := len(c.agentSkewOrder); n > maxAgentSkews+1 {
			t.Fatalf("skew order holds %d rows, cap %d", n, maxAgentSkews)
		}
		if n := len(c.agentSkewLive); n > maxAgentSkews+1 {
			t.Fatalf("live skew map holds %d names, cap %d", n, maxAgentSkews)
		}
	})
}

// newFuzzCollector is a collector on a frozen clock, so the skew ledger, the
// id ledger and the retention window age against a fixed timeline and a replay
// of the same sequence answers the same way on every run.
func newFuzzCollector(t *testing.T, now time.Time) *Collector {
	t.Helper()
	c := New(nil, time.Second)
	c.SetNow(func() time.Time { return now })
	t.Cleanup(func() { c.SetNow(nil) })
	return c
}

// feed is a copy of the retained events, taken under the collector's lock the
// way emit takes them.
func (c *Collector) feed() []core.AgentEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]core.AgentEvent(nil), c.agents...)
}

func (c *Collector) summary(now time.Time) core.AgentSummary {
	c.mu.Lock()
	defer c.mu.Unlock()
	return core.Summarize(c.agents, now)
}

// feedHolds reports whether the feed holds an event the recorder answered kept
// for. The stored copy carries the skew correction, so it is matched on the
// fields a sender chose rather than on the whole struct: the id, the agent,
// the engine and the counts.
func feedHolds(feed []core.AgentEvent, ev core.AgentEvent) bool {
	for _, got := range feed {
		if got.ID != ev.ID || got.Agent != ev.Agent || got.ViaEngine != ev.ViaEngine {
			continue
		}
		if got.PromptTokens == ev.PromptTokens && got.OutputTokens == ev.OutputTokens &&
			got.ThinkingTokens == ev.ThinkingTokens && got.Span == ev.Span {
			return true
		}
	}
	return false
}

// assertFeedInvariants pins the two properties every consumer of the feed
// assumes: it holds no more than the retained window, and it reads newest-last
// under core.AgentCmp.
func assertFeedInvariants(t *testing.T, feed []core.AgentEvent) {
	t.Helper()
	if len(feed) > core.AgentHistoryLen {
		t.Fatalf("feed holds %d events, cap %d", len(feed), core.AgentHistoryLen)
	}
	for i := 1; i < len(feed); i++ {
		if core.AgentCmp(feed[i-1], feed[i]) > 0 {
			t.Fatalf("feed is out of order at %d: %+v then %+v", i, feed[i-1], feed[i])
		}
	}
	for i, ev := range feed {
		if ev.At.IsZero() {
			t.Fatalf("event %d kept a zero timestamp", i)
		}
	}
}

// assertSummaryInvariants checks the numbers the feed is rendered as. A
// wrapped token total or a rate divided by a zero span reaches the dashboard
// as a red row, so every rate is finite and non-negative and the unattributed
// half is never larger than the whole.
func assertSummaryInvariants(t *testing.T, sum core.AgentSummary, ev core.AgentEvent) {
	t.Helper()
	check := func(which string, rows []core.AgentRate) {
		for i, r := range rows {
			for name, v := range map[string]float64{"TokPS": r.TokPS, "PromptPS": r.PromptPS} {
				if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
					t.Fatalf("%s[%d] (%s) %s = %v from %+v", which, i, r.Agent, name, v, ev)
				}
			}
			if r.Tokens < 0 || r.Prompt < 0 || r.Thinking < 0 {
				t.Fatalf("%s[%d] (%s) holds a negative total: %+v", which, i, r.Agent, r)
			}
		}
	}
	check("rates", sum.Rates)
	check("own", sum.Own)
	for _, o := range sum.Own {
		for _, r := range sum.Rates {
			if r.Agent != o.Agent {
				continue
			}
			if o.Tokens > r.Tokens || o.Prompt > r.Prompt || o.Thinking > r.Thinking {
				t.Fatalf("unattributed totals exceed the whole for %q: %+v vs %+v", o.Agent, o, r)
			}
		}
	}
}
