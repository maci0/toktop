// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentwatch

import (
	"context"
	"errors"
	"sync"

	"github.com/maci0/toktop/agentusage"
	"testing"
	"time"
)

// A second Run while the first is live is refused and starts nothing: one
// discovery loop owns the tracker table, and a second would walk the process
// table twice as often, race the store claim between two passes, and stop every
// tracker when either loop returned. The mirror of the same claim in
// internal/collector, asserted the same way.
func TestSecondRunRefusedWhileFirstIsLive(t *testing.T) {
	w := New(&recorder{}, nil)
	w.listAgents = func() []agentusage.Process { return nil }
	w.discoverEvery = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { first <- w.Run(ctx) }()

	// The first Run has claimed by the time any scheduling lets the second
	// reach Run, so poll rather than assume: a refusal asserted before the
	// claim was taken would pass for the wrong reason.
	waitFor(t, time.Second, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.running
	})
	if err := w.Run(ctx); !errors.Is(err, errRunInProgress) {
		t.Fatalf("second Run = %v, want errRunInProgress", err)
	}

	// The refused run left no second discovery loop behind: cancelling the
	// context ends the first, and Run returns rather than blocking on a
	// tracker the loop never started.
	cancel()
	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("first Run = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first Run did not return after its context was canceled")
	}

	// The claim is released with the run, so a run started after a finished one
	// is a fresh join rather than a second live loop.
	after, cancelAfter := context.WithCancel(context.Background())
	cancelAfter()
	if err := w.Run(after); err != nil {
		t.Fatalf("Run after the first returned = %v, want nil", err)
	}
}

// Two concurrent Runs race for the claim, and exactly one wins: the loser must
// be told it lost rather than admitted, or the guard holds only against a
// caller that happens to be slow.
func TestConcurrentRunsExactlyOneHoldsTheClaim(t *testing.T) {
	w := New(&recorder{}, nil)
	w.listAgents = func() []agentusage.Process { return nil }
	w.discoverEvery = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = w.Run(ctx)
		}()
	}
	close(start)
	waitFor(t, time.Second, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.running
	})
	cancel()
	wg.Wait()

	refused := 0
	for _, err := range errs {
		switch {
		case err == nil:
		case errors.Is(err, errRunInProgress):
			refused++
		default:
			t.Fatalf("Run = %v, want nil or errRunInProgress", err)
		}
	}
	if refused != 1 {
		t.Fatalf("%d of 2 runs refused, want 1", refused)
	}
}
