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

func TestDirVariantsCoverBothNormalizationForms(t *testing.T) {
	nfc := "caf\u00e9"  // precomposed
	nfd := "cafe\u0301" // e + combining acute

	got := dirVariants(nfc)
	if !slices.Equal(got, []string{nfc, nfd}) {
		t.Errorf("dirVariants(nfc) = %q, want [%q %q]", got, nfc, nfd)
	}

	got = dirVariants(nfd)
	if !slices.Equal(got, []string{nfd, nfc}) {
		t.Errorf("dirVariants(nfd) = %q, want [%q %q]", got, nfd, nfc)
	}

	// Pure ASCII has nothing to vary; it must not grow variants.
	if got := dirVariants("/Users/mw/projects"); !slices.Equal(got, []string{"/Users/mw/projects"}) {
		t.Errorf("dirVariants(ascii) = %q, want a single spelling", got)
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
