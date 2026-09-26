// Per-agent throughput, measured from the agent events the collector holds.
//
// The engines report their own rate; agents do not, so it is computed here
// from what they spent and when. Nothing is extrapolated: an agent with a
// single reading has no rate yet, and one that has gone quiet falls out of
// the window (and the list) rather than holding its last value forever.
//
// An event with ViaEngine set is this agent's share of an engine already on
// the dashboard. It still appears in the list (who is working) but is left
// out of header and chart totals so those tokens are not counted twice.

package core

import (
	"cmp"
	"slices"
	"time"

	"golang.org/x/text/unicode/norm"
)

// AgentRate is one agent's measured throughput.
type AgentRate struct {
	Agent     string
	TokPS     float64
	PromptPS  float64
	Tokens    int64
	Prompt    int64
	Thinking  int64
	Last      time.Time
	ViaEngine string
}

// AgentRates summarizes the recent event stream, busiest first.
func AgentRates(events []AgentEvent, now time.Time) []AgentRate {
	return agentRatesFiltered(events, now, false)
}

func agentRatesFiltered(events []AgentEvent, now time.Time, ownOnly bool) []AgentRate {
	if len(events) == 0 {
		return nil
	}
	type acc struct {
		tokens   int64
		prompt   int64
		thinking int64
		first    time.Time
		last     time.Time
		via      string
		n        int
	}
	by := map[string]*acc{}
	cutoff := now.Add(-AgentRateWindow)
	for _, ev := range events {
		if ownOnly && ev.ViaEngine != "" {
			continue
		}
		if ev.At.Before(cutoff) {
			continue
		}
		if ev.OutputTokens <= 0 && ev.PromptTokens <= 0 && ev.ThinkingTokens <= 0 && ev.ViaEngine == "" {
			continue
		}
		agent := norm.NFC.String(ev.Agent)
		a, ok := by[agent]
		if !ok {
			a = &acc{first: ev.At}
			by[agent] = a
		}
		a.tokens += ev.OutputTokens
		a.prompt += ev.PromptTokens
		a.thinking += ev.ThinkingTokens
		a.last = ev.At
		a.via = ev.ViaEngine
		a.n++
	}

	out := make([]AgentRate, 0, len(by))
	for name, a := range by {
		r := AgentRate{
			Agent:     name,
			Tokens:    a.tokens,
			Prompt:    a.prompt,
			Thinking:  a.thinking,
			Last:      a.last,
			ViaEngine: a.via,
		}
		// A rate needs a span. One event says how much, not how fast, so it
		// reports tokens without a rate.
		if span := a.last.Sub(a.first).Seconds(); a.n > 1 && span > 0 {
			r.TokPS = float64(a.tokens) / span
			r.PromptPS = float64(a.prompt) / span
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b AgentRate) int {
		if c := cmp.Compare(b.TokPS, a.TokPS); c != 0 {
			return c
		}
		return cmp.Compare(a.Agent, b.Agent)
	})
	return out
}

// AgentOwnTokPS is the output/prompt rate of tokens not already in an
// engine's totals. Skip is per event, not per agent: an agent that
// connects to (or leaves) a monitored engine mid-window still contributes
// the unattributed slice. The per-agent row keeps the last ViaEngine so
// it shows who they are talking to now.
func AgentOwnTokPS(events []AgentEvent, now time.Time) (outPS, inPS float64) {
	for _, r := range agentRatesFiltered(events, now, true) {
		outPS += r.TokPS
		inPS += r.PromptPS
	}
	return
}
