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
	"sync/atomic"
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
	// Now reads the clock the deadline and the stale age are measured on, and
	// Sleep is the wait between polls. A nil half is wall-clock time, which is
	// production. A driver supplies a virtual pair, so the polls, the instant
	// the deadline is reached and the instant a lock ages past Stale are steps
	// it took rather than how long the process happened to block: the same
	// contention then breaks and gives up at the same steps on every run,
	// which is the only way the give-up and the break can be replayed.
	Now   func() time.Time
	Sleep func(time.Duration)
}

// clock returns the timing the policy runs on, substituting wall-clock time for
// either half a caller left nil.
func (p Policy) clock() (func() time.Time, func(time.Duration)) {
	now, sleep := p.Now, p.Sleep
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	return now, sleep
}

// Raw prints a path as it is. A caller whose messages hide the home
// directory passes core.RedactHome in its place.
func Raw(s string) string { return s }

// redactPathErr returns err with the path it names printed through redact.
//
// The os calls below all fail with a *fs.PathError, whose message is its own
// path spelled out in full. Wrapping one with %w next to a redacted path
// therefore prints the redacted path and then the same path unredacted one
// clause later, and the account name the redaction exists to remove is in the
// message either way. The operation and the cause are carried over intact, so
// errors.Is and errors.As still reach the reason the call failed.
func redactPathErr(redact func(string) string, err error) error {
	var pe *fs.PathError
	if !errors.As(err, &pe) {
		return err
	}
	return &fs.PathError{Op: pe.Op, Path: redact(pe.Path), Err: pe.Err}
}

// With runs fn while holding the lock file at lock and releases it on the way
// out. owner names what the lock protects, and redact maps a path to the form
// the failure messages print.
//
// A lock that cannot be released is reported rather than dropped: the next
// caller spends p.Wait on it before breaking it as stale, and an operator who
// never learns why has no way to act. The result is named so the deferred
// release can fold its own failure into whatever fn returned.
//
// The waiting is paced on p.Now and p.Sleep, so a driver supplies a virtual
// pair and the wait, the stale break and the give-up land on the steps it took
// rather than on real time.
func With(lock, owner string, p Policy, redact func(string) string, fn func() error) (err error) {
	now, sleep := p.clock()
	deadline := now().Add(p.Wait)
	for {
		// The token names this acquisition, and the release below removes the
		// lock only while the file still carries it.
		token := newToken()
		f, cerr := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if cerr == nil {
			_, werr := f.WriteString(token)
			if closeErr := f.Close(); werr == nil {
				werr = closeErr
			}
			if werr != nil {
				// A lock file that survives the failed write makes every
				// later caller report a lock held by no process, so the
				// failure to clear it rides along with the failure that
				// left it there.
				lerr := fmt.Errorf("cannot write the lock %s: %w", redact(lock), redactPathErr(redact, werr))
				if rerr := os.Remove(lock); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
					lerr = errors.Join(lerr,
						fmt.Errorf("left a lock file at %s that must be deleted: %w", redact(lock), redactPathErr(redact, rerr)))
				}
				return lerr
			}
			defer func() {
				// Only while the file is still this acquisition's. The break
				// below hands the lock to a peer that is inside its own
				// critical section by then, and an unconditional remove here
				// deleted that peer's lock instead: the process that broke
				// the stale lock was still running, so two holders were in
				// the section at once and the third caller to arrive walked
				// straight in beside both. That is the read-modify-write race
				// the lock exists to close, and the host-key store and the
				// install are both read-modify-writes. A lock whose token has
				// moved on belongs to someone else and is left alone; the
				// peer breaks it on its own stale policy.
				if !lockIs(lock, token) {
					return
				}
				if rerr := os.Remove(lock); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
					err = errors.Join(err, fmt.Errorf("cannot release the lock at %s: %w",
						redact(lock), redactPathErr(redact, rerr)))
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
		if info, serr := os.Stat(lock); serr == nil && core.Age(now(), info.ModTime()) > p.Stale {
			if breakErr = os.Remove(lock); breakErr == nil {
				continue
			}
		}
		if now().After(deadline) {
			if breakErr != nil {
				return fmt.Errorf("%s is locked by another toktop; the stale lock at %s could not be removed: %w",
					redact(owner), redact(lock), redactPathErr(redact, breakErr))
			}
			return fmt.Errorf("%s is locked by another toktop; giving up after %s", redact(owner), p.Wait)
		}
		sleep(p.Poll)
	}
}

// newToken names one acquisition of the lock. The process id says which
// process holds it, and the counter says which of its acquisitions, so a
// process that takes the lock twice in a row never writes the same token
// twice: a holder comparing its own token against the file cannot mistake a
// re-acquisition for the one it is releasing.
//
// The value is diagnostic. Nothing parses it, and a lock whose file cannot be
// read is treated as held by someone else rather than as free, so a token
// nobody wrote costs a wait and a stale break, not a lost lock.
func newToken() string {
	return fmt.Sprintf("toktop-lock %d %d\n", os.Getpid(), nextToken.Add(1))
}

// nextToken numbers the acquisitions within one process, so two With calls
// from the same process carry different tokens.
var nextToken atomic.Uint64

// lockIs reports whether the lock file at path still carries token, which is
// how a holder tells its own lock from the one a stale break handed to a peer.
//
// A file that cannot be read answers false. That is the safe direction: the
// release then leaves it alone, and the holder reports nothing while the
// lock ages out and the next caller breaks it, rather than the release
// unlinking a lock whose owner it cannot identify.
func lockIs(path, token string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return string(b) == token
}
