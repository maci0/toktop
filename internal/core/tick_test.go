// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestTick(t *testing.T) {
	// Every receive below is bounded, so a ticker that stops firing fails the
	// test instead of wedging it until the go test panic.
	awaitFire := func(t *testing.T, fired <-chan time.Time, within time.Duration) time.Time {
		t.Helper()
		select {
		case at := <-fired:
			return at
		case <-time.After(within):
			t.Fatalf("prober did not fire within %s", within)
			return time.Time{}
		}
	}

	t.Run("fires without waiting for the first tick", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fired := make(chan time.Time, 1)
		prober := func() { fired <- time.Now() }
		// The interval is far longer than the wait below, so a first call
		// that arrives is the immediate one and not a tick: with a 10ms
		// interval the two are indistinguishable and deferring the first
		// probe to the tick would still pass.
		Tick(ctx, time.Hour, prober, prober)
		awaitFire(t, fired, 2*time.Second)
	})

	t.Run("fires on every tick", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fired := make(chan time.Time, 8)
		prober := func() { fired <- time.Now() }
		Tick(ctx, 10*time.Millisecond, prober, prober)

		first := awaitFire(t, fired, 2*time.Second)
		for i := 0; i < 2; i++ {
			if second := awaitFire(t, fired, 2*time.Second); second == first {
				t.Fatal("ticker fired twice at the same instant")
			}
		}
	})

	t.Run("stops on cancel", func(t *testing.T) {
		// Inside a synctest bubble the ticker, the callback and the waits
		// share one clock, and the clock only moves once every goroutine is
		// durably blocked. A tick that was already in flight when cancel
		// landed therefore completes before the wait below returns, so a
		// correct ticker cannot be caught mid-call and counted as one that
		// ran again. Sampled against the wall clock the same test fails
		// whenever a probe overruns its interval.
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var mu sync.Mutex
			count := 0
			prober := func() {
				mu.Lock()
				defer mu.Unlock()
				count++
			}
			done := Tick(ctx, 5*time.Millisecond, prober, prober)
			<-time.After(32 * time.Millisecond) // several ticks
			cancel()
			<-done // wait for the ticker loop to exit
			mu.Lock()
			final := count
			mu.Unlock()
			if final == 0 {
				t.Fatal("the ticker never fired, so stopping it proves nothing")
			}
			<-time.After(20 * time.Millisecond) // past several ticks
			mu.Lock()
			defer mu.Unlock()
			if count != final {
				t.Fatalf("prober ran %d more times after cancel", count-final)
			}
		})
	})
}
