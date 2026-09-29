// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentwatch

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"time"

	"github.com/maci0/toktop/agentusage"

	"github.com/maci0/toktop/internal/core"
)

// discover starts following new agent processes and forgets exited ones.
func (w *Watcher) discover(ctx context.Context) {
	found := w.runningAgents()
	newProcs, replaced, exited := w.classify(found)

	// Stop the reused-PID tracker before attaching its replacement: two
	// watchers tailing the same transcripts would double-count growth.
	for _, t := range replaced {
		w.stopOne(t)
	}

	// Exited trackers are stopped here, before any watcher is installed, not
	// at the end of the pass. The handover below promotes a follower onto the
	// store a dead tracker was still tailing, and until that tracker is
	// stopped its poll loop keeps running: two watchers on one store, each
	// reporting growth under its own PID, so the collector's id window cannot
	// merge them and every token written in the overlap counts twice. stopOne
	// also reports the dead watcher's final growth, so the stopped tracker's
	// share lands before the follower's baseline is taken and the two
	// partitions do not overlap.
	for _, t := range exited {
		w.stopOne(t)
	}

	// A store is followed once. Two watchers tailing the same transcripts each
	// report the same growth under their own PID, and the two records carry
	// different sample ids, so the collector's id window cannot merge them and
	// every token written by either process is counted twice. The first live
	// process for a store follows it; the rest are tracked (they belong on the
	// dashboard) with no watcher of their own, and pick the store up below if
	// the follower exits.
	var started []*tracked
	var startCtx []context.Context
	claimed := w.claimedStores()

	// attach gives p its own watcher, or hands the one it built to the
	// tracker already in the table when that tracker has none (the handover
	// below). follow inserts a tracker with none.
	//
	// A watcher counts only what is written after it attaches, so an agent
	// already halfway through a task contributes from here on rather than
	// retroactively. That keeps the rate honest at the cost of the first part
	// of a session toktop was not running for. since is wall time even when
	// SetNow injects a simulated clock: session stores record real timestamps,
	// and comparing them to a demo origin would count the whole history as this
	// run. Watch walks transcript stores; doing that under w.mu would stall
	// report() for agents already being followed. cancel is set before the map
	// insert so stopOne never observes a nil cancel.
	attach := func(p agentusage.Process) {
		watch := p.Watch(time.Now())
		if watch == nil {
			return // this agent keeps nothing readable
		}
		// The sample stamp is not a filesystem comparison, so it follows the
		// injected clock: report derives the event id from it, and an id
		// carrying a wall-clock instant makes the same reading look like a new
		// event on every replay.
		watch.SetNow(w.instant)
		tctx, cancel := context.WithCancel(ctx)
		t := &tracked{proc: p, dirNote: core.ShortDir(p.Dir), watch: watch, done: make(chan struct{}), cancel: cancel}
		w.mu.Lock()
		prev, seen := w.tracked[p.PID]
		switch {
		case !seen:
			w.tracked[p.PID] = t
		case prev.watch == nil:
			// Handover. A follower (watch == nil) holds the PID with no
			// watcher of its own; taking the store over replaces it, so the
			// handover below can make progress. The tracker in the table is
			// the one that must start reading: dropping the watch here (as a
			// plain "already tracked" bail would) leaves a live agent with no
			// reader and every token it writes unreported. The fields are
			// written under the same lock stopOne and report read them under,
			// so the goroutine started below is the only reader of a tracker
			// that already had none.
			prev.proc, prev.dirNote = t.proc, t.dirNote
			prev.watch, prev.done, prev.cancel = t.watch, t.done, t.cancel
			t = prev
		default:
			// A watched tracker keeps its PID: two watchers tailing the same
			// transcripts would double-count growth.
			w.mu.Unlock()
			cancel()
			return
		}
		w.mu.Unlock()
		started = append(started, t)
		startCtx = append(startCtx, tctx)
	}
	follow := func(p agentusage.Process) {
		t := &tracked{proc: p, dirNote: core.ShortDir(p.Dir), done: closedDone(), cancel: func() {}}
		w.mu.Lock()
		if _, seen := w.tracked[p.PID]; seen {
			w.mu.Unlock()
			return
		}
		w.tracked[p.PID] = t
		w.mu.Unlock()
	}

	// promoteWatch is attach for a tracker that already exists: the handover
	// installs a watcher on a follower rather than adding a second tracker for
	// a PID that is already tracked. It leaves the follower as it found it if
	// the agent keeps nothing readable, or if the tracker went away or gained
	// a watcher in between (shutdown, or a reattached store).
	promoteWatch := func(p agentusage.Process, t *tracked) {
		watch := p.Watch(time.Now())
		if watch == nil {
			return // this agent keeps nothing readable
		}
		watch.SetNow(w.instant)
		tctx, cancel := context.WithCancel(ctx)
		w.mu.Lock()
		cur, ok := w.tracked[t.proc.PID]
		if !ok || cur != t || cur.watch != nil {
			w.mu.Unlock()
			cancel()
			return
		}
		cur.watch = watch
		cur.cancel = cancel
		cur.done = make(chan struct{})
		w.mu.Unlock()
		started = append(started, cur)
		startCtx = append(startCtx, tctx)
	}

	// newProcs is PID-ordered, so the lowest PID for a store takes it and the
	// choice does not depend on map iteration.
	for _, p := range newProcs {
		if claimed[storeKey(p)] {
			follow(p)
			continue
		}
		before := len(started)
		attach(p)
		if len(started) > before {
			claimed[storeKey(p)] = true
		}
	}

	// Handover: a store whose follower exited is picked up by a process still
	// running against it, so a live agent is never left unwatched. Ordered by
	// PID like every other pass, so the choice is reproducible.
	w.mu.Lock()
	type promotion struct {
		proc agentusage.Process
		tr   *tracked
	}
	var promote []promotion
	for _, t := range w.trackedList() {
		if t.watch == nil && !claimed[storeKey(t.proc)] {
			claimed[storeKey(t.proc)] = true
			promote = append(promote, promotion{proc: t.proc, tr: t})
		}
	}
	w.mu.Unlock()
	// A follower already holds a tracker, so the watcher is installed into it
	// rather than attached as a new one: attach would find the PID taken and
	// drop the watcher, leaving the store followed by nobody.
	for _, pm := range promote {
		promoteWatch(pm.proc, pm.tr)
	}

	for i, t := range started {
		go func(t *tracked, tctx context.Context) {
			defer close(t.done)
			t.watch.Run(tctx, w.readEvery, func(s agentusage.Sample) {
				w.report(t, s)
			})
		}(t, startCtx[i])
	}

	w.matchEngines()
}

// classify sorts the processes found on this pass against the ones already
// tracked: those to start following, the trackers a reused PID has taken over
// from, and the trackers whose process is gone. Each set is returned in a
// fixed order, so the sequence of stops, and the events they emit, is the
// same on every pass.
func (w *Watcher) classify(found []agentusage.Process) (newProcs []agentusage.Process, replaced, exited []*tracked) {
	live := make(map[int]bool, len(found))
	w.mu.Lock()
	for _, p := range found {
		live[p.PID] = true
		t, seen := w.tracked[p.PID]
		switch {
		case !seen:
			newProcs = append(newProcs, p)
		case sameProcess(t.proc, p):
			// still the same OS process
		default:
			// PID reused by a different process. Drop the stale tracker
			// now so the insert below does not treat it as still live.
			delete(w.tracked, p.PID)
			replaced = append(replaced, t)
			newProcs = append(newProcs, p)
		}
	}
	for pid, t := range w.tracked {
		if !live[pid] {
			delete(w.tracked, pid)
			exited = append(exited, t)
		}
	}
	w.mu.Unlock()
	// The replaced and exited sets are collected from the tracked map, so their
	// order is the map's.
	sortTracked(replaced)
	sortTracked(exited)
	slices.SortFunc(newProcs, func(a, b agentusage.Process) int {
		return cmp.Compare(a.PID, b.PID)
	})
	return newProcs, replaced, exited
}

// claimedStores names the transcript stores an existing watcher already
// follows, so a second process on the same store is tracked without one.
func (w *Watcher) claimedStores() map[string]bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	claimed := make(map[string]bool)
	for _, t := range w.tracked {
		if t.watch != nil {
			claimed[storeKey(t.proc)] = true
		}
	}
	return claimed
}

// matchEngines records which engine each tracked agent is generating through.
// Re-read every pass rather than once at discovery: an agent connects to its
// engine after it starts, and may switch engines mid-session. One table read
// covers every tracked pid; matching each agent against each engine
// separately would reread /proc/net/tcp per pair.
func (w *Watcher) matchEngines() {
	w.mu.Lock()
	pids := slices.Sorted(maps.Keys(w.tracked))
	w.mu.Unlock()

	endpoints, labels, err := w.engineEndpoints()
	w.engineError(err)
	matched := agentusage.MatchingEndpoints(pids, endpoints)
	w.mu.Lock()
	for pid, t := range w.tracked {
		t.viaEngine = labels[matched[pid]]
	}
	w.mu.Unlock()
}
