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

// StaleTempAge is how old a leftover staging file has to be before the next
// write removes it. A kill between CreateTemp and the rename leaves one in
// the directory forever, since nothing else ever looks for it. The age gate
// is what keeps the sweep from deleting a staging file another process is
// still writing.
const StaleTempAge = 24 * time.Hour

// SweepStaleTemps removes staging files an earlier write did not get to rename
// away, where prefix names the staging files of the caller. Callers serialize
// their writers, so within one process only the crashed runs of earlier
// sessions are ever this old.
//
// The ages are measured against now, so a driver decides which leftovers a
// write sweeps instead of the sweep following the wall clock: which staging
// files are swept has to be a step the run took, or a write that deletes a
// peer's in-flight staging file and a write that spares it replay differently.
//
// A directory that cannot be listed is not an error: the write about to run
// needs that directory far more than the leftovers do, and it fails with the
// cause itself if it cannot write there. A staging file that cannot be
// unlinked is a different condition, and is reported, for the reason
// [DiscardStaged] gives: unverified content sits where the operator looks for
// the real file, and a sweep that dropped the refusal on the floor left them
// with a directory holding one and no line saying so. The failures of several
// files are joined into the one error, so a caller names every leftover rather
// than only the first.
func SweepStaleTemps(dir, prefix string, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	cutoff := now.Add(-StaleTempAge)
	var errs error
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		name := filepath.Join(dir, e.Name())
		if rerr := os.Remove(name); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			errs = errors.Join(errs,
				fmt.Errorf("left a staging file at %s that must be deleted: %w", name, rerr))
		}
	}
	return errs
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
