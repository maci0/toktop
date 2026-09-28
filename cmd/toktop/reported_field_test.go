// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package main

import (
	"strings"
	"testing"

	"github.com/rivo/uniseg"
)

// Both startup warnings print names this program did not write: a key out of
// ~/.gauntlet/agents.json and a name out of the environment. The escape
// sequence below sets a terminal title, and the bidi and zero-width marks
// beside it render one name as another. SanitizeText takes all of it, so
// reportedField is the only thing standing between a wrapper script and the
// operator's clipboard.
func TestReportedFieldStripsTerminalControl(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"osc title", "k\x1b]0;toktop\x07s", "ks"},
		{"csi redraw", "\x1b[2Jk", "k"},
		{"bidi override", "k\u202Etxt", "ktxt"},
		{"zero width space", "k\u200b", "k"},
		{"newlines fold", "a\nb", "a b"},
		{"carriage return", "a\rb", "ab"},
		{"control byte", "a\x07b", "ab"},
		{"plain name unchanged", "prompt_cache_read", "prompt_cache_read"},
		// An ill-formed byte is dropped rather than printed, so a name cannot
		// reach the terminal as something the terminal will not render.
		{"ill-formed utf8", "a\xffb", "ab"},
	}
	for _, c := range cases {
		if got := reportedField(c.in); got != c.want {
			t.Errorf("%s: reportedField(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// A name is externally supplied and the startup output is pasted into issues,
// so one name cannot spend an unbounded share of a line. The cap is counted in
// user-perceived characters, so the names that reach it with the fewest code
// points are the ones that would overflow first: a decomposed accent and a ZWJ
// emoji, which are one character in two and three code points.
func TestReportedFieldCapsOnGraphemeClusters(t *testing.T) {
	emoji, accented := "\U0001f469‍\U0001f4bb", "é"
	for _, unit := range []string{emoji, accented} {
		got := reportedField(strings.Repeat(unit, maxReportedName+10))
		if n := uniseg.GraphemeClusterCount(got); n != maxReportedName {
			t.Errorf("reportedField on %d %q clusters kept %d, want %d",
				maxReportedName+10, unit, n, maxReportedName)
		}
		// Every character kept is whole, so the result is exactly the first
		// cap of them: no dangling joiner, no letter stripped of its accent.
		if want := strings.Repeat(unit, maxReportedName); got != want {
			t.Errorf("reportedField on %q clusters = %q, want %q", unit, got, want)
		}
	}
	// A name already inside the cap is untouched.
	if got := reportedField("prompt_cache_read"); got != "prompt_cache_read" {
		t.Errorf("reportedField(%q) = %q, want it unchanged", "prompt_cache_read", got)
	}
}
