// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"math"
	"slices"
	"strings"
)

// ContainsAny reports whether s contains any of subs. The engine and host
// parsers match the same field-name vocabulary, so the helper lives here
// rather than once per package.
func ContainsAny(s string, subs ...string) bool {
	return slices.ContainsFunc(subs, func(sub string) bool { return strings.Contains(s, sub) })
}

// SatInt coerces a float a vendor reported into an int, saturating at the
// type's bounds. NaN and non-positive values collapse to zero: every counter
// these parsers read is a magnitude, and an absent or negative one is not a
// measurement.
func SatInt(v float64) int {
	if !(v > 0) { // also catches NaN: every comparison with it is false
		return 0
	}
	if v >= math.MaxInt {
		return math.MaxInt
	}
	return int(v)
}

// SatUint is SatInt for the unsigned counts engines and vendor tools publish
// as floats (ctx_size, VRAM sizes); same rationale.
func SatUint(v float64) uint64 {
	if !(v > 0) {
		return 0
	}
	if v >= float64(math.MaxUint64) {
		return math.MaxUint64
	}
	return uint64(v)
}

// SatAddU64 adds two counts saturating at MaxUint64: the rule every counter
// sum in the tree follows, because a wrapped sum reads as no CPU used at all,
// a small allocation, or a plausible-looking byte count a remote host never
// measured. Agent token totals saturate at MaxInt64 (SatAddPos) and agentusage
// saturates at its own token ceiling, so those carry their own helpers.
func SatAddU64(a, b uint64) uint64 {
	if b > math.MaxUint64-a {
		return math.MaxUint64
	}
	return a + b
}

// SatAddPos adds two non-negative counts, saturating at MaxInt64. Agent token
// totals accumulate one event per retained sample from several producers,
// some of which report whatever their transcript file happened to hold, so
// the sum must not wrap to a negative total.
func SatAddPos(a, b int64) int64 {
	if a < 0 {
		a = 0
	}
	if b < 0 {
		b = 0
	}
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
