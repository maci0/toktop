// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build !darwin && !windows

package agentusage

import (
	"path/filepath"
	"slices"
	"testing"
)

// Away from macOS, normalization forms are different directories, so the
// spelling list must stay exactly what was asked for: one entry per distinct
// path, no synthesized variants.
func TestDirSpellingsHaveNoSynthesizedVariants(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "caf\u00e9") // NFC
	got := dirSpellings(dir)
	want := []string{dir} // TempDir is absolute and has no symlink to resolve differently
	if resolved := resolveDir(dir); resolved != dir {
		want = append(want, resolved)
	}
	if !slices.Equal(got, want) {
		t.Errorf("dirSpellings = %q, want exactly %q", got, want)
	}

	if !sameSpelling(dir, dir) || sameSpelling(dir, filepath.Join(t.TempDir(), "other")) {
		t.Error("sameSpelling must be byte equality here")
	}
	if v := dirVariants(dir); !slices.Equal(v, []string{dir}) {
		t.Errorf("dirVariants = %q, want the input unchanged", v)
	}
	if got := dirKey(dir); got != dir {
		t.Errorf("dirKey(%q) = %q, want the path unchanged here", dir, got)
	}
	if got := dirKey("/Users/Dev/proj"); got != "/Users/Dev/proj" {
		t.Errorf("dirKey folded case = %q; two differently cased names are two directories here", got)
	}
}
