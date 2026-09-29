// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package lockfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A caller that is inside its critical section when its lock is broken as
// stale must not remove the lock the breaker took. An unconditional release
// deleted the breaker's file on the way out, and the caller after that
// acquired a third lock and ran its read-modify-write beside both: the
// exclusion the lock exists to provide was gone, and the two callers it guards
// (the ssh host-key pin store and the self-update install) both lose a pin
// when two of them interleave.
func TestStaleBrokenHolderLeavesTheBreakersLockAlone(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "store.lock")
	policy := Policy{Wait: 2 * time.Second, Poll: 5 * time.Millisecond, Stale: 20 * time.Millisecond}

	held := make(chan struct{})
	release := make(chan struct{})
	// An empty lock file is what a holder of an older build left behind, and
	// the break keys on its mtime rather than its contents. Writing one here
	// and ageing it puts this process in the state a killed peer leaves, so
	// the second With is the breaker rather than a plain waiter.
	enter := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = With(lock, "store", policy, Raw, func() error {
			close(enter)
			<-held
			return nil
		})
	}()
	<-enter
	// Age the lock past the stale window while the holder is still inside it.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}

	breakerEntered := make(chan struct{})
	breakerDone := make(chan struct{})
	go func() {
		defer close(breakerDone)
		_ = With(lock, "store", policy, Raw, func() error {
			close(breakerEntered)
			<-release
			return nil
		})
	}()
	<-breakerEntered

	// The broken-on holder finishes while the breaker still holds the lock.
	close(held)
	wg.Wait()

	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("the lock the breaker holds was removed by the holder it displaced: %v", err)
	}
	// A third caller must still be kept out rather than walking in beside the
	// breaker, which is the state the unconditional release produced.
	entered := make(chan struct{})
	go func() {
		_ = With(lock, "store", Policy{Wait: 5 * time.Second, Poll: 5 * time.Millisecond, Stale: time.Hour}, Raw, func() error {
			close(entered)
			return nil
		})
	}()
	select {
	case <-entered:
		t.Fatal("a third caller entered the critical section while the breaker held it")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	<-breakerDone
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("the third caller never entered after the breaker released")
	}
}

// The ordinary path still works: the lock is gone once the holder returns, and
// a lock written with a token is one the next caller can take.
func TestLockIsReleasedWhenNothingBrokeIt(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "store.lock")
	policy := Policy{Wait: time.Second, Poll: 5 * time.Millisecond, Stale: time.Minute}
	if err := With(lock, "store", policy, Raw, func() error { return nil }); err != nil {
		t.Fatalf("first With: %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("lock still present after release: %v", err)
	}
	if err := With(lock, "store", policy, Raw, func() error { return nil }); err != nil {
		t.Fatalf("second With: %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("lock still present after the second release: %v", err)
	}
}

// Two acquisitions in one process carry different tokens, so a holder
// releasing its own lock cannot be confused by a re-acquisition of the same
// path.
func TestEachAcquisitionCarriesItsOwnToken(t *testing.T) {
	first, second := newToken(), newToken()
	if first == second {
		t.Fatal("two acquisitions of one process produced the same token")
	}
}

// testPolicy keeps the give-up arms short enough for a unit test and long
// enough that a scheduling hiccup does not read as a broken lock.
func testPolicy() Policy {
	return Policy{Wait: 200 * time.Millisecond, Poll: time.Millisecond, Stale: time.Minute}
}

// virtualOrigin is where the simulated timelines below start. A fixed instant
// rather than time.Now: the lock mtimes a test ages are set against it, so the
// stale break is decided on the same timeline every run rather than on how far
// the run happened to take.
var virtualOrigin = time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

// virtualClock is the timeline a simulated contention runs on. Sleep advances
// it instead of blocking, so a wait costs no wall time and the number of polls
// is a function of the policy alone: the same contention replays the same
// number of times on every run, which is the whole point of the seam.
type virtualClock struct {
	mu     sync.Mutex
	at     time.Time
	slept  []time.Duration
	origin time.Time
}

func newVirtualClock() *virtualClock {
	return &virtualClock{at: virtualOrigin, origin: virtualOrigin}
}

func (c *virtualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *virtualClock) Sleep(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.slept = append(c.slept, d)
	c.mu.Unlock()
}

// elapsed is how far the simulated timeline has moved, and polls how many times
// the loop slept to get there.
func (c *virtualClock) elapsed() (time.Duration, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at.Sub(c.origin), len(c.slept)
}

// onClock returns p running on this clock.
func (c *virtualClock) onClock(p Policy) Policy {
	p.Now, p.Sleep = c.Now, c.Sleep
	return p
}

// The wait loop is a function of the policy and the clock alone: run twice from
// the same origin against the same stuck holder and the two give up on the same
// poll of the same timeline. Under wall-clock time the poll count is a function
// of how long the process was descheduled, so a failure in the give-up could
// not be replayed from the instant it was seen.
func TestStuckHolderGivesUpOnTheSameStepEveryRun(t *testing.T) {
	p := Policy{Wait: 200 * time.Millisecond, Poll: 5 * time.Millisecond, Stale: time.Minute}
	run := func() (time.Duration, int, error) {
		dir := t.TempDir()
		lock := filepath.Join(dir, "store.lock")
		holdLock(t, lock)
		clock := newVirtualClock()
		ran := false
		err := With(lock, "known-host store", clock.onClock(p), Raw, func() error {
			ran = true
			return nil
		})
		if ran {
			t.Fatal("fn ran while another process held the lock")
		}
		elapsed, polls := clock.elapsed()
		return elapsed, polls, err
	}
	firstElapsed, firstPolls, firstErr := run()
	secondElapsed, secondPolls, secondErr := run()
	if firstErr == nil || secondErr == nil {
		t.Fatalf("With succeeded against a lock nobody released: %v / %v", firstErr, secondErr)
	}
	if firstElapsed != secondElapsed || firstPolls != secondPolls {
		t.Errorf("the same contention gave up at %s after %d polls and at %s after %d polls",
			firstElapsed, firstPolls, secondElapsed, secondPolls)
	}
	// The deadline is what ends the wait, not a scheduler: past Wait, and
	// within one poll of it, because the loop only looks at the clock between
	// polls.
	if firstElapsed <= p.Wait || firstElapsed > p.Wait+p.Poll {
		t.Errorf("gave up after %s; the deadline is %s and the loop checks it between polls of %s",
			firstElapsed, p.Wait, p.Poll)
	}
}

// A lock that ages past Stale on the simulated timeline is broken on the first
// attempt, with no poll at all. Wall-clock time cannot place a break that
// precisely: whether the break beat the deadline is a race between the test's
// ageing and the loop's first deadline check.
func TestStaleLockBreaksOnTheFirstPollOfAVirtualTimeline(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "store.lock")
	holdLock(t, lock)
	stale := virtualOrigin.Add(-2 * time.Minute)
	if err := os.Chtimes(lock, stale, stale); err != nil {
		t.Fatal(err)
	}
	clock := newVirtualClock()
	p := Policy{Wait: 200 * time.Millisecond, Poll: 5 * time.Millisecond, Stale: time.Minute}
	ran := false
	if err := With(lock, "store", clock.onClock(p), Raw, func() error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("a stale lock was not broken: %v", err)
	}
	if !ran {
		t.Error("fn never ran after the stale lock was broken")
	}
	if elapsed, polls := clock.elapsed(); elapsed != 0 || polls != 0 {
		t.Errorf("the break waited %s over %d polls; a lock already stale is taken at once", elapsed, polls)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("the lock outlived the broken critical section: %v", err)
	}
}

// holdLock takes the lock the way a peer process would and leaves it behind,
// so a test can contend with a holder that never releases.
func holdLock(t *testing.T, lock string) {
	t.Helper()
	f, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("plant the peer lock: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close the planted lock: %v", err)
	}
}

// The critical section runs, and the lock is gone on the way out: a lock file
// left behind makes every later caller report a holder that is not running.
func TestWithRunsAndReleases(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "store.lock")
	ran := false
	if err := With(lock, "store", testPolicy(), Raw, func() error {
		ran = true
		if _, err := os.Stat(lock); err != nil {
			t.Errorf("the lock was not held while fn ran: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("uncontended With: %v", err)
	}
	if !ran {
		t.Error("fn never ran")
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("the lock outlived the critical section: %v", err)
	}
}

// fn's own failure is the caller's answer, and the release still happens: a
// lock kept past a failed critical section is the wedge the whole package
// exists to avoid.
func TestWithReleasesOnAFailedCriticalSection(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "store.lock")
	want := errors.New("read-modify-write failed")
	err := With(lock, "store", testPolicy(), Raw, func() error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("With = %v, want the failure fn returned", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("a failed critical section left the lock behind: %v", err)
	}
}

// A lock held by a peer is waited on, not taken: the second caller runs only
// once the first is out, and never interleaves with it.
func TestWithWaitsForTheHolder(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "store.lock")
	release := make(chan struct{})
	held := make(chan struct{})
	go func() {
		_ = With(lock, "first", testPolicy(), Raw, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	ran := make(chan struct{})
	go func() {
		_ = With(lock, "second", testPolicy(), Raw, func() error { close(ran); return nil })
	}()
	select {
	case <-ran:
		t.Fatal("the second caller ran while the lock was held")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the second caller never got the lock after the holder released")
	}
}

// A lock nobody releases is a real failure with a real cause: the give-up
// names what is locked and how long the caller waited, so the operator can
// tell a slow peer from a wedged one.
func TestWithGivesUpOnAStuckHolder(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "store.lock")
	holdLock(t, lock)

	clock := newVirtualClock()
	err := With(lock, "known-host store", clock.onClock(testPolicy()), Raw, func() error {
		t.Error("fn ran while another process held the lock")
		return nil
	})
	if err == nil {
		t.Fatal("With succeeded against a lock nobody released")
	}
	for _, want := range []string{"known-host store", "locked by another toktop", testPolicy().Wait.String()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("give-up message %q does not name %q", err, want)
		}
	}
}

// A stale lock that will not unlink is a different failure from a live one,
// and reporting it as the latter names a peer that is not running. The
// message carries the path, so it goes through the caller's redactor: a
// caller that hides its home directory must not leak one into an error. A
// non-empty directory stands in for the unlink that keeps failing (a
// read-only config dir cannot be arranged portably), and it also proves the
// retry gives up on the deadline instead of spinning on the stale arm.
func TestWithReportsAnUnremovableStaleLock(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "store.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lock, "held"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stale := virtualOrigin.Add(-2 * time.Minute)
	if err := os.Chtimes(lock, stale, stale); err != nil {
		t.Fatal(err)
	}

	redact := func(s string) string { return strings.ReplaceAll(s, dir, "<home>") }
	clock := newVirtualClock()
	err := With(lock, "known-host store", clock.onClock(testPolicy()), redact, func() error {
		t.Error("fn ran while the lock could not be broken")
		return nil
	})
	if err == nil {
		t.Fatal("With succeeded against a stale lock that would not unlink")
	}
	// On the simulated timeline the spin the stale arm would have caused is a
	// poll count, not a wall-clock stall.
	if elapsed, polls := clock.elapsed(); elapsed > 10*time.Second || polls > int(testPolicy().Wait/testPolicy().Poll)+1 {
		t.Errorf("the give-up took %s over %d polls; the stale arm spins instead of checking the deadline", elapsed, polls)
	}
	for _, want := range []string{"known-host store", "could not be removed", "<home>"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("give-up message %q does not name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), dir) {
		t.Errorf("give-up message %q leaked the unredacted path", err)
	}
	// The redaction rewrites the path a *fs.PathError carries; it must not cost
	// the caller the reason the call failed.
	var pe *fs.PathError
	if !errors.As(err, &pe) {
		t.Fatalf("give-up error %v is not a *fs.PathError; the cause is unreachable", err)
	}
	if pe.Path != "<home>/store.lock" {
		t.Errorf("PathError.Path = %q, want the redacted lock path", pe.Path)
	}
	if pe.Err == nil {
		t.Error("PathError.Err is nil, so the reason the break failed was dropped")
	}
}

// A lock older than the stale age belongs to a process that died holding it.
// Waiting out the full deadline on it would report a holder that is not
// running, so it is broken and the caller proceeds.
func TestWithBreaksAStaleLock(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "store.lock")
	holdLock(t, lock)
	stale := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(lock, stale, stale); err != nil {
		t.Fatal(err)
	}

	ran := false
	if err := With(lock, "store", testPolicy(), Raw, func() error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("a stale lock was not broken: %v", err)
	}
	if !ran {
		t.Error("fn never ran after the stale lock was broken")
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("the lock outlived the broken critical section: %v", err)
	}
}

// A lock that cannot be created at all is not a lock failure: the directory
// is missing or unwritable, and the work itself reports that with a clearer
// error. Reporting a lock error here would name a peer that does not exist
// and leave the operator with nothing to act on, so fn runs either way.
func TestWithRunsWhenTheLockCannotBeCreated(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "absent", "store.lock")
	want := errors.New("the work reports its own cause")
	ran := false
	err := With(lock, "store", testPolicy(), Raw, func() error {
		ran = true
		return want
	})
	if !ran {
		t.Error("fn never ran, so the caller saw a lock error instead of the work's")
	}
	if !errors.Is(err, want) {
		t.Fatalf("With = %v, want the failure fn returned", err)
	}
}

// Raw is the policy a caller with nothing to hide passes: the lock path
// reaches its own messages unchanged.
func TestRawIsIdentity(t *testing.T) {
	const path = "/home/operator/.config/toktop/known_hosts"
	if got := Raw(path); got != path {
		t.Errorf("Raw(%q) = %q, want it unchanged", path, got)
	}
}
