// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

// Package agentwatch reports token throughput for AI coding agents running on
// this machine.
//
// toktop already accepts agent events pushed over HTTP by a harness that
// cooperates. This is the other half: agents that are simply running, with
// nobody pushing anything. It finds them, reads the token counts they already
// write to their own session transcripts, and feeds the same event stream, so
// a claude or codex working in a terminal shows up next to the engines.
//
// The reading is done by github.com/maci0/toktop/agentusage, the same code
// gauntlet uses for its dashboard. Its contract carries over: every
// number came from an agent that reported it, and an agent that reports
// nothing produces no rate rather than a zero.
//
// One concern per file: this one is the watcher's own state and lifetime,
// discover.go the pass that starts and stops trackers, engines.go the
// monitored engines those trackers are matched against, report.go the events
// read off a tracker.
package agentwatch

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/maci0/toktop/agentusage"

	"github.com/maci0/toktop/internal/core"
)

// Engines reports the endpoints toktop is already measuring, as the URLs the
// providers advertise. An agent generating through one of those engines has
// its tokens counted by the engine already. Events are still recorded so the
// agent stays on the dashboard, with ViaEngine set so header and chart
// totals skip them: the engine is the closer, more complete source (it sees
// every client, including ones that keep no transcript).
type Engines func() []string

// Defaults chosen so a monitor stays cheap: discovery is a /proc walk, and
// reading is a stat per transcript.
const (
	defaultDiscoverEvery = 3 * time.Second
	defaultReadEvery     = time.Second
)

// Watcher follows the agent processes on this machine.
type Watcher struct {
	rec           core.AgentRecorder
	engines       Engines
	discoverEvery time.Duration
	readEvery     time.Duration
	// listAgents lists running agent processes. Nil means agentusage.Discover.
	listAgents func() []agentusage.Process

	// clockMu guards the two fields SetNow and SetOnError write. Their reads
	// are not confined to Run's goroutine: every tracker runs its own reader
	// goroutine, and report stamps each event from the clock on that
	// goroutine, so an unguarded write races every tracker still following an
	// agent.
	clockMu sync.Mutex
	now     func() time.Time // always non-nil: New sets time.Now, SetNow normalizes nil
	// pace paces the discovery loop, under the same lock and for the same
	// reason as now: a run that stamps its events from an injected timeline
	// has to step its passes off that timeline too. Always non-nil: New sets
	// core.WallPacer, SetPacer normalizes nil.
	pace core.Pacer
	// onError surfaces a condition the operator must see that Run cannot
	// return. Nil disables reporting.
	onError func(error)

	mu      sync.Mutex
	tracked map[int]*tracked
	// running is a Run is live: see errRunInProgress. Guarded by mu, and
	// written only while it is held.
	running bool
	// engineErr is the last parse failure engineEndpoints reported, so a
	// permanent misconfiguration is surfaced once rather than every tick.
	engineErr string
}

// tracked is one agent process being followed.
type tracked struct {
	proc  agentusage.Process
	watch *agentusage.Watcher
	last  agentusage.Sample
	// viaEngine names the monitored engine this agent generates through, when
	// it has one. Token deltas are still recorded so the agent stays on the
	// dashboard; ViaEngine on the event is what stops aggregates adding them
	// on top of the engine's own numbers.
	viaEngine string
	// dirNote is core.ShortDir(proc.Dir), resolved once at discovery. Every
	// reported event carries it, and deriving it walks the path's symlinks,
	// which is one lstat per component on a path that cannot change while the
	// process lives.
	dirNote string
	cancel  context.CancelFunc
	done    chan struct{}
}

// New returns a watcher feeding rec. A nil engines function means nothing is
// being measured elsewhere.
func New(rec core.AgentRecorder, engines Engines) *Watcher {
	return &Watcher{
		rec: rec, engines: engines,
		discoverEvery: defaultDiscoverEvery, readEvery: defaultReadEvery,
		now:     time.Now,
		pace:    core.WallPacer,
		tracked: map[int]*tracked{},
	}
}

// SetNow overrides the clock used to stamp recorded events. Safe to call
// while Run is going: the write is taken under the same lock every tracker's
// goroutine reads it under.
// Demo mode passes the simulated clock so transcript-derived events stay on
// the seeded timeline. Attach "since" for database-backed agents stays wall
// time: session stores record real timestamps, not the simulated ones.
func (w *Watcher) SetNow(fn func() time.Time) {
	if fn == nil {
		fn = time.Now
	}
	w.clockMu.Lock()
	w.now = fn
	w.clockMu.Unlock()
}

// SetOnError installs the sink for conditions Run cannot return. Pass nil to
// disable reporting. Safe to call while Run is going, for the same reason
// SetNow is: the write is taken under the lock reportError reads it under.
func (w *Watcher) SetOnError(fn func(error)) {
	w.clockMu.Lock()
	w.onError = fn
	w.clockMu.Unlock()
}

// reportError hands err to the installed sink, if there is one. The sink is
// read under clockMu and called with it released: it is caller-supplied and
// may reach back into this watcher.
func (w *Watcher) reportError(err error) {
	w.clockMu.Lock()
	fn := w.onError
	w.clockMu.Unlock()
	if fn != nil {
		fn(err)
	}
}

// instant reads the injected clock, which the record path stamps from so a
// transcript event lands on the same timeline as the sample that carried it.
// The clock is read under clockMu and called with it released, so a
// caller-supplied clock that re-enters the watcher cannot deadlock against
// this accessor.
func (w *Watcher) instant() time.Time {
	w.clockMu.Lock()
	fn := w.now
	w.clockMu.Unlock()
	if fn == nil {
		return time.Now()
	}
	return fn()
}

// errRunInProgress is what a second concurrent Run is refused with. A watcher
// is built for one run: the discovery ticker and the trackers it starts belong
// to that one.
var errRunInProgress = errors.New("agentwatch: already running")

// Run follows agents until the context is canceled. Load the agent
// definitions (agentusage.LoadDefinitions, as main does) before Run so a
// malformed definitions file is reported where the operator can see it, not
// swallowed inside a goroutine behind the alt screen.
//
// A second Run while the first is live is refused with errRunInProgress and
// starts nothing. Two loops over one watcher walk the process table twice as
// often and run two passes of discover against the same tracker table, so every
// agent is scanned twice per pass and the one store that may be followed is
// claimed by whichever pass reached it first; both loops then stop every
// tracker when either one returns, and the second stopAll reports a final
// growth against a table the first has already cleared. This is the claim
// internal/collector's Run takes, for the same reason and over the same state.
//
// The claim is released when the loop returns, so a restart after a finished
// run is unaffected: what is refused is a second live loop, not a second run.
func (w *Watcher) Run(ctx context.Context) error {
	if !w.claimRun() {
		return errRunInProgress
	}
	defer w.releaseRun()
	if w.discoverEvery <= 0 {
		w.discoverEvery = defaultDiscoverEvery
	}
	if w.readEvery <= 0 {
		w.readEvery = defaultReadEvery
	}
	discover := w.pacer().New(w.discoverEvery)
	defer discover.Stop()

	w.discover(ctx)
	for {
		select {
		case <-ctx.Done():
			w.stopAll()
			return nil
		case <-discover.C():
			w.discover(ctx)
		}
	}
}

// claimRun takes the single live-run claim, reporting false when another Run
// already holds it and having changed nothing.
func (w *Watcher) claimRun() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running {
		return false
	}
	w.running = true
	return true
}

// releaseRun hands the claim back, so a run started after the previous one
// returned is not read as a second live loop.
func (w *Watcher) releaseRun() {
	w.mu.Lock()
	w.running = false
	w.mu.Unlock()
}

// SetPacer replaces what paces the discovery loop. Production leaves it on
// core.WallPacer. A simulated run passes a core.VirtualPacer and drives it,
// so the passes that discover agents are a function of the driver's steps
// rather than of wall-clock time: a watcher whose events are stamped on an
// injected clock but whose discovery wave fires on the wall clock admits an
// agent whenever the process happened to get there first, and the same seed
// then replays two different process tables. A nil pacer restores the wall
// clock.
//
// Safe to call at any time, but Run reads it once, when it builds the
// discovery ticker: a call during a live Run leaves that ticker on the old
// pacer and takes effect on the next Run.
func (w *Watcher) SetPacer(p core.Pacer) {
	if p == nil {
		p = core.WallPacer
	}
	w.clockMu.Lock()
	w.pace = p
	w.clockMu.Unlock()
}

// pacer reads the timing source under the same lock now is, so a run cannot
// stamp its events from one timeline and pace its passes from another.
func (w *Watcher) pacer() core.Pacer {
	w.clockMu.Lock()
	p := w.pace
	w.clockMu.Unlock()
	return p
}

func (w *Watcher) runningAgents() []agentusage.Process {
	if w.listAgents != nil {
		return w.listAgents()
	}
	return agentusage.Discover()
}

// sameProcess reports whether found is the OS process already being followed
// at that PID. Linux supplies a start time, which changes when the kernel
// reuses the PID; when the platform does not (Darwin), tool and working
// directory stand in. The directory is compared the way the file system
// compares it: on macOS and Windows two spellings of one checkout differ byte
// for byte, and treating them as two processes restarts the session's counters
// on every poll.
func sameProcess(was, found agentusage.Process) bool {
	if was.PID != found.PID {
		return false
	}
	if !was.Started.IsZero() && !found.Started.IsZero() {
		return was.Started.Equal(found.Started)
	}
	return was.Tool == found.Tool && agentusage.SameDir(was.Dir, found.Dir)
}

// storeKey names the transcript store a process writes to. A store is
// identified by the tool and the working directory, never by the PID:
// agentusage.Watch resolves its source from that pair, so two processes in one
// repo enumerate the same sessions. The directory is folded the way DirKey
// folds it, or two spellings of one checkout would claim the same store twice
// and two watchers would tail it.
func storeKey(p agentusage.Process) string { return p.Tool + "\x00" + agentusage.DirKey(p.Dir) }

// closedDone is the done channel of a tracker with no watcher of its own. It
// is already closed, so stopOne's wait returns at once instead of blocking on
// a goroutine that was never started.
func closedDone() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

func (w *Watcher) stopAll() {
	w.mu.Lock()
	gone := w.trackedList()
	clear(w.tracked)
	w.mu.Unlock()
	for _, t := range gone {
		w.stopOne(t)
	}
}

// stopWait bounds how long a stop waits for a tracker's read loop to unwind.
// Cancel stops the ticker, but the poll already in flight is inside the kernel
// walking a transcript store or reading a transcript, and neither takes the
// context. A mount that stopped answering would otherwise hang shutdown, and
// with it process exit, for as long as the read blocks. Past the bound the
// tail read is dropped: the transcripts of an exited agent are one more poll
// from the dashboard anyway.
const stopWait = 3 * time.Second

func (w *Watcher) stopOne(t *tracked) {
	// Both fields are set before the entry is in the map and reassigned only
	// alongside each other, so neither is nil here.
	t.cancel()
	timer := time.NewTimer(stopWait)
	defer timer.Stop()
	select {
	case <-t.done:
	case <-timer.C:
		return
	}
	if t.watch != nil {
		w.report(t, t.watch.Poll())
	}
}

// trackedList is the followed agents in PID order. Report and shutdown
// sequences must not depend on map iteration, or equal-timestamp events
// land in a different order across replays. Caller holds w.mu.
func (w *Watcher) trackedList() []*tracked {
	return sortTracked(slices.Collect(maps.Values(w.tracked)))
}

// sortTracked orders trackers by PID in place.
func sortTracked(ts []*tracked) []*tracked {
	slices.SortFunc(ts, func(a, b *tracked) int {
		return cmp.Compare(a.proc.PID, b.proc.PID)
	})
	return ts
}
