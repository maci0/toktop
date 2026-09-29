// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"context"
	"sync"
	"sync/atomic"
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

// The pacer a replayed run runs on is published API, and every claim below is
// one its docstring makes and a driver relies on: the tickers stay silent
// until Fire, a tick is dropped rather than queued behind the one already
// held, and Stop takes the ticker out of the schedule without closing the
// channel the loop selects on. Nothing here sleeps: a Fire is synchronous, so
// a receive after one either has the tick or never will.
func TestVirtualPacer(t *testing.T) {
	// A tick lands only when the driver says so, so a receive on a channel
	// the driver has not fired has to be proved absent without blocking.
	silent := func(t *testing.T, c <-chan time.Time) {
		t.Helper()
		select {
		case at := <-c:
			t.Fatalf("ticker fired unasked at %s", at)
		default:
		}
	}

	t.Run("tickers stay silent until fired", func(t *testing.T) {
		p := NewVirtualPacer()
		tk := p.New(time.Hour)
		defer tk.Stop()
		silent(t, tk.C())

		at := time.Unix(1_700_000_000, 0)
		p.Fire(at)
		select {
		case got := <-tk.C():
			if !got.Equal(at) {
				t.Errorf("tick stamped %s, want the instant the driver fired (%s)", got, at)
			}
		default:
			t.Fatal("Fire left the ticker silent, so a driver's step never reaches the loop")
		}
	})

	// A pass that runs long must not shift the schedule by however long it
	// took, which is what a one-slot channel buys. The tick already held
	// stands for the ones dropped behind it, so a second receive has to find
	// the channel empty rather than a queue of backed-up ticks.
	t.Run("a held tick coalesces the ones behind it", func(t *testing.T) {
		p := NewVirtualPacer()
		tk := p.New(time.Second)
		defer tk.Stop()

		p.Fire(time.Unix(1_700_000_000, 0))
		p.Fire(time.Unix(1_700_000_005, 0))
		p.Fire(time.Unix(1_700_000_010, 0))

		first := <-tk.C()
		if want := time.Unix(1_700_000_000, 0); !first.Equal(want) {
			t.Errorf("first tick = %s, want %s", first, want)
		}
		silent(t, tk.C())
	})

	// Every ticker a run created is a loop selecting on it, so a Fire that
	// reached only one of them would pace one pass and stall the rest.
	t.Run("one fire reaches every live ticker", func(t *testing.T) {
		p := NewVirtualPacer()
		a, b := p.New(time.Second), p.New(time.Second)
		defer a.Stop()
		defer b.Stop()

		p.Fire(time.Unix(1_700_000_000, 0))
		for i, tk := range []Ticker{a, b} {
			select {
			case <-tk.C():
			default:
				t.Errorf("ticker %d was not fired", i)
			}
		}
	})

	// A stopped ticker must leave the schedule or a loop that already ended
	// would still be handed ticks for the rest of the run.
	t.Run("a stopped ticker is dropped from the schedule", func(t *testing.T) {
		p := NewVirtualPacer()
		live, stopped := p.New(time.Second), p.New(time.Second)
		defer live.Stop()
		stopped.Stop()

		p.Fire(time.Unix(1_700_000_000, 0))
		silent(t, stopped.C())
		select {
		case <-live.C():
		default:
			t.Error("the live ticker was not fired, so the negative above proved nothing")
		}
	})

	// The channel is left open on purpose: a closed one returns from the
	// loop's select at once, so the loop would spin until its context is
	// done rather than blocking on the next tick.
	t.Run("stop leaves the channel open", func(t *testing.T) {
		p := NewVirtualPacer()
		tk := p.New(time.Second)
		tk.Stop()
		silent(t, tk.C())
	})

	// A ticker registered after the step does not reach back for the tick
	// that has already passed: Fire decides when a tick lands, and a ticker
	// that replayed history would be a second source of schedule.
	t.Run("a ticker registered later misses an earlier fire", func(t *testing.T) {
		p := NewVirtualPacer()
		p.Fire(time.Unix(1_700_000_000, 0))
		tk := p.New(time.Second)
		defer tk.Stop()
		silent(t, tk.C())
	})
}

// TickWith is the loop a replayed run runs, and the two halves of a replay are
// the driver's clock and the driver's pacer together. A loop stepped only by
// its pacer proves TickWith neither defers the warm pass to the first tick nor
// reads wall-clock time of its own: every pass below is accounted for by an
// explicit Fire.
func TestTickWithRunsOnThePacerAlone(t *testing.T) {
	p := NewVirtualPacer()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The loop owns both counters, so they are read across goroutines here
	// even though the driver is the only thing deciding when they move.
	var warm, refresh atomic.Int32
	done := TickWith(ctx, p, time.Hour,
		func() { warm.Add(1) },
		func() { refresh.Add(1) })

	// The warm pass runs on the goroutine, so a read before the first Fire
	// has to wait for it rather than assume a schedule.
	waitForCount(t, &warm, 1, "the warm pass never ran")
	if got := refresh.Load(); got != 0 {
		t.Errorf("refresh ran %d times before the driver fired a tick", got)
	}

	for i := int32(1); i <= 3; i++ {
		p.Fire(time.Unix(1_700_000_000+int64(i), 0))
		waitForCount(t, &refresh, i, "the driver fired a tick the loop did not pass on")
	}

	cancel()
	<-done // the loop has returned, so a later tick cannot race the read below
	settled := refresh.Load()
	p.Fire(time.Unix(1_700_000_100, 0))
	if got := refresh.Load(); got != settled {
		t.Errorf("refresh ran %d more times after the loop returned", got-settled)
	}
}

// waitForCount polls a counter the loop goroutine writes. A tick is handed to
// the channel synchronously, so the pass receiving it has not run by the time
// Fire returns, and the read cannot be a bare comparison.
func waitForCount(t *testing.T, n *atomic.Int32, want int32, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n.Load() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s: got %d, want %d", msg, n.Load(), want)
}
