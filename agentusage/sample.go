// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"math"
	"time"
)

// Sample is cumulative usage observed since the watcher attached.
type Sample struct {
	// Output is generated tokens observed since attach.
	Output int
	// Thinking is the reasoning share of Output, when the agent reports it
	// separately: what the model spent before it wrote anything the user sees.
	Thinking int
	// Total is the largest per-request context size seen, not a sum: summing
	// those would count the same conversation once per turn.
	Total int
	// Input is billed prompt tokens, accrued per request the same way Output is.
	Input int
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
	// At is when the current sample was read, whether or not anything grew, so
	// a caller can stamp the interval the samples span.
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
		At:       s.At,
	}
	return d, d.Output > 0 || d.Thinking > 0 || d.Input > 0
}

// Rate returns output tokens per second between two samples, and whether it
// could be computed at all. Both samples need a timestamp: a missing one is
// not a reading, and treating it as the zero instant would invent a rate off
// a first sample whose counter has already grown. It never extrapolates:
// without two readings and a positive span there is no rate to report.
// Prompt growth is InputRate.
func Rate(prev, cur Sample) (float64, bool) {
	return deltaRate(prev.At, cur.At, prev.Output, cur.Output)
}

// InputRate returns billed prompt tokens per second between two samples, and
// whether it could be computed. Same rules as Rate: both samples need a
// timestamp, and no positive span or no growth means no rate, not a zero.
func InputRate(prev, cur Sample) (float64, bool) {
	return deltaRate(prev.At, cur.At, prev.Input, cur.Input)
}

// ThinkingRate returns reasoning tokens per second between two samples, and
// whether it could be computed. Same rules as [Rate], over the reasoning
// share of Output rather than all of it. An agent that does not report
// reasoning separately never grows it, so this reports no rate for one.
func ThinkingRate(prev, cur Sample) (float64, bool) {
	return deltaRate(prev.At, cur.At, prev.Thinking, cur.Thinking)
}

func deltaRate(prevAt, curAt time.Time, prevN, curN int) (float64, bool) {
	if prevAt.IsZero() || curAt.IsZero() {
		return 0, false
	}
	span := curAt.Sub(prevAt).Seconds()
	if span <= 0 || curN <= prevN {
		return 0, false
	}
	return float64(curN-prevN) / span, true
}

// values is one record's contribution, before it is folded into a [Sample].
type values struct {
	output   int
	thinking int
	total    int
	input    int
}

// present is the same rule as Sample.Empty inverted: any counter is a
// reading. Sources and parsers that skip "empty" records must use this, or
// a thinking-only line is dropped before it can become a Sample.
func (v values) present() bool {
	return v.output > 0 || v.thinking > 0 || v.total > 0 || v.input > 0
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
// generic walker.
func counter(n int) int {
	if n < 0 || n > maxSaneTokens {
		return 0
	}
	return n
}

// clampSane is the one statement of the ceiling counter enforces: a negative
// or absurd magnitude reads as absent, the same judgment asInt makes for the
// generic walker.
func clampSane(n int64) int64 {
	if n < 0 || n > maxSaneTokens {
		return 0
	}
	return n
}

// counter64 is counter for values that arrive as int64 from a database
// column, so a magnitude that does not fit in int is rejected before the
// conversion rather than wrapping.
func counter64(n int64) int {
	c := clampSane(n)
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

func satSub(a, b int) int {
	if a < b {
		return 0
	}
	return a - b
}
