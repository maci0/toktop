// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build linux

package agentusage

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The kernel holds comm in TASK_COMM_LEN-1 (15) bytes and writes argv into
// cmdline verbatim, so a non-ASCII binary name arrives cut through a rune. A
// half rune must not reach the agent identity: canonicalTool composes to NFC
// and x/text leaves a lone 0xc3 there rather than dropping it, so the name
// would be keyed on bytes no other reader of the same process can produce.
func TestProcTextDropsIllFormedBytes(t *testing.T) {
	// "caf" + the first byte of "é" (0xc3 0xa9): exactly what the kernel
	// writes for a binary named "café" once the 15-byte comm limit cuts it.
	cut := []byte("caf\xc3")
	// Each case is the input and the text it must become: only the
	// ill-formed bytes go, every readable byte stays in place. An input
	// whose expected output is written out separately is what makes the
	// drop the property under test rather than a side effect of it.
	for _, tc := range []struct{ in, want string }{
		{string(cut), "caf"},
		{"\xffclaude\x00", "claude\x00"}, // argv[0] in an invalid locale
		{string(append(cut, 0, 'c', 'l', 'a', 'u', 'd', 'e', 0)), "caf\x00claude\x00"}, // cut comm, whole argv
		// A valid two-byte rune is not a half one and must survive whole.
		{"café", "café"},
		// A real Ã is two valid bytes, not the lead byte of one: dropping it
		// would lose an agent named for a character the walk can read.
		{"cafÃ", "cafÃ"},
	} {
		got := procText([]byte(tc.in))
		if !utf8.ValidString(got) {
			t.Errorf("procText(%q) = %q, ill-formed UTF-8 survived", tc.in, got)
			continue
		}
		if got != tc.want {
			t.Errorf("procText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := procText([]byte("claude\x00--dangerously-skip-permissions\x00")); got != "claude\x00--dangerously-skip-permissions\x00" {
		t.Errorf("procText on valid input = %q, want the input unchanged", got)
	}
}

// A cut name still matches nothing, but it must match nothing for the right
// reason: with the byte dropped, "claude" spelled with a cut rune is simply an
// unknown name, and canonicalTool returns it untouched rather than producing a
// key built on an ill-formed string.
func TestCanonicalToolOverCutNameIsValid(t *testing.T) {
	name := canonicalTool(strings.TrimSpace(procText([]byte("clau\xc3"))))
	if !utf8.ValidString(name) {
		t.Errorf("canonicalTool = %q, ill-formed UTF-8 reached the agent identity", name)
	}
	if knownNames()[name] {
		t.Errorf("canonicalTool = %q, a cut name must not alias a built-in agent", name)
	}
}
