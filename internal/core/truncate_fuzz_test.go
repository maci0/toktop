// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rivo/uniseg"
	"golang.org/x/text/unicode/norm"
)

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
