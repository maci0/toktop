// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package ui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/rivo/uniseg"
	unorm "golang.org/x/text/unicode/norm"
)

// The unit a cut is allowed to land on is the whole point of shorten, and an
// ASCII-only suite cannot see it: every ASCII character is at once one byte,
// one rune and one cell, so all three units agree and every cut looks right.
// The inputs here are the spellings where the three units part company — a
// grapheme cluster is several runes (a flag, a ZWJ sequence, a keycap, an
// emoji with a skin-tone modifier), and a rune is not a cell (CJK and most
// emoji occupy two columns).
//
// The property: for every input and every budget the result is valid UTF-8 (a
// cut never lands inside a multi-byte sequence), never wider than the budget
// (a cut never leaves the row overlong), and made only of whole clusters of
// the input (a cut never orphans half of one).
func TestShortenCellBudgetAcrossGraphemes(t *testing.T) {
	inputs := []struct {
		name string
		s    string
	}{
		{"ascii", "abcdefghij"},
		{"accented nfc", "école école"},
		{"accented nfd", "école école"},
		{"zwj family", "\U0001f468‍\U0001f469‍\U0001f467‍\U0001f466family"},
		{"flags", "\U0001f1fa\U0001f1f8\U0001f1eb\U0001f1f7flags"},
		{"stacked combining", "á̂̃"},
		{"cjk", "日本語テキスト"},
		{"mixed", "abc\U0001f600déf"},
		{"skin tone", "\U0001f44d\U0001f3fdwave"},
		{"keycap", "\U0001f511one"},
	}
	for _, in := range inputs {
		for n := 0; n <= lipgloss.Width(in.s)+4; n++ {
			got := shorten(in.s, n)
			if !utf8.ValidString(got) {
				t.Errorf("shorten(%s, %d) = %q, which is not valid UTF-8", in.name, n, got)
			}
			if w := lipgloss.Width(got); w > n {
				t.Errorf("shorten(%s, %d) = %q, %d cells wide, over the budget", in.name, n, got, w)
			}
			if n > 0 && got == "" {
				t.Errorf("shorten(%s, %d) returned nothing for a positive budget", in.name, n)
			}
			// What survived the cut must be whole clusters taken in order from
			// the input. Splitting a flag or a ZWJ sequence would print a lone
			// regional indicator or a dangling U+200D in the pane.
			head := strings.TrimSuffix(got, "…")
			if head == "" {
				continue
			}
			clusters := splitGraphemeClusters(in.s)
			for i, c := range splitGraphemeClusters(head) {
				if i >= len(clusters) || c != clusters[i] {
					t.Errorf("shorten(%s, %d) = %q: cluster %d is %q, the input had %q",
						in.name, n, got, i, c, clusters[i])
				}
			}
		}
	}
}

// The two normalization spellings of one word are the same word to the
// reader, and the tree canonicalizes labels to NFC before comparing them, so a
// name rendered in either spelling has to be cut at the same place and render
// the same width. Comparing the results byte for byte would be wrong: NFC "é"
// is two bytes and NFD "e" plus a combining acute is three, so an uncut word
// comes back differently encoded while rendering identically. What has to hold
// is that the visible text and the width agree.
func TestShortenCutsAlikeAcrossNormalizationForms(t *testing.T) {
	words := []string{"café", "naïve", "résumé", "ÀÉÎÕÜ", "日本語"}
	for _, w := range words {
		nfc, nfd := unorm.NFC.String(w), unorm.NFD.String(w)
		if lipgloss.Width(nfc) != lipgloss.Width(nfd) {
			t.Fatalf("%q renders %d cells as NFC and %d as NFD", w,
				lipgloss.Width(nfc), lipgloss.Width(nfd))
		}
		for n := 1; n <= lipgloss.Width(nfc)+2; n++ {
			a, b := shorten(nfc, n), shorten(nfd, n)
			if strings.TrimSuffix(a, "…") == "" {
				continue
			}
			if unorm.NFC.String(a) != unorm.NFC.String(b) {
				t.Errorf("shorten(%q, %d) cuts the two spellings apart: %q (NFC) vs %q (NFD)", w, n, a, b)
			}
		}
	}
}

// splitGraphemeClusters splits s into grapheme clusters, the unit shorten
// cuts on.
func splitGraphemeClusters(s string) []string {
	var out []string
	state := -1
	for rest := s; rest != ""; {
		var c string
		c, rest, _, state = uniseg.FirstGraphemeClusterInString(rest, state)
		out = append(out, c)
	}
	return out
}