// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build darwin

package agentusage

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// dirVariants must hand a lookup every spelling the volume resolves to the
// directory it was given: each entry has to be one spellingEqual accepts, the
// given spelling has to stay first for callers that prefer it, and the fold
// dirKey maps the path to has to be among them, so the form a map collapses
// two spellings into is a form a lookup actually tries.
func checkDirVariants(t *testing.T, p string, wantAny ...string) {
	t.Helper()
	got := dirVariants(p)
	if got[0] != p {
		t.Errorf("dirVariants(%q)[0] = %q, want the given spelling first", p, got[0])
	}
	for _, w := range slices.Concat(wantAny, []string{dirKey(p)}) {
		if !slices.Contains(got, w) {
			t.Errorf("dirVariants(%q) = %q, missing %q", p, got, w)
		}
	}
	for _, v := range got {
		if !sameSpelling(p, v) {
			t.Errorf("dirVariants(%q) added %q, which the volume would not resolve", p, v)
		}
	}
}

func TestDirVariantsCoverNormalizationAndCase(t *testing.T) {
	// A volume is reached by any normalization form and any case, so a session
	// recorded from "Users/Foo" is found from the spelling the watcher
	// resolved, and an accented name from either normalization form.
	checkDirVariants(t, "/Users/Foo/Caf\u00e9",
		"/Users/Foo/Caf\u00e9",  // NFC
		"/Users/Foo/Cafe\u0301", // NFD
	)
	checkDirVariants(t, "caf\u00e9", "cafe\u0301")

	// A path already in folded form with nothing to decompose adds nothing.
	if got, want := dirVariants("/users/mw/projects"), 1; len(got) != want {
		t.Errorf("dirVariants(folded ascii) = %q, want %d spelling", got, want)
	}
}

func TestSameSpellingAcrossNormalizationForms(t *testing.T) {
	if !sameSpelling("caf\u00e9", "cafe\u0301") {
		t.Error("NFC and NFD spellings of one name must match on darwin")
	}
	if !sameSpelling("/Users/Foo/proj", "/Users/foo/proj") {
		t.Error("case-insensitive APFS default: case must fold")
	}
	if sameSpelling("/home/café", "/home/other") {
		t.Error("different names must not match")
	}
}

func TestDirKeyFoldsCaseAndNormalization(t *testing.T) {
	if got, want := dirKey("/Users/Foo/café"), "/users/foo/café"; got != want {
		t.Errorf("dirKey = %q, want %q", got, want)
	}
}

// The full spelling list for a non-ASCII directory must reach every
// normalization form an agent could have recorded.
func TestDirSpellingsIncludeNormalizationVariants(t *testing.T) {
	base := t.TempDir()
	nfc := filepath.Join(base, "caf\u00e9")
	nfd := filepath.Join(base, "cafe\u0301")
	for _, d := range []string{nfc, nfd} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	got := dirSpellings(nfc)
	if !slices.Contains(got, nfd) {
		t.Errorf("dirSpellings(%q) = %q, missing the NFD spelling", nfc, got)
	}
	if slices.Contains(got, "") {
		t.Errorf("dirSpellings produced an empty entry: %q", got)
	}
}
