// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import "testing"

// DirKey is what a caller keys a map by, SameDir what it compares with, and a
// caller doing both needs the two to agree: a map keyed by DirKey holds one
// entry exactly when SameDir calls the two spellings one directory. The test
// states that in terms of SameDir, so it holds on every platform, and the
// per-platform files pin what folding each one does on top of it.
func TestDirKeyAgreesWithSameDir(t *testing.T) {
	spellings := [][2]string{
		{"/Users/dev/proj", "/Users/dev/proj"},
		{"/Users/dev/proj/", "/Users/dev/proj"},
		{"/Users/dev/café", "/Users/dev/café"}, // NFC against NFD
		{"/Users/Foo/proj", "/Users/foo/proj"},
		{"/Users/dev/proj", "/Users/dev/other"},
	}
	for _, s := range spellings {
		same, oneKey := SameDir(s[0], s[1]), DirKey(s[0]) == DirKey(s[1])
		if same != oneKey {
			if same {
				t.Errorf("SameDir(%q, %q) is true but DirKey differs: a map keyed by DirKey would count one directory twice", s[0], s[1])
			} else {
				t.Errorf("DirKey(%q) == DirKey(%q) but SameDir is false: a map keyed by DirKey would count one directory as two", s[0], s[1])
			}
		}
	}
}

// Folding has to settle in one pass: a key built from a directory that a
// caller already folded must be the same key again.
func TestDirKeyIsIdempotent(t *testing.T) {
	for _, p := range []string{"/Users/dev/proj/", "/Users/Dev/proj", "C:\\Users\\dev\\proj", "/home/dev/café"} {
		once := DirKey(p)
		if twice := DirKey(once); twice != once {
			t.Errorf("DirKey(%q) = %q, then DirKey of that = %q", p, once, twice)
		}
	}
}
