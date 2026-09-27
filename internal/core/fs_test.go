// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SyncDir is best effort by contract: a write that already renamed its file
// into place must not be reported as failed because the directory could not
// be flushed, or the caller would trade a durable write for none. A directory
// that cannot be opened at all has to be a no-op rather than a panic.
func TestSyncDirNeverFails(t *testing.T) {
	SyncDir(t.TempDir())
	SyncDir("")
	SyncDir(t.TempDir() + "/absent")
}

// ExpandHome is the shared tilde expansion for an ssh_config IdentityFile and
// an agent transcript root, so its edges decide what those two read. A "~"
// inside a path is a real character in a filename and must survive, and an
// empty result would silently drop the file the caller named.
func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory to expand against: %v", err)
	}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"a bare tilde", "~", home},
		{"a tilde and a slash", "~/keys/gpu", filepath.Join(home, "keys/gpu")},
		{"a tilde and a backslash", `~\keys\gpu`, filepath.Join(home, `keys\gpu`)},
		{"a tilde in the middle", "/a/~/b", "/a/~/b"},
		{"a tilde inside a name", "a~b", "a~b"},
		{"a tilde user is not expanded", "~other/x", "~other/x"},
		{"an absolute path", "/etc/ssh/config", "/etc/ssh/config"},
		{"a relative path", "keys/gpu", "keys/gpu"},
		{"empty", "", ""},
		{"a bare dot", ".", "."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ExpandHome(c.in)
			if got != c.want {
				t.Fatalf("ExpandHome(%q) = %q, want %q", c.in, got, c.want)
			}
			if strings.HasPrefix(c.in, "~/") && !strings.HasPrefix(got, home) {
				t.Errorf("ExpandHome(%q) = %q, want it under the home directory", c.in, got)
			}
		})
	}
}
