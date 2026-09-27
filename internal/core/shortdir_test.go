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
