// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"math"
	"testing"
)

// FuzzSatCoerce drives the numeric trust boundary with arbitrary float bits.
// Vendor and engine telemetry reaches these helpers as JSON numbers, and a
// JSON number can be 1e400, negative, fractional, or NaN after a round trip
// through a producer that computed a ratio. Every caller sums the result into
// a displayed total, so a coerced count must never be negative, never wrap,
// and must agree with the ordinary int/uint64 conversion wherever that
// conversion is already in range.
func FuzzSatCoerce(f *testing.F) {
	for _, seed := range []float64{
		0, 1, -1, 0.5, -0.5, 1e308, -1e308,
		float64(math.MaxInt64), float64(math.MaxInt64) + 1, math.MaxInt32, math.MaxInt16,
		math.SmallestNonzeroFloat64, -math.SmallestNonzeroFloat64,
		math.NaN(), math.Inf(1), math.Inf(-1),
		float64(math.MaxUint64), float64(math.MaxUint64) / 2, 1 << 63, -1 << 62,
	} {
		f.Add(seed, seed/3, seed*7)
	}

	f.Fuzz(func(t *testing.T, v, w, x float64) {
		got := SatInt(v)
		if got < 0 {
			t.Fatalf("SatInt(%v) = %d, negative", v, got)
		}
		switch {
		case math.IsNaN(v) || v <= 0:
			if got != 0 {
				t.Fatalf("SatInt(%v) = %d, want 0 for a non-positive or NaN count", v, got)
			}
		case v >= math.MaxInt:
			if int64(got) != math.MaxInt64 {
				t.Fatalf("SatInt(%v) = %d, want MaxInt64", v, got)
			}
		default:
			if int64(got) != int64(v) {
				t.Fatalf("SatInt(%v) = %d, want the truncated %d", v, got, int64(v))
			}
		}

		gu := SatUint(w)
		switch {
		case math.IsNaN(w) || w <= 0:
			if gu != 0 {
				t.Fatalf("SatUint(%v) = %d, want 0 for a non-positive or NaN count", w, gu)
			}
		case w >= float64(math.MaxUint64):
			if gu != math.MaxUint64 {
				t.Fatalf("SatUint(%v) = %d, want MaxUint64", w, gu)
			}
		default:
			if gu != uint64(w) {
				t.Fatalf("SatUint(%v) = %d, want the truncated %d", w, gu, uint64(w))
			}
		}

		// Coercion must be monotonic: a larger reported count can never come
		// back as a smaller one, or a spiking process would read as a drop.
		if v > 0 && w > v && w < math.MaxInt && SatInt(w) < got {
			t.Fatalf("SatInt is not monotonic: SatInt(%v)=%d > SatInt(%v)=%d", v, got, w, SatInt(w))
		}
		if v > 0 && w > v && w < float64(math.MaxUint64) && SatUint(w) < gu {
			t.Fatalf("SatUint is not monotonic: SatUint(%v)=%d > SatUint(%v)=%d", v, gu, w, SatUint(w))
		}

		// Totals accumulate one event per retained sample, so a run of
		// additions must stay non-negative and must saturate rather than
		// wrap, whatever the producers reported.
		a, b, c := int64(SatInt(v)), int64(SatInt(w)), int64(SatInt(x))
		sum := SatAddPos(SatAddPos(SatAddPos(a, b), c), c)
		if sum < 0 {
			t.Fatalf("SatAddPos summed %v, %v, %v into %d", v, w, x, sum)
		}
		// Where the four terms cannot overflow between them, the chain is
		// the plain sum: no wrapping, no dropped sample.
		const noOverflow = math.MaxInt64 / 8
		if a <= noOverflow && b <= noOverflow && c <= noOverflow {
			if want := a + b + 2*c; sum != want {
				t.Fatalf("SatAddPos chain = %d, want the exact sum %d for %v, %v, %v", sum, want, v, w, x)
			}
		}
		if SatAddPos(math.MaxInt64, 1) != math.MaxInt64 {
			t.Fatal("SatAddPos(MaxInt64, 1) wrapped")
		}
		if SatAddPos(math.MaxInt64, math.MaxInt64) != math.MaxInt64 {
			t.Fatal("SatAddPos(MaxInt64, MaxInt64) wrapped")
		}
		if SatAddPos(-1, -1) != 0 {
			t.Fatal("SatAddPos kept negative inputs negative")
		}
	})
}
