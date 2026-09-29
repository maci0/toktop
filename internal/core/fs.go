// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ExpandHome expands a leading tilde in a path the way a shell would: "~" to
// the home directory, "~/x" (and "~\x" on a Windows config file) to a path
// under it. Anything else, and any path whose home lookup fails, is returned
// unchanged: expanding to an empty base would silently drop the key or
// transcript root the caller named.
//
// It lives here because two packages expand a leading tilde in paths read from
// user-editable files (an ssh_config IdentityFile and an agent transcript
// root); one helper is the only way those two cannot drift apart.
func ExpandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}

// SyncDir flushes the directory entry a rename into place created. The file's
// contents are already fsynced by the writer, but a rename is a change to the
// directory: without this the rename itself can be lost to a crash, leaving
// the previous file in place after the write reported success.
//
// It lives here because two packages write by rename and must both make the
// rename durable (the ssh host-key pin store and the self-update install);
// one helper is the only way those two cannot drift apart.
//
// The reason it could not be flushed is returned, not dropped. A caller that
// reported its write as successful while the flush failed reported a durability
// it never had: the rename is what a crash loses, and the file the writer
// fsynced is not the name the reader opens. Deciding what an unflushable
// directory means belongs to the caller, because it differs per write: a
// renamed binary that does not survive a boot leaves nothing to run, while a
// store a peer can rebuild is a different problem. A platform with no
// directory sync to call is the caller's to recognize, not this helper's to
// hide.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// StaleTempAge is how old a leftover staging file has to be before the next
// write removes it. A kill between CreateTemp and the rename leaves one in
// the directory forever, since nothing else ever looks for it. The age gate
// is what keeps the sweep from deleting a staging file another process is
// still writing.
const StaleTempAge = 24 * time.Hour

// SweepStaleTemps removes staging files an earlier write did not get to rename
// away, where prefix names the staging files of the caller. Anything it cannot
// remove is left alone. Callers serialize their writers, so within one process
// only the crashed runs of earlier sessions are ever this old.
//
// The ages are measured against now, so a driver decides which leftovers a
// write sweeps instead of the sweep following the wall clock: which staging
// files are swept has to be a step the run took, or a write that deletes a
// peer's in-flight staging file and a write that spares it replay differently.
func SweepStaleTemps(dir, prefix string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := now.Add(-StaleTempAge)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// DiscardStaged unlinks the staging file at name on the way out of a write and
// returns err unchanged.
//
// A staging file that survives a failed write is unverified content sitting
// where the operator looks for the real file, so a removal that fails is
// reported alongside the failure that triggered it rather than swallowed: told
// only that the write failed, the operator has no way to know the directory
// now holds one. The same holds after a write that reported success. A rename
// that landed is the common case and leaves nothing at name, but a writer that
// decides on its own not to rename (an install whose target already carries the
// release checksum) hands here a staging file that is still there, and one that
// cannot be unlinked is content the operator was never shown, at a path they
// were never given.
//
// It lives here because two packages stage a file next to its destination and
// must both clean up the same way (the ssh host-key pin store and the
// self-update install); one helper is the only way those two cannot drift.
func DiscardStaged(name string, err error) error {
	rerr := os.Remove(name)
	if rerr == nil || errors.Is(rerr, fs.ErrNotExist) {
		return err
	}
	return errors.Join(err,
		fmt.Errorf("left a staging file at %s that must be deleted: %w", name, rerr))
}
