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
	Agent string
	TokPS float64
	// PromptPS is the input rate. Thinking is the reasoning subset of Tokens,
	// a breakdown of it rather than an extra, and both views of the summary
	// carry it: an unattributed row reports the same three totals its
	// attributed sibling does, over the same events minus the routed ones.
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

// AgentNameField normalizes an event's agent name: sanitize, fold to one
// line, cap, then collapse a mixed-script spoof ("сlaude" for "claude") and an empty result
// to AgentAnonymous. Both producers run it, so the feed cannot hold a name
// only one of them approved.
func AgentNameField(s string) string {
	s = ClampField(SingleLine(s), AgentNameMax)
	if s == "" || mixedScriptIdentity(s) {
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

// agentAcc is one agent's running totals over the summary window.
type agentAcc struct {
	tokens   int64
	prompt   int64
	thinking int64
	first    time.Time
	last     time.Time
	// via is the engine every attributed event named, and viaSplit records
	// that the agent named more than one across the window. Neither is read
	// off the last event walked: the feed is not time-ordered, so that made
	// the label depend on arrival order.
	via      string
	viaSplit bool
	n        int
	// Unattributed half. Kept per event rather than per agent: an
	// agent that connects to (or leaves) a monitored engine mid-window
	// contributes the slice that went direct.
	ownTokens   int64
	ownPrompt   int64
	ownThinking int64
	ownFirst    time.Time
	ownLast     time.Time
	ownN        int
	// span is the sum of event durations the sender reported. spanned counts
	// those events. A rate uses the durations only when every event in the
	// window brought one: a grok turn is a single event minutes after the
	// last, and the gap between them is not how long the model ran.
	span       time.Duration
	spanned    int
	ownSpan    time.Duration
	ownSpanned int
}

// viaEngine is the engine a rate row may name: one every in-window event of
// the agent went through. A direct event in the window withdraws the claim,
// because the engine did not see those tokens and the totals below count them,
// and so does a window that names two engines, because there is no single one
// to point at. An agent in either case reports no label, which every consumer
// already renders as the row's own measured rate.
func (a *agentAcc) viaEngine() string {
	if a.ownN > 0 || a.viaSplit {
		return ""
	}
	return a.via
}

// Summarize walks the feed once and returns both views. Own is empty for an
// agent whose every in-window event went through an engine, since that
// agent contributes nothing to the unattributed totals.
func Summarize(events []AgentEvent, now time.Time) AgentSummary {
	if len(events) == 0 {
		return AgentSummary{}
	}
	by := map[string]*agentAcc{}
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
			a = &agentAcc{first: ev.At}
			by[agent] = a
		}
		a.tokens = satAddPos(a.tokens, ev.OutputTokens)
		a.prompt = satAddPos(a.prompt, ev.PromptTokens)
		a.thinking = satAddPos(a.thinking, ev.ThinkingTokens)
		// The feed is not ordered by time: the remote ingest endpoint
		// accepts any ts, and a producer's clock can step. Track the
		// extremes rather than the last event walked, or a single
		// out-of-order stamp shortens the span below and the rate with it.
		if ev.At.Before(a.first) {
			a.first = ev.At
		}
		if ev.At.After(a.last) {
			a.last = ev.At
		}
		if ev.ViaEngine != "" {
			if a.via == "" {
				a.via = ev.ViaEngine
			} else if a.via != ev.ViaEngine {
				a.viaSplit = true
			}
		}
		a.n++
		if ev.Span > 0 {
			a.span += ev.Span
			a.spanned++
		}
		if ev.ViaEngine == "" {
			if a.ownN == 0 {
				a.ownFirst = ev.At
			} else if ev.At.Before(a.ownFirst) {
				a.ownFirst = ev.At
			}
			a.ownTokens = satAddPos(a.ownTokens, ev.OutputTokens)
			a.ownPrompt = satAddPos(a.ownPrompt, ev.PromptTokens)
			a.ownThinking = satAddPos(a.ownThinking, ev.ThinkingTokens)
			if ev.Span > 0 {
				a.ownSpan += ev.Span
				a.ownSpanned++
			}
			if ev.At.After(a.ownLast) {
				a.ownLast = ev.At
			}
			a.ownN++
		}
	}

	sum := AgentSummary{
		Rates: make([]AgentRate, 0, len(by)),
		Own:   make([]AgentRate, 0, len(by)),
	}
	for name, a := range by {
		r := AgentRate{
			Agent:    name,
			Tokens:   a.tokens,
			Prompt:   a.prompt,
			Thinking: a.thinking,
			Last:     a.last,
			// The engine label says the engine already counts these tokens, so
			// it is claimed only when it is true of the whole row; see
			// agentAcc.viaEngine.
			ViaEngine: a.viaEngine(),
		}
		// A rate needs a span. One event says how much, not how fast, so it
		// reports tokens without a rate, unless the event itself says how
		// long the model spent. That duration is the span, not the time
		// since the previous event.
		if a.spanned == a.n && a.span > 0 {
			secs := a.span.Seconds()
			r.TokPS = float64(a.tokens) / secs
			r.PromptPS = float64(a.prompt) / secs
		} else if span := a.last.Sub(a.first).Seconds(); a.n > 1 && span > 0 {
			r.TokPS = float64(a.tokens) / span
			r.PromptPS = float64(a.prompt) / span
		}
		sum.Rates = append(sum.Rates, r)
		if a.ownN == 0 {
			continue
		}
		o := AgentRate{
			Agent:    name,
			Tokens:   a.ownTokens,
			Prompt:   a.ownPrompt,
			Thinking: a.ownThinking,
			Last:     a.ownLast,
		}
		if a.ownSpanned == a.ownN && a.ownSpan > 0 {
			secs := a.ownSpan.Seconds()
			o.TokPS = float64(a.ownTokens) / secs
			o.PromptPS = float64(a.ownPrompt) / secs
		} else if span := a.ownLast.Sub(a.ownFirst).Seconds(); a.ownN > 1 && span > 0 {
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
