// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestShortDirKeepsTheIdentifyingPart(t *testing.T) {
	// Pin home away from the fixture paths so this is the last-two-components
	// rule alone, not the home-stripping one.
	elsewhere := t.TempDir()
	t.Setenv("HOME", elsewhere)
	t.Setenv("USERPROFILE", elsewhere)
	cases := map[string]string{
		"/home/dev/src/project": "src/project",
		"/home/dev/project":     "dev/project",
		"project":               "project",
		"/":                     "/",
		// A trailing separator is not a component: spending one of the two on
		// the cut leaves a slash in the note.
		"/var/log/":      "var/log",
		"/var/log":       "var/log",
		"/home/dev/app/": "dev/app",
		"//":             "/",
		// A backslash path is only a path where backslash is the separator.
		// Elsewhere it is one filename, kept whole minus the two components
		// the scan still finds in it.
		`C:\Users\dev\src\app`: `src\app`,
	}
	if runtime.GOOS == "windows" {
		cases[`C:\Users\dev\src\app`] = "src/app" // separators fold to '/'
		cases[`C:/Users/dev/project`] = "dev/project"
	}
	for in, want := range cases {
		if got := ShortDir(in); got != want {
			t.Errorf("ShortDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShortDirHidesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if got := ShortDir(home); got != "~" {
		t.Errorf("home itself = %q, want ~", got)
	}
	if got := ShortDir(""); got != "" {
		t.Errorf("empty = %q, want empty", got)
	}
	one := filepath.Join(home, "toktop")
	if got := ShortDir(one); got != "~/toktop" {
		t.Errorf("project in home = %q, want ~/toktop", got)
	}
	two := filepath.Join(home, "src", "toktop")
	if got := ShortDir(two); got != "src/toktop" {
		t.Errorf("nested under home = %q, want src/toktop", got)
	}
	outside := filepath.Join(filepath.Dir(home), "other", "proj")
	if got := ShortDir(outside); strings.Contains(got, filepath.Base(home)) {
		t.Errorf("path outside home still names home: %q", got)
	}
}

// A directory that is not on disk (removed mid-session, or not created yet)
// still lives under home, and the note must say so rather than printing the
// operator's username. The spellings only diverge when home is reached
// through a symlink, which on macOS is every temporary directory.
func TestShortDirHidesHomeForAPathNotOnDisk(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("HOME", link)
	t.Setenv("USERPROFILE", link)

	if got := ShortDir(filepath.Join(link, "toktop")); got != "~/toktop" {
		t.Errorf("project in home = %q, want ~/toktop", got)
	}
}

// relUnderFolded is only reached on the platforms whose file systems fold
// case, so it is tested directly to keep the walk under test on every one.
func TestRelUnderFolded(t *testing.T) {
	sep := string(filepath.Separator)
	home := strings.Join([]string{"Users", "dev"}, sep)
	cases := []struct {
		dir  string
		want string
		ok   bool
	}{
		{strings.Join([]string{"Users", "dev"}, sep), ".", true},
		{strings.Join([]string{"users", "dev"}, sep), ".", true},
		{strings.Join([]string{"users", "dev", "src", "toktop"}, sep), filepath.Join("src", "toktop"), true},
		{strings.Join([]string{"USERS", "DEV", "toktop"}, sep), "toktop", true},
		// A different directory that shares the home's first element is not
		// under home, however the two are spelled.
		{strings.Join([]string{"users", "dev2", "proj"}, sep), "", false},
		{strings.Join([]string{"users"}, sep), "", false},
		{"/var/log", "", false},
	}
	for _, c := range cases {
		got, ok := relUnderFolded(home, c.dir)
		if ok != c.ok || got != c.want {
			t.Errorf("relUnderFolded(%q, %q) = %q, %t; want %q, %t", home, c.dir, got, ok, c.want, c.ok)
		}
	}
}

// The end of the fold: a home reached with a different case is still the
// operator's home, and the note must not carry the account name. Windows is
// covered by filepath.Rel's own folding, so this pins the macOS walk.
func TestShortDirHidesHomeSpelledWithAnotherCase(t *testing.T) {
	if !lookupFoldsCase() {
		t.Skip("this platform compares path names by their bytes")
	}
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("HOME", link)
	t.Setenv("USERPROFILE", link)

	folded := swapCase(link)
	if folded == link {
		t.Skipf("%q has no letters to fold", link)
	}
	if got := ShortDir(filepath.Join(folded, "src", "toktop")); got != "src/toktop" {
		t.Errorf("nested under a home spelled another case = %q, want src/toktop", got)
	}
	if got := ShortDir(folded); got != "~" {
		t.Errorf("home itself spelled another case = %q, want ~", got)
	}
}

// swapCase flips the case of the ASCII letters in a path, leaving the rest
// alone. The result is a spelling of the same directory on the platforms
// that fold case, and a different one everywhere else.
func swapCase(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r - 'A' + 'a')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
