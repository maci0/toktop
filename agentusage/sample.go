// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"math"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// Sample is the usage observed since the watcher attached, as the transcripts
// stand at the last read. The counters are a level rather than a running total
// that only rises: a rewritten transcript is re-read whole and republishes
// lower figures, so a caller reporting events must take growth with
// [Sample.Delta].
type Sample struct {
	// Output is generated tokens observed since attach.
	Output int
	// Thinking is the reasoning share of Output, when the agent reports it
	// separately: what the model spent before it wrote anything the user sees.
	Thinking int
	// Total is the largest per-request context size seen, not a sum: summing
	// those would count the same conversation once per turn. It is a level
	// rather than a running total, so it has no [Delta] field: a caller showing
	// a context window reads cur.Total off the current sample, and does not
	// report it as growth. A sample that only grows Total still reaches a Run
	// callback, and cur.Delta(prev) reports no growth for it, since no tokens
	// were spent.
	Total int
	// Input is billed prompt tokens, accrued per request the same way Output is.
	Input int
	// Span is how long the model spent producing the tokens in this sample,
	// when the transcript records it. [Rate] and its siblings divide by it
	// when it grew, and by the time between the two samples when it did
	// not, which is the only interval a transcript without a recorded span
	// leaves a caller.
	Span time.Duration
	// At is when the counters last changed, which is when the reading was
	// taken. A poll that observed nothing does not move it, so a stalled
	// agent's rate is averaged over the whole pause rather than over the
	// poll interval.
	At time.Time
}

// Empty reports whether nothing has been observed yet. Thinking-only samples
// count: an agent that reports reasoning without a billed output is still a
// reading, and callers that skip Empty samples must not drop it.
func (s Sample) Empty() bool {
	return !values{output: s.Output, thinking: s.Thinking, total: s.Total, input: s.Input}.present()
}

// Delta is the growth between two consecutive samples: one interval's usage,
// where a [Sample] is the total observed since the watcher attached. A caller
// that reports events reports these, never the totals, or it bills the same
// tokens once per poll.
type Delta struct {
	// Output is generated tokens since the previous sample.
	Output int
	// Thinking is the reasoning share of Output, under the same rules.
	Thinking int
	// Input is billed prompt tokens since the previous sample.
	Input int
	// Span is how long the model spent producing this interval's tokens,
	// when the transcript recorded it. Zero means it did not, and the rate
	// for the interval is the growth over the time between the two samples.
	Span time.Duration
	// At is the current sample's own [Sample.At] copied across: when the
	// counters last changed, not when this interval was measured. A poll that
	// observed nothing does not move it, so a delta reporting no growth
	// carries the same instant the sample before it did. A caller stamping an
	// interval reads the start from the previous sample's At and the end from
	// here; [Rate] and its siblings take both samples and do that themselves.
	At time.Time
}

// Delta returns what grew from prev to the current sample, and whether
// anything did.
//
// Counters only rise between two readings of the same watcher, so a sample
// smaller than the one before it is a transcript rewritten under the watcher:
// the figures it replaced are ones it no longer records, and counting them as
// growth bills the same tokens twice. That case reports no growth, which is
// what taking the current sample as the new baseline comes to. A caller
// passing the previous sample and keeping the current one has the whole
// re-baselining rule:
//
//	if d, ok := cur.Delta(prev); ok {
//		report(d)
//	}
//	prev = cur
func (s Sample) Delta(prev Sample) (Delta, bool) {
	d := Delta{
		Output:   satSub(s.Output, prev.Output),
		Thinking: satSub(s.Thinking, prev.Thinking),
		Input:    satSub(s.Input, prev.Input),
		Span:     s.Span - prev.Span,
		At:       s.At,
	}
	if d.Span < 0 {
		d.Span = 0
	}
	return d, d.Output > 0 || d.Thinking > 0 || d.Input > 0
}

// RateFrom returns output tokens per second between prev and the current
// sample, and whether it could be computed. It is [Rate] with the two samples
// in the order a caller holds them, the same shape as [Sample.Delta], so the
// arguments cannot be swapped at the call site: cur.RateFrom(prev), never
// Rate(prev, cur) with the names to keep straight.
func (s Sample) RateFrom(prev Sample) (float64, bool) { return Rate(prev, s) }

// InputRateFrom is [InputRate] in the same argument order as [Sample.RateFrom].
func (s Sample) InputRateFrom(prev Sample) (float64, bool) { return InputRate(prev, s) }

// ThinkingRateFrom is [ThinkingRate] in the same argument order as
// [Sample.RateFrom].
func (s Sample) ThinkingRateFrom(prev Sample) (float64, bool) { return ThinkingRate(prev, s) }

// Rate returns output tokens per second between two samples, and whether it
// could be computed at all. Both samples need a timestamp: a missing one is
// not a reading, and treating it as the zero instant would invent a rate off
// a first sample whose counter has already grown. It never extrapolates:
// without two readings and a positive span there is no rate to report.
// Prompt growth is InputRate. A caller holding the current sample calls
// [Sample.RateFrom] instead, which takes the two in the same order as
// [Sample.Delta].
//
// The span it divides by is the time the model spent producing the interval's
// tokens when the transcript recorded one ([Sample.Span]), and the wall gap
// between the two readings otherwise. A transcript that reports a turn's
// length reports it because the gap is not the same interval: a grok turn's
// counts arrive when the turn ends, and the gap since the previous one covers
// that turn's whole wall time, tool calls included, so dividing by it
// reports a rate of a generation that was never continuous.
func Rate(prev, cur Sample) (float64, bool) {
	return deltaRate(prev, cur, prev.Output, cur.Output)
}

// InputRate returns billed prompt tokens per second between two samples, and
// whether it could be computed. Same rules as Rate: the recorded span where
// there is one, and both timestamps needed otherwise, and no positive span or
// no growth means no rate, not a zero.
func InputRate(prev, cur Sample) (float64, bool) {
	return deltaRate(prev, cur, prev.Input, cur.Input)
}

// ThinkingRate returns reasoning tokens per second between two samples, and
// whether it could be computed. Same rules as [Rate], over the reasoning
// share of Output rather than all of it. An agent that does not report
// reasoning separately never grows it, so this reports no rate for one.
func ThinkingRate(prev, cur Sample) (float64, bool) {
	return deltaRate(prev, cur, prev.Thinking, cur.Thinking)
}

func deltaRate(prev, cur Sample, prevN, curN int) (float64, bool) {
	if curN <= prevN {
		return 0, false
	}
	n := float64(curN - prevN)
	if span := cur.Span - prev.Span; span > 0 {
		return n / span.Seconds(), true
	}
	if prev.At.IsZero() || cur.At.IsZero() {
		return 0, false
	}
	span := cur.At.Sub(prev.At).Seconds()
	if span <= 0 {
		return 0, false
	}
	return n / span, true
}

// values is one record's contribution, before it is folded into a [Sample].
type values struct {
	output   int
	thinking int
	total    int
	input    int
	// span is how long the model spent producing this record, when the
	// transcript says so. A grok turn is one record at the end, minutes
	// after the previous one, and the rate is this record's tokens over
	// this duration rather than the gap between two turns.
	span time.Duration
}

// present is the same rule as Sample.Empty inverted: any counter is a
// reading. Sources and parsers that skip "empty" records must use this, or
// a thinking-only line is dropped before it can become a Sample.
func (v values) present() bool {
	return v.output > 0 || v.thinking > 0 || v.total > 0 || v.input > 0
}

// foldCounters folds one provider's counter set, the shape several JSON
// transcripts report: separate prompt, output, thoughts and cached counters
// beside a total. cached is added to the prompt only when the total is larger
// than prompt+output+thoughts by that share; when the total already equals
// that sum, cached is inside the prompt. thoughts is 0 for a source that
// folds reasoning into output already, and the total is rebuilt from the
// parts when the source omits it.
func foldCounters(prompt, output, thoughts, cached, total int) (values, bool) {
	in := counter(prompt)
	out := counter(output)
	think := counter(thoughts)
	cache := counter(cached)
	tot := counter(total)
	parts := satAdd(in, satAdd(out, think))
	if cache > 0 && tot >= satAdd(parts, cache) && tot != parts {
		in = satAdd(in, cache)
	}
	v := values{output: out, thinking: think, input: in, total: tot}
	if v.total == 0 {
		v.total = satAdd(in, satAdd(out, think))
	}
	if !v.present() {
		return values{}, false
	}
	return v, true
}

// valueKind says how an adapter's numbers accumulate.
type valueKind uint8

const (
	// perMessage values are added up: each line carries one message's usage.
	perMessage valueKind = iota
	// cumulative values already include everything before them, so the
	// watcher subtracts the first value it sees.
	cumulative
)

// maxSaneTokens bounds one counter a transcript line may contribute. Real
// usage never approaches it; anything larger is corruption or hostility, and
// reporting nothing beats displaying a lie (or overflowing the totals).
const maxSaneTokens = 1 << 40

// counter coerces a decoded transcript counter to its contribution: negative
// or absurd magnitudes read as absent, the same judgment asInt makes for the
// generic walker. It is generic over the two widths counters arrive in, so
// both an int from a transcript and an int64 from a database column are
// checked before any conversion narrows them.
func counter[T int | int64](n T) T {
	if n < 0 || n > maxSaneTokens {
		return 0
	}
	return n
}

// counter64 is counter for values that arrive as int64 from a database
// column, so a magnitude that does not fit in int is rejected before the
// conversion rather than wrapping.
func counter64(n int64) int {
	c := counter(n)
	if c > math.MaxInt {
		return 0
	}
	return int(c)
}

// satAdd sums two non-negative counters, saturating instead of wrapping: a
// transcript with absurd counts must read as enormous, never as negative. It
// saturates at maxSaneTokens, the same ceiling counter enforces on a single
// record, so a total never reaches a magnitude this package would refuse to
// parse back.
func satAdd(a, b int) int {
	s := a + b
	if s < 0 || s > maxSaneTokens {
		return maxSaneTokens
	}
	return s
}

// satAdd64 is satAdd for counters that arrive from a session store as int64.
// It saturates at the same ceiling, and for the same reason: a store holding
// sessions that total more than the ceiling is corrupt, and the reading that
// says so is enormous rather than nothing. Refusing the reading instead loses
// every poll while the offending rows stand, since the store is re-read from
// the attach instant each time and the same total comes back each time.
func satAdd64(a, b int64) int64 {
	if a > int64(maxSaneTokens)-b {
		return int64(maxSaneTokens)
	}
	return a + b
}

func satSub(a, b int) int {
	if a < b {
		return 0
	}
	return a - b
}

// satAddSpan sums two model-time spans the way satAdd sums counters: by
// saturating at maxSaneSpan rather than wrapping. One record may carry a
// span of up to maxTurnMS, which is close to the whole int64 nanosecond
// range, so two records on one transcript summed with a plain + wrapped the
// accumulator negative and the sample reported a negative span for work that
// was counted.
//
// The ceiling matches core.MaxEventSpan, the longest span the event boundary
// will carry: past it the figure is dropped rather than reported, so
// accumulating further only loses the information that made it unusable.
func satAddSpan(a, b time.Duration) time.Duration {
	if a <= 0 || b <= 0 {
		return max(a, b, 0)
	}
	if a > maxSaneSpan-b {
		return maxSaneSpan
	}
	return a + b
}

// maxSaneSpan bounds an accumulated model-time span, the same ceiling the
// event boundary applies to a single event: see satAddSpan.
const maxSaneSpan = core.MaxEventSpan
