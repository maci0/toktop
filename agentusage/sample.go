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

// counter64 is counter for values that arrive as int64 from a database
// column, so a magnitude that does not fit in int is rejected before the
// conversion rather than wrapping.
func counter64(n int64) int {
	if n < 0 || n > maxSaneTokens || n > math.MaxInt {
		return 0
	}
	return int(n)
}

func clampSane(n int64) int64 {
	if n < 0 || n > maxSaneTokens {
		return 0
	}
	return n
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
