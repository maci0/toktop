// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package lockfile

import (
	"os"
	"path/filepath"
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
