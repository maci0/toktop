// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rivo/uniseg"
	"golang.org/x/text/unicode/norm"
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

// FuzzClampRawText drives the display-length caps over raw bytes from an
// engine or vendor response. Everything here ends up in a terminal cell, so a
// cap must never split a grapheme cluster, never emit invalid UTF-8, never
// exceed the cap it was given, and must leave Snippet on one line and
// render-safe even when the body carries escape sequences, control bytes, or
// nothing but ill-formed encoding.
func FuzzClampRawText(f *testing.F) {
	for _, seed := range []string{
		"", "plain", "héllo ✓ 日本語 🎉", "👩‍💻", "\U0001F1E9\U0001F1EA",
		"é", "\x1b]52;c;YU9UQw==\x07", "\x1b[2J\x1b[3;5H", "\x00\x01\x7f",
		"cl\u202ee", "a\tb\nc\r\nd", "éééééééééé",
		strings.Repeat("🎉", 100), strings.Repeat("\U0001F1E9\U0001F1EA", 100),
		strings.Repeat("é", 100), strings.Repeat("a", 1000),
		strings.Repeat("\x1b]0;", 20) + "tail", strings.Repeat("\x00", 50),
		"\ufeffcl\u00ad", "👩‍💻\U000E0064",
	} {
		for _, n := range []int{0, 1, 8, 64, 256, 4096} {
			f.Add([]byte(seed), n)
		}
	}
	// Raw bytes that are not valid UTF-8 at all: these caps are handed
	// response bodies, which arrive as bytes, not as text.
	for _, seed := range [][]byte{
		{0xff, 0xfe, 0xfd}, {0xc0, 0x80}, {0xed, 0xa0, 0x80}, {0xf4, 0x90, 0x80, 0x80},
		{0xc2}, {0xe2, 0x82}, {0x00, 0x1b, 0x5b}, {0x80},
	} {
		for _, n := range []int{0, 1, 8, 64} {
			f.Add(seed, n)
		}
	}

	f.Fuzz(func(t *testing.T, raw []byte, n int) {
		s := string(raw)
		// Truncation cuts bytes, so it is only held to the valid-UTF-8
		// contract for input that was valid to begin with. Snippet sanitizes
		// first and must always come back valid.
		inValid := utf8.ValidString(s)
		total := uniseg.GraphemeClusterCount(s)

		got := TruncateClusters(s, n)
		if n <= 0 {
			if got != "" {
				t.Fatalf("TruncateClusters(%q, %d) = %q, want empty", s, n, got)
			}
		} else {
			if inValid && !utf8.ValidString(got) {
				t.Fatalf("TruncateClusters(%q, %d) emitted invalid UTF-8: %q", s, n, got)
			}
			// A whole-cluster prefix: what comes back is exactly the first
			// min(n, total) clusters, never a re-segmented rendering of them.
			if c := uniseg.GraphemeClusterCount(got); c != min(n, total) {
				t.Fatalf("TruncateClusters(%q, %d) kept %d clusters, want %d", s, n, c, min(n, total))
			}
			if !strings.HasPrefix(s, got) {
				t.Fatalf("TruncateClusters(%q, %d) = %q, not a prefix of the input", s, n, got)
			}
		}

		clamped := ClampField(s, n)
		if n <= 0 {
			if clamped != "" {
				t.Fatalf("ClampField(%q, %d) = %q, want empty", s, n, clamped)
			}
		} else {
			if inValid && !utf8.ValidString(clamped) {
				t.Fatalf("ClampField(%q, %d) emitted invalid UTF-8: %q", s, n, clamped)
			}
			if c := uniseg.GraphemeClusterCount(clamped); c > n {
				t.Fatalf("ClampField(%q, %d) kept %d clusters", s, n, c)
			}
			if inValid && !norm.NFC.IsNormalString(clamped) {
				t.Fatalf("ClampField(%q, %d) result is not NFC: %q", s, n, clamped)
			}
		}

		snip := Snippet(raw)
		if !utf8.ValidString(snip) {
			t.Fatalf("Snippet(%q) emitted invalid UTF-8: %q", s, snip)
		}
		if c := uniseg.GraphemeClusterCount(snip); c > SnippetCap {
			t.Fatalf("Snippet(%q) kept %d characters, cap %d", s, c, SnippetCap)
		}
		if strings.ContainsAny(snip, "\n\r\t\v\f") {
			t.Fatalf("Snippet(%q) = %q, not collapsed to one line", s, snip)
		}
		if again := SanitizeText(snip); again != snip {
			t.Fatalf("Snippet(%q) = %q still holds unsanitized text: %q", s, snip, again)
		}
	})
}
