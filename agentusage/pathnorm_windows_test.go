// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build windows

package agentusage

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSameSpellingFoldsCaseAndSeparators(t *testing.T) {
	a := `C:\Users\Dev\project`
	b := `c:/users/dev/project`
	if !sameSpelling(a, b) {
		t.Errorf("sameSpelling(%q, %q) = false, want true", a, b)
	}
	if sameSpelling(a, `C:\Users\Dev\other`) {
		t.Error("different names must not match")
	}
}

func TestDirVariantsCoverSlashAndCase(t *testing.T) {
	p := `C:\Users\Dev\project`
	got := dirVariants(p)
	wantAny := []string{
		p,
		filepath.ToSlash(p),
		strings.ToLower(p),
		strings.ToLower(filepath.ToSlash(p)),
	}
	for _, w := range wantAny {
		if !slices.Contains(got, w) {
			t.Errorf("dirVariants(%q) = %q, missing %q", p, got, w)
		}
	}
}

func TestDirKeyCleansSlashesAndCase(t *testing.T) {
	if got, want := dirKey(`C:\Users\Dev\project\`), "c:/users/dev/project"; got != want {
		t.Errorf("dirKey = %q, want %q", got, want)
	}
}
