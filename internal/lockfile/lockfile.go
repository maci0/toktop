// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

// Package lockfile takes the cross-process lock toktop holds before it
// rewrites a file another process may be reading: the known-host pin store
// and the installed binary. The lock is a file created exclusively and
// removed on release, because creation is atomic on every filesystem toktop
// runs on, which a lock over the target file itself is not on Windows.
package lockfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// Policy is the timing one caller takes its lock under. The constants differ
// per lock, not the rule, which is what With implements.
type Policy struct {
	// Wait bounds how long a caller waits for a peer to finish.
	Wait time.Duration
	// Poll is how often a waiting caller looks for the lock again.
	Poll time.Duration
	// Stale is how old a lock has to be before its holder is assumed to have
	// died holding it and the lock is broken.
	Stale time.Duration
}

// Raw prints a path as it is. A caller whose messages hide the home
// directory passes core.RedactHome in its place.
func Raw(s string) string { return s }

// With runs fn while holding the lock file at lock and releases it on the way
// out. owner names what the lock protects, and redact maps a path to the form
// the failure messages print.
//
// A lock that cannot be released is reported rather than dropped: the next
// caller spends p.Wait on it before breaking it as stale, and an operator who
// never learns why has no way to act. The result is named so the deferred
// release can fold its own failure into whatever fn returned.
func With(lock, owner string, p Policy, redact func(string) string, fn func() error) (err error) {
	deadline := time.Now().Add(p.Wait)
	for {
		f, cerr := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if cerr == nil {
			if err := f.Close(); err != nil {
				// A lock file that survives the failed write makes every
				// later caller report a lock held by no process, so the
				// failure to clear it rides along with the failure that
				// left it there.
				lerr := fmt.Errorf("cannot write the lock %s: %w", redact(lock), err)
				if rerr := os.Remove(lock); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
					lerr = errors.Join(lerr,
						fmt.Errorf("left a lock file at %s that must be deleted: %w", redact(lock), rerr))
				}
				return lerr
			}
			defer func() {
				if rerr := os.Remove(lock); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
					err = errors.Join(err, fmt.Errorf("cannot release the lock at %s: %w", redact(lock), rerr))
				}
			}()
			return fn()
		}
		if !os.IsExist(cerr) {
			// The directory is unwritable, or the filesystem has no
			// exclusive create. The work itself fails on its own with a
			// clearer error, so run it rather than reporting a lock error
			// the operator cannot act on.
			return fn()
		}
		// A stale lock is only retried once the break actually took. A lock
		// that cannot be unlinked (a read-only config dir, a peer recreating
		// it between the Stat and the Remove) would otherwise keep the stale
		// arm true and spin here with no sleep and no deadline check. The
		// reason the break failed is carried to the give-up message: without
		// it an unremovable lock is reported as one another toktop holds,
		// which is not true and leaves the operator with nothing to act on.
		var breakErr error
		// core.Age, not time.Since: the mtime carries no monotonic reading,
		// so this ages a wall clock, and a backward step (an NTP correction, a
		// resumed laptop) makes the age negative. A negative age is not
		// "older than the stale age", so the lock from a killed toktop is
		// never broken and the give-up below names a peer that is not
		// running.
		if info, serr := os.Stat(lock); serr == nil && core.Age(time.Now(), info.ModTime()) > p.Stale {
			if breakErr = os.Remove(lock); breakErr == nil {
				continue
			}
		}
		if time.Now().After(deadline) {
			if breakErr != nil {
				return fmt.Errorf("%s is locked by another toktop; the stale lock at %s could not be removed: %w",
					redact(owner), redact(lock), breakErr)
			}
			return fmt.Errorf("%s is locked by another toktop; giving up after %s", redact(owner), p.Wait)
		}
		time.Sleep(p.Poll)
	}
}
