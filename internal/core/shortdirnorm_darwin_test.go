// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build darwin

package core

import (
	"path/filepath"
	"testing"
)

// A home directory macOS stored decomposed is the same account as the
// composed spelling an agent carries in its working directory. Compared byte
// for byte the relation came back "..", the home was not stripped, and the
// note kept the whole absolute path with the account name in it.
func TestShortDirFoldsAcrossUnicodeNormalization(t *testing.T) {
	decomposed := absPath("Users", "re\u0301ne", "private-user")
	composed := absPath("Users", "r\u00e9ne", "private-user")
	if decomposed == composed {
		t.Skip("the temp path is already normalized; nothing to fold")
	}
	setHome(t, decomposed)

	project := filepath.Join(composed, "src", "project")
	want := "~/src/project"
	if got := ShortDir(project); got != want {
		t.Fatalf("ShortDir(%q) = %q, want %q", project, got, want)
	}
	// The reverse spelling names the same directory, and so does home itself.
	if got := ShortDir(filepath.Join(decomposed, "src", "project")); got != want {
		t.Errorf("ShortDir(decomposed) = %q, want %q", got, want)
	}
	if got := ShortDir(composed); got != "~" {
		t.Errorf("ShortDir(home) = %q, want %q", got, "~")
	}
	// A directory outside home keeps the last-two-components rule.
	if got := ShortDir(absPath("srv", "work", "app")); got != "work/app" {
		t.Errorf("ShortDir(outside home) = %q, want %q", got, "work/app")
	}
}
