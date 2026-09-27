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

// CanonicalAgent is an agent's identity: its name composed to NFC, so the two
// spellings of one name (a macOS-typed decomposed "café" beside a precomposed
// one from a JSON sender) are one agent. Every consumer that groups, counts or
// names agents keys on this; keying on the raw name instead lets one agent
// show up as two rows, or be counted twice.
func CanonicalAgent(name string) string { return norm.NFC.String(name) }

// Caps on the free-form text fields of an AgentEvent. One set of constants
// because the two producers of an event (the ingest endpoint and the local
// process watcher) apply them independently, and a cap that drifted between
// them would let one producer keep a field the other truncates.
const (
	AgentNameMax  = 64
	AgentIDMax    = 128
	AgentModelMax = 128
	AgentViaMax   = 128
	AgentNoteMax  = 512
	AgentKindMax  = 24
)

// AgentAnonymous is the name an event carries when its agent field arrives
// empty or unusable.
const AgentAnonymous = "anonymous"

// AgentNameField normalizes an event's agent name: sanitize, cap, then
// collapse a mixed-script spoof ("сlaude" for "claude") and an empty result
// to AgentAnonymous. Both producers run it, so the feed cannot hold a name
// only one of them approved.
func AgentNameField(s string) string {
	s = ClampField(SanitizeText(s), AgentNameMax)
	if MixedScriptIdentity(s) {
		return AgentAnonymous
	}
	if s == "" {
		return AgentAnonymous
	}
	return s
}

// AgentSummary is the agent feed accounted once: every agent's rate, plus
// the rates of only the tokens no engine already reports.
//
// The dashboard needs both halves in the same frame (the agent list and
// the header aggregate), and the feed retains 512 events that each half
// would otherwise walk, group and normalize on its own.
type AgentSummary struct {
	Rates []AgentRate // every agent reporting, engine-attributed events included
	Own   []AgentRate // the same, counting only unattributed tokens
}

// Summarize walks the feed once and returns both views. Own is empty for an
// agent whose every in-window event went through an engine, since that
// agent contributes nothing to the unattributed totals.
func Summarize(events []AgentEvent, now time.Time) AgentSummary {
	if len(events) == 0 {
		return AgentSummary{}
	}
	type acc struct {
		tokens   int64
		prompt   int64
		thinking int64
		first    time.Time
		last     time.Time
		via      string
		n        int
		// Unattributed half. Kept per event rather than per agent: an
		// agent that connects to (or leaves) a monitored engine mid-window
		// contributes the slice that went direct.
		ownTokens int64
		ownPrompt int64
		ownFirst  time.Time
		ownLast   time.Time
		ownN      int
	}
	by := map[string]*acc{}
	cutoff := now.Add(-AgentRateWindow)
	for _, ev := range events {
		if ev.At.Before(cutoff) {
			continue
		}
		if ev.OutputTokens <= 0 && ev.PromptTokens <= 0 && ev.ThinkingTokens <= 0 && ev.ViaEngine == "" {
			continue
		}
		agent := CanonicalAgent(ev.Agent)
		a, ok := by[agent]
		if !ok {
			a = &acc{first: ev.At}
			by[agent] = a
		}
		a.tokens = SatAddPos(a.tokens, ev.OutputTokens)
		a.prompt = SatAddPos(a.prompt, ev.PromptTokens)
		a.thinking = SatAddPos(a.thinking, ev.ThinkingTokens)
		a.last = ev.At
		a.via = ev.ViaEngine
		a.n++
		if ev.ViaEngine == "" {
			if a.ownN == 0 {
				a.ownFirst = ev.At
			}
			a.ownTokens = SatAddPos(a.ownTokens, ev.OutputTokens)
			a.ownPrompt = SatAddPos(a.ownPrompt, ev.PromptTokens)
			a.ownLast = ev.At
			a.ownN++
		}
	}

	sum := AgentSummary{
		Rates: make([]AgentRate, 0, len(by)),
		Own:   make([]AgentRate, 0, len(by)),
	}
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
		sum.Rates = append(sum.Rates, r)
		if a.ownN == 0 {
			continue
		}
		o := AgentRate{
			Agent:  name,
			Tokens: a.ownTokens,
			Prompt: a.ownPrompt,
			Last:   a.ownLast,
		}
		if span := a.ownLast.Sub(a.ownFirst).Seconds(); a.ownN > 1 && span > 0 {
			o.TokPS = float64(a.ownTokens) / span
			o.PromptPS = float64(a.ownPrompt) / span
		}
		sum.Own = append(sum.Own, o)
	}
	sortRates(sum.Rates)
	sortRates(sum.Own)
	return sum
}

func sortRates(r []AgentRate) {
	slices.SortFunc(r, func(a, b AgentRate) int {
		if c := cmp.Compare(b.TokPS, a.TokPS); c != 0 {
			return c
		}
		return cmp.Compare(a.Agent, b.Agent)
	})
}
