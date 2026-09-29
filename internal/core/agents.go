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

// tokenAcc is one counted half of an agent's window: every event the agent
// reported, or only the unattributed ones.
type tokenAcc struct {
	tokens   int64
	prompt   int64
	thinking int64
	first    time.Time
	last     time.Time
	n        int
	// span is the sum of event durations the sender reported. spanned counts
	// those events. A rate uses the durations only when every event in the
	// window brought one: a grok turn is a single event minutes after the
	// last, and the gap between them is not how long the model ran.
	span    time.Duration
	spanned int
}

// add folds one event into the totals. The feed is not ordered by time: the
// remote ingest endpoint accepts any ts, and a producer's clock can step. Track
// the extremes rather than the last event walked, or a single out-of-order
// stamp shortens the span below and the rate with it.
func (t *tokenAcc) add(ev AgentEvent) {
	if t.n == 0 {
		t.first = ev.At
	}
	if ev.At.Before(t.first) {
		t.first = ev.At
	}
	if ev.At.After(t.last) {
		t.last = ev.At
	}
	t.tokens = satAddPos(t.tokens, ev.OutputTokens)
	t.prompt = satAddPos(t.prompt, ev.PromptTokens)
	t.thinking = satAddPos(t.thinking, ev.ThinkingTokens)
	if ev.Span > 0 {
		t.span += ev.Span
		t.spanned++
	}
	t.n++
}

// row renders the accumulated totals. A rate needs a span: one event says how
// much, not how fast, so a window holding a single event reports tokens without
// a rate, unless the event itself said how long the model spent. That duration
// is the span, not the time since the previous event.
func (t *tokenAcc) row(agent, via string) AgentRate {
	r := AgentRate{
		Agent:     agent,
		Tokens:    t.tokens,
		Prompt:    t.prompt,
		Thinking:  t.thinking,
		Last:      t.last,
		ViaEngine: via,
	}
	var secs float64
	switch {
	case t.spanned == t.n && t.span > 0:
		secs = t.span.Seconds()
	case t.n > 1:
		secs = t.last.Sub(t.first).Seconds()
	}
	if secs > 0 {
		r.TokPS = float64(t.tokens) / secs
		r.PromptPS = float64(t.prompt) / secs
	}
	return r
}

// agentAcc is one agent's running totals over the summary window.
type agentAcc struct {
	// all and own are the two halves the summary reports. They are kept per
	// event rather than per agent: an agent that connects to (or leaves) a
	// monitored engine mid-window contributes the slice that went direct.
	all tokenAcc
	own tokenAcc
	// via is the engine every attributed event named, and viaSplit records
	// that the agent named more than one across the window. Neither is read
	// off the last event walked: the feed is not time-ordered, so that made
	// the label depend on arrival order.
	via      string
	viaSplit bool
}

// viaEngine is the engine a rate row may name: one every in-window event of
// the agent went through. A direct event in the window withdraws the claim,
// because the engine did not see those tokens and the totals below count them,
// and so does a window that names two engines, because there is no single one
// to point at. An agent in either case reports no label, which every consumer
// already renders as the row's own measured rate.
func (a *agentAcc) viaEngine() string {
	if a.own.n > 0 || a.viaSplit {
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
			a = &agentAcc{}
			by[agent] = a
		}
		a.all.add(ev)
		if ev.ViaEngine != "" {
			if a.via == "" {
				a.via = ev.ViaEngine
			} else if a.via != ev.ViaEngine {
				a.viaSplit = true
			}
			continue
		}
		a.own.add(ev)
	}

	sum := AgentSummary{
		Rates: make([]AgentRate, 0, len(by)),
		Own:   make([]AgentRate, 0, len(by)),
	}
	for name, a := range by {
		// The engine label says the engine already counts these tokens, so it
		// is claimed only when it is true of the whole row; see
		// agentAcc.viaEngine.
		sum.Rates = append(sum.Rates, a.all.row(name, a.viaEngine()))
		if a.own.n == 0 {
			continue
		}
		sum.Own = append(sum.Own, a.own.row(name, ""))
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
