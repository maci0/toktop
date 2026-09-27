// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build darwin

package core

import (
	"path/filepath"
	"testing"
)

// A home directory macOS stored decomposed is the same account as the
// composed spelling a process carries in its diagnostics. Comparing the two
// byte for byte left the account name in the message, which is the one thing
// RedactHome exists to prevent.
func TestRedactHomeFoldsAcrossUnicodeNormalization(t *testing.T) {
	decomposed := absPath("Users", "re\u0301ne", "private-user")
	composed := absPath("Users", "r\u00e9ne", "private-user")
	if decomposed == composed {
		t.Skip("the temp path is already normalized; nothing to fold")
	}
	setHome(t, decomposed)

	sep := string(filepath.Separator)
	msg := "open " + filepath.Join(composed, ".toktop", "toktop.log") + ": permission denied"
	// Only the home prefix is rewritten; the text around it is the caller's.
	want := "open ~" + sep + filepath.Join(".toktop", "toktop.log") + ": permission denied"
	if got := RedactHome(msg); got != want {
		t.Fatalf("RedactHome(%q) = %q, want %q", msg, got, want)
	}
	// The bare home, spelled the other way round, names the same directory.
	if got := RedactHome(composed); got != "~" {
		t.Errorf("RedactHome(%q) = %q, want %q", composed, got, "~")
	}
	// A sibling directory that merely shares the composed prefix is a
	// different directory and must survive verbatim.
	sibling := filepath.Join(absPath("Users", "r\u00e9ne"), "other") + sep + "toktop.log"
	if got := RedactHome("open " + sibling); got != "open "+sibling {
		t.Errorf("RedactHome(%q) = %q, want it unchanged", "open "+sibling, got)
	}
}
