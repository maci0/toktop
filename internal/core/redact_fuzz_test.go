// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzRedactHome drives both home redactors with arbitrary bytes. The text
// they are handed is whatever a tool, an engine or a remote host wrote: a
// shell error quoting a path, an ssh failure quoting the peer's stderr, a
// JSON report destined for an issue. Whatever it contains, the account name
// must be gone from the result, the scanner must never split a rune while
// matching a home spelled in a width it folds to, and a message that carries
// no home must come back byte for byte.
func FuzzRedactHome(f *testing.F) {
	for _, seed := range []struct{ user, msg string }{
		{"", ""},
		{"", "nothing to redact here"},
		{"me", ""},
		{"me", "/home/me"},
		{"me", "/home/me/"},
		{"me", "/home/me/.bashrc: No such file"},
		{"me", "/home/mem/cache and /home/me-too/state"},
		{"me", "/home/me" + "\n/home/me"},
		{"me", "C:\\Users\\me\\notes.txt"},
		{"c:\\users\\ME\\notes.txt", "prefix"},
		{"me", "C:\\Users\\me-too"},
		{"me", "Z:\\Users\\me\\x"},
		{"me", "/Users/me/Library/Caches"},
		{"me", `/Users\me\x`},
		{"me", "/home/ME/x and /HOME/me/y"},
		{"me", "/home/mÉ"},
		{"me", "/home/me\u0301/x"},
		{"me", "/home/\u212a/x"},
		{"me", "/home/K/x"},
		{"me", "ſomewhere"},
		{"me", "/home/me\u00a0trailing"},
		{"me", "/home/me\x00\x01"},
		{"me", strings.Repeat("/home/me/", 64)},
		{"me", strings.Repeat("a", 4096) + "/home/me"},
		{"me", "\xff\xfe/home/me/\xc0\x80"},
		{"me", "\x1b]0;/home/me\x07title"},
		{".", "/home/./x"},
		{"..", "/home/../x"},
		{"a/b", "/home/a/b/x"},
		{`a\b`, `/home/a\b/x`},
		{`c:\`, `C:\Users\me`},
		{" ", "/home/ /x"},
		{"me ", "/home/me /x"},
		{"me", "/home/me-" + strings.Repeat("é", 200)},
		{"me", "/home/me/very/deep/path/that/keeps/going/for/a/while/indeed"},
		{"dev", "/home/dev/proj: permission denied"},
		{"dev", "/home/developer"},
		{"dev", "  /home/dev  "},
		{"dev", "/home/dev/"},
		{"dev", "/home/dev\n"},
		{"dev", "/home/dev//"},
		{"dev", "text /home/dev more text /home/dev end"},
	} {
		f.Add(seed.user, seed.msg)
	}

	redactableHome := func() (string, bool) {
		home, err := os.UserHomeDir()
		if err != nil || !filepath.IsAbs(home) {
			return "", false
		}
		home = filepath.Clean(home)
		return home, filepath.Dir(home) != home
	}

	f.Fuzz(func(t *testing.T, user, msg string) {
		got := RedactUserHome(user, msg)
		if utf8.ValidString(msg) && !utf8.ValidString(got) {
			// The match walks rune by rune, so a home spelled in a width it
			// folds to must never leave the scan between a rune's bytes.
			t.Fatalf("RedactUserHome(%q, %q) split a rune: %q", user, msg, got)
		}
		if again := RedactUserHome(user, msg); again != got {
			t.Fatalf("RedactUserHome(%q, %q) is not deterministic: %q then %q", user, msg, got, again)
		}
		// A name that is not one path component names no account, so the text
		// it was given has to survive untouched.
		if user == "" || user == "." || user == ".." || strings.ContainsAny(user, `/\:`) {
			if got != msg {
				t.Fatalf("RedactUserHome(%q, %q) rewrote text for a name that is not an account: %q", user, msg, got)
			}
			return
		}
		// What is left of the account name is always inside a longer name:
		// "/home/me-too" belongs to someone else and keeps its spelling. The
		// scanner is the only thing standing between a redactor and a report
		// that names every account on the host.
		assertNoBareUserHome(t, user, msg, got)

		home, ok := redactableHome()
		if !ok {
			return
		}
		out := RedactHome(msg)
		if utf8.ValidString(msg) && !utf8.ValidString(out) {
			t.Fatalf("RedactHome(%q) split a rune: %q", msg, out)
		}
		if again := RedactHome(msg); again != out {
			t.Fatalf("RedactHome(%q) is not deterministic: %q then %q", msg, out, again)
		}
		// Every spelling the local home can reach a message with, separator
		// and all, must be absent from the result.
		for _, spelling := range homeSpellings(home) {
			full := normalizeSpelling(spelling) + string(filepath.Separator)
			if strings.Contains(out, full) {
				t.Fatalf("RedactHome(%q) left the home path in %q", msg, out)
			}
		}
		if !foldsHomeSpelling() && len(out) > len(msg) {
			t.Fatalf("RedactHome grew the string: %d -> %d (%q from %q)", len(msg), len(out), out, msg)
		}
	})
}

// homeSpellings lists the ways a path to the local home can be written into
// a message: the platform spelling, and the slash spelling on the platform
// that has one separator for a different one.
func homeSpellings(home string) []string {
	out := []string{home}
	if slash := filepath.ToSlash(home); slash != home {
		out = append(out, slash)
	}
	return out
}

func foldsHomeSpelling() bool {
	return runtime.GOOS == "windows" || runtime.GOOS == "darwin"
}

// assertNoBareUserHome checks that the account name survives in the output
// only where a longer name continues it.
func assertNoBareUserHome(t *testing.T, user, msg, got string) {
	t.Helper()
	spelled := normalizeSpelling(user)
	for _, prefix := range userHomePrefixes {
		home := prefix + spelled
		scan := normalizeSpelling(got)
		for {
			at, n, ok := indexFold(scan, home)
			if !ok {
				break
			}
			rest := scan[at+n:]
			if !nameContinues(rest) {
				t.Fatalf("RedactUserHome(%q, %q) left the account name in %q", user, msg, got)
			}
			scan = scan[at+len(home):]
		}
	}
}
