// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// FuzzWidthClipShorten throws untrusted label text at the measurement and
// cutting path. The strings reaching it are whatever another program said:
// model ids off a /v1/models payload, agent names off an event stream, GPU
// driver strings, remote hostnames. Every one of them is walked by hand over
// escape sequences and grapheme clusters with index arithmetic, and a cell
// miscounted here desynchronizes every panel on the frame rather than
// corrupting one line.
//
// The invariants are the ones the renderers rely on, and they are asserted
// rather than left to the fuzzer to discover by crashing: plainWidth answers
// exactly what lipgloss answers or declines, shorten and clip never hand back
// a line wider than the budget they were given, cutting is idempotent, and
// the two block joins reproduce lipgloss byte for byte.
func FuzzWidthClipShorten(f *testing.F) {
	for _, seed := range []struct {
		s string
		w int
	}{
		{"", 0},
		{"abc", 3},
		{"abc", 2},
		{"世界", 4},
		{"世界", 3},
		{"cafe\u0301 latte", 6},
		{"⠁⠂⠄", 2},
		{"\x1b[31mred\x1b[0m", 3},
		{"\x1b[38;2;1;2;3;mx\x1b[0m", 1},
		{"\x1b]0;title\x07x", 1},
		{"\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\", 4},
		{"\x1b[", 3},
		{"\x1b]unterminated", 5},
		{"\x1b", 1},
		{"\x1bX", 1},
		{"\x1b\x1b\x1b", 1},
		{"\x1b[0K", 1},
		{"a\x00b", 3},
		{"tab\there", 4},
		{"\xff\xfe", 2},
		{"▲ 1.2k tok/s", 8},
		{"a\nbb\n\nccc", 4},
		{"emoji 👩‍👩‍👧‍👦 zwj", 6},
		{"🇯🇵🇺🇸", 3},
		// The two shapes the fuzzer found: a two-byte escape, which the fast
		// path used to answer one cell wider than lipgloss does, and a C1 byte
		// followed by the spaces a pad appends.
		{"\x1bX0", 4},
		{"0\x9f", 70},
		{strings.Repeat("│", 40), 40},
		{strings.Repeat("x", 300), 10},
	} {
		f.Add(seed.s, seed.w)
	}

	f.Fuzz(func(t *testing.T, s string, w int) {
		// A budget is a panel width: a few cells to a screen, never negative
		// in a frame. Clamping keeps the target from spending its whole budget
		// on the negative half of int64, which is one line of code either way.
		if w < 0 {
			w = 0
		}
		if w > 512 {
			w = 512
		}

		// plainWidth is a fast path over hand-rolled escape and rune scanning,
		// and widthOf trusts it whenever it does not decline. Wherever it
		// answers, it has to answer the same as the library it stands in for,
		// or the frame mixes two different measures of the same string.
		if got := plainWidth(s); got >= 0 && got != lipgloss.Width(s) {
			t.Fatalf("plainWidth(%q) = %d, lipgloss.Width = %d", s, got, lipgloss.Width(s))
		}
		if got := widthOf(s); got != lipgloss.Width(s) {
			t.Fatalf("widthOf(%q) = %d, lipgloss.Width = %d", s, got, lipgloss.Width(s))
		}

		// A cut line that renders wider than its budget is the failure that
		// reaches the operator: the panel it sits in stops lining up, and no
		// assertion anywhere else would notice.
		short := shorten(s, w)
		if got := widthOf(short); got > w {
			t.Fatalf("shorten(%q, %d) = %q, %d cells wide", s, w, short, got)
		}
		if cut := shorten(short, w); cut != short {
			t.Fatalf("shorten is not idempotent at %d: %q then %q", w, short, cut)
		}
		if w == 0 && short != "" {
			t.Fatalf("shorten(%q, 0) = %q, want the empty string", s, short)
		}

		clipped := clip(s, w)
		if got := widthOf(clipped); got > w {
			t.Fatalf("clip(%q, %d) = %q, %d cells wide", s, w, clipped, got)
		}
		if w == 0 && clipped != "" {
			t.Fatalf("clip(%q, 0) = %q, want the empty string", s, clipped)
		}

		// Padding is what turns a measurement into a row. On the single-line
		// rows the renderers pad, a string that already fits is grown to
		// exactly w, and a string that overflows is left alone rather than
		// truncated: a pad that measured with something other than widthOf
		// would put a short row in a column of w-wide ones.
		//
		// A string carrying a control byte is excluded because the measure it
		// is padded against is not additive there: lipgloss reads a trailing
		// "\x1b" or a lone "\x9f" as a sequence opener and drops the spaces
		// after it, so the row measures one cell before padding and one
		// after. core.SanitizeText strips both, so no rendered label has one.
		measured := widthOf(s)
		if padStable(s) && measured > 0 && measured <= w {
			if padded := padTo(s, w); widthOf(padded) != w {
				t.Fatalf("padTo(%q, %d) = %q, %d cells, want %d", s, w, padded, widthOf(padded), w)
			}
			if padded := padStart(s, w); widthOf(padded) != w {
				t.Fatalf("padStart(%q, %d) = %q, %d cells, want %d", s, w, padded, widthOf(padded), w)
			}
		}
		if padded := padTo(s, w); widthOf(padded) < measured {
			t.Fatalf("padTo(%q, %d) = %q narrowed the row from %d cells to %d", s, w, padded, measured, widthOf(padded))
		}
	})
}

// padStable reports whether widthOf measures s by what s alone contributes,
// so appending padding spaces adds exactly the cells they are worth. A control
// byte breaks that: lipgloss resolves some of them as sequence openers and
// drops what follows, and core.SanitizeText strips them from rendered labels.
func padStable(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || (c >= 0x7f && c <= 0x9f) {
			return false
		}
	}
	return true
}

// FuzzJoinBlocksMatchesLipgloss is the differential half: joinBlocks and
// joinAcross replaced the lipgloss calls that were a fifth of a frame's CPU,
// so the two implementations have to stay interchangeable on every input, not
// only the hand-picked rows in the unit tests. The blocks here carry the
// untrusted labels the panels are built from, so the rows a join has to
// measure are exactly the rows a hostile string can shape.
func FuzzJoinBlocksMatchesLipgloss(f *testing.F) {
	for _, seed := range [][]string{
		{},
		{""},
		{"a", "bb", "ccc"},
		{"\x1b[31mred\x1b[0m", "plain"},
		{"世界", "a"},
		{"\n\n\n", "x"},
		{"a\nbb", ""},
		{"⠁⠂", "│x│\n⠄"},
		{"\x1b]unterminated", "\x1b[31mok\x1b[0m"},
	} {
		// A NUL-joined string rather than []string: fuzz corpus entries are
		// one blob per target. A block that itself carries a NUL is split into
		// two, which is still a block list the joins accept.
		f.Add(strings.Join(seed, "\x00"))
	}

	f.Fuzz(func(t *testing.T, joined string) {
		blocks := strings.Split(joined, "\x00")
		// Keep the target from growing without bound: the interesting shapes
		// are the short rows with hostile bytes in them, and a wide block costs
		// a full extra pass on both implementations for nothing.
		if len(blocks) > 8 {
			blocks = blocks[:8]
		}
		for i, b := range blocks {
			if len(b) > 256 {
				blocks[i] = b[:256]
			}
		}

		if got, want := joinBlocks(blocks...), lipgloss.JoinVertical(lipgloss.Left, blocks...); got != want {
			t.Fatalf("joinBlocks(%q)\n got %q\nwant %q", blocks, got, want)
		}
		if got, want := joinAcross(blocks...), lipgloss.JoinHorizontal(lipgloss.Top, blocks...); got != want {
			t.Fatalf("joinAcross(%q)\n got %q\nwant %q", blocks, got, want)
		}
	})
}
