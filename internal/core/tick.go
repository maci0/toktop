// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"context"
	"slices"
	"sync"
	"time"
)

// Ticker is the timing source one background loop waits on. C is the channel
// the loop selects on; Stop releases whatever paces it.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// Pacer produces the Ticker a background loop runs on. WallPacer is
// production time. A simulation supplies its own, so a loop's schedule is a
// function of the timeline the driver stepped rather than of wall-clock time
// and the OS scheduler: with the clock injection SetNow takes, a pacer and an
// instant are the two halves that make a whole run replayable.
type Pacer interface {
	New(d time.Duration) Ticker
}

// WallPacer paces loops on wall-clock time.
var WallPacer Pacer = wallPacer{}

type wallPacer struct{}

// New wraps time.NewTicker, whose one-slot channel is what makes a pass that
// runs long fire the next tick straight away instead of shifting the whole
// schedule by however long it took.
func (wallPacer) New(d time.Duration) Ticker { return wallTicker{t: time.NewTicker(d)} }

type wallTicker struct{ t *time.Ticker }

func (w wallTicker) C() <-chan time.Time { return w.t.C }
func (w wallTicker) Stop()               { w.t.Stop() }

// VirtualPacer paces loops on a simulated timeline. The tickers it hands out
// fire when the driver fires them and never on their own, so a run's schedule
// is the step sequence and nothing else. Use it with a clock injected through
// the SetNow each package already takes: a loop whose frames are stamped by
// the driver and whose passes are fired by the driver replays exactly.
type VirtualPacer struct {
	mu      sync.Mutex
	tickers []*virtualTicker
}

// NewVirtualPacer returns a pacer whose tickers stay silent until the driver
// fires them.
func NewVirtualPacer() *VirtualPacer { return &VirtualPacer{} }

// New registers a ticker with the pacer, so a later Fire reaches it. The
// interval is the driver's to apply: Fire decides when a tick lands, so a
// ticker that kept it would only be a second source of schedule.
func (p *VirtualPacer) New(time.Duration) Ticker {
	t := &virtualTicker{p: p, c: make(chan time.Time, 1)}
	p.mu.Lock()
	p.tickers = append(p.tickers, t)
	p.mu.Unlock()
	return t
}

// Fire hands one tick, stamped at, to every live ticker. A tick whose channel
// already holds one is dropped: a wall-clock ticker coalesces the same way
// when a pass runs long, and the pending tick stands for both.
func (p *VirtualPacer) Fire(at time.Time) {
	p.mu.Lock()
	live := slices.Clone(p.tickers)
	p.mu.Unlock()
	for _, t := range live {
		t.fire(at)
	}
}

type virtualTicker struct {
	c       chan time.Time
	mu      sync.Mutex
	p       *VirtualPacer
	stopped bool
}

func (t *virtualTicker) C() <-chan time.Time { return t.c }

// Stop deregisters the ticker. Its channel is left open on purpose: a closed
// one would make the loop's select return at once and spin.
func (t *virtualTicker) Stop() {
	t.mu.Lock()
	t.stopped = true
	t.mu.Unlock()
	t.p.drop(t)
}

func (t *virtualTicker) fire(at time.Time) {
	t.mu.Lock()
	stopped := t.stopped
	t.mu.Unlock()
	if stopped {
		return
	}
	select {
	case t.c <- at:
	default:
	}
}

// drop unregisters a stopped ticker.
func (p *VirtualPacer) drop(t *virtualTicker) {
	p.mu.Lock()
	p.tickers = slices.DeleteFunc(p.tickers, func(x *virtualTicker) bool { return x == t })
	p.mu.Unlock()
}

// Tick runs one background loop until ctx is done, calling warm once up front
// and refresh on every tick. It returns a channel closed once the loop has
// returned, so a caller that must not outlive it can join.
//
// The first call is not deferred to the first tick. A manual probe should put
// a request on the wire when the dashboard comes up, not one interval later,
// and the host-vitals poller warms its cache before the first frame reads it.
// Where the two passes are the same function it is passed twice: the process
// table has no cache to warm, and a second helper for that case would be the
// same loop written again.
//
// The loop is paced by wall-clock time. TickWith is this loop on a caller-
// supplied pacer, for a run whose passes a driver steps.
func Tick(ctx context.Context, every time.Duration, warm, refresh func()) <-chan struct{} {
	return TickWith(ctx, WallPacer, every, warm, refresh)
}

// TickWith is Tick on a supplied pacer. The pass order, the warm call and the
// join channel are Tick's; only the timing source differs, so the same loop
// body runs in production and under a driver.
func TickWith(ctx context.Context, p Pacer, every time.Duration, warm, refresh func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := p.New(every)
		defer t.Stop()
		warm()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C():
				if ctx.Err() != nil {
					return
				}
				refresh()
			}
		}
	}()
	return done
}
