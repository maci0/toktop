// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// SyncDir is best effort by contract: a write that already renamed its file
// into place must not be reported as failed because the directory could not
// be flushed, or the caller would trade a durable write for none. A directory
// that cannot be opened at all has to be a no-op rather than a panic.
func TestSyncDirNeverFails(t *testing.T) {
	dir := t.TempDir()
	// The caller's write is what the sync protects, so a directory whose
	// entry is flushed still has to hold the file, byte for byte.
	name := filepath.Join(dir, "state.json")
	if err := os.WriteFile(name, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	SyncDir(dir)
	if b, err := os.ReadFile(name); err != nil || string(b) != `{"ok":true}` {
		t.Fatalf("after SyncDir: %q, %v; want the file unchanged", b, err)
	}
	// Every shape that cannot be opened has to come back rather than panic,
	// including a path whose parent does not exist and a file used as a
	// directory.
	SyncDir("")
	SyncDir(filepath.Join(dir, "absent"))
	SyncDir(name)
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

// SweepStaleTemps deletes off disk, on the self-update and known-hosts write
// paths, so the age gate is the whole contract: a staging file younger than
// StaleTempAge may still be a write in flight, and a file carrying another
// prefix belongs to someone else. Both survive; only the aged one goes.
func TestSweepStaleTempsRemovesOnlyAgedStagingFiles(t *testing.T) {
	dir := t.TempDir()
	// A fixed sweep instant, so the ages below are the ones the test names
	// rather than however long the setup took.
	sweep := time.Now()
	write := func(name string, age time.Duration) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		when := sweep.Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
		return path
	}
	aged := write("known_hosts.tmp-old", StaleTempAge+time.Hour)
	fresh := write("known_hosts.tmp-new", time.Hour)
	other := write("known_hosts", StaleTempAge+time.Hour)
	sub := filepath.Join(dir, "known_hosts.tmp-dir")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sub, sweep.Add(-StaleTempAge-time.Hour), sweep); err != nil {
		t.Fatal(err)
	}

	SweepStaleTemps(dir, "known_hosts.tmp", sweep)

	if _, err := os.Stat(aged); !os.IsNotExist(err) {
		t.Errorf("the aged staging file survived the sweep: %v", err)
	}
	// A directory is never a staging file, however old or however prefixed.
	for _, kept := range []string{fresh, other, sub} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s was swept: %v", filepath.Base(kept), err)
		}
	}
}

// A directory the caller cannot read is not a reason to fail the write that
// is about to happen: the sweep runs before it.
func TestSweepStaleTempsOnMissingDir(t *testing.T) {
	SweepStaleTemps(filepath.Join(t.TempDir(), "absent"), "toktop.tmp", time.Now())
}

// DiscardStaged decides what a caller is told about a staging file left behind
// by a failed write, so both branches are pinned: a success must not report a
// removal, and a failure must not swallow one.
func TestDiscardStaged(t *testing.T) {
	dir := t.TempDir()
	leaked := filepath.Join(dir, "stage.tmp")
	if err := os.WriteFile(leaked, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A write that failed has to report the leftover; an operator told only
	// that the write failed has no way to know the directory now holds one.
	boom := errors.New("checksum mismatch")
	err := DiscardStaged(leaked, boom)
	if !errors.Is(err, boom) {
		t.Errorf("the failure it triggered was lost: %v", err)
	}
	if _, serr := os.Stat(leaked); !os.IsNotExist(serr) {
		t.Errorf("the staging file survived: %v", serr)
	}

	// A write that succeeded renamed the file away, so nothing is left and
	// nothing is reported.
	if err := DiscardStaged(leaked, nil); err != nil {
		t.Errorf("a succeeded write reported %v", err)
	}
}
