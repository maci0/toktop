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
package agentwatch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
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
	// onError surfaces a condition the operator must see that Run cannot
	// return. Nil disables reporting.
	onError func(error)

	mu      sync.Mutex
	tracked map[int]*tracked
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
// SetNow is: the write is taken under the lock engineError reads it under.
func (w *Watcher) SetOnError(fn func(error)) {
	w.clockMu.Lock()
	w.onError = fn
	w.clockMu.Unlock()
}

// engineError reports err, and repeats it only when it differs from the last
// one reported. A misconfigured engine address fails on every discovery tick,
// so an undeduplicated report would be a permanent error banner over a
// condition the operator has already seen.
//
// A clean tick clears the latch, so a condition that recovers and then comes
// back is reported again. Without that, an address fixed at runtime (a
// gateway that finished starting, a forward that reconnected) and broken
// again an hour later would be silenced by the first report: the operator
// fixed it, saw the banner go, and is never told it is back.
func (w *Watcher) engineError(err error) {
	w.mu.Lock()
	if err == nil {
		w.engineErr = ""
		w.mu.Unlock()
		return
	}
	repeat := w.engineErr == err.Error()
	w.engineErr = err.Error()
	w.mu.Unlock()
	if repeat {
		return
	}
	w.reportError(err)
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

// Run follows agents until the context is canceled. Load the agent
// definitions (agentusage.LoadDefinitions, as main does) before Run so a
// malformed definitions file is reported where the operator can see it, not
// swallowed inside a goroutine behind the alt screen.
func (w *Watcher) Run(ctx context.Context) {
	if w.discoverEvery <= 0 {
		w.discoverEvery = defaultDiscoverEvery
	}
	if w.readEvery <= 0 {
		w.readEvery = defaultReadEvery
	}
	discover := time.NewTicker(w.discoverEvery)
	defer discover.Stop()

	w.discover(ctx)
	for {
		select {
		case <-ctx.Done():
			w.stopAll()
			return
		case <-discover.C:
			w.discover(ctx)
		}
	}
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
// directory stand in.
func sameProcess(was, found agentusage.Process) bool {
	if was.PID != found.PID {
		return false
	}
	if !was.Started.IsZero() && !found.Started.IsZero() {
		return was.Started.Equal(found.Started)
	}
	return was.Tool == found.Tool && was.Dir == found.Dir
}

// storeKey names the transcript store a process writes to. A store is
// identified by the tool and the working directory, never by the PID:
// agentusage.Watch resolves its source from that pair, so two processes in one
// repo enumerate the same sessions.
func storeKey(p agentusage.Process) string { return p.Tool + "\x00" + p.Dir }

// closedDone is the done channel of a tracker with no watcher of its own. It
// is already closed, so stopOne's wait returns at once instead of blocking on
// a goroutine that was never started.
func closedDone() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

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
		t.viaEngine = ""
		if ap, ok := matched[pid]; ok {
			for i, e := range endpoints {
				if e == ap {
					t.viaEngine = labels[i]
					break
				}
			}
		}
	}
	w.mu.Unlock()
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
	if t.cancel != nil {
		t.cancel()
	}
	if t.done != nil {
		timer := time.NewTimer(stopWait)
		defer timer.Stop()
		select {
		case <-t.done:
		case <-timer.C:
			return
		}
	}
	if t.watch != nil {
		w.report(t, t.watch.Poll())
	}
}

// engineEndpoints parses the monitored engines' advertised URLs into addresses
// that can be compared against a process's open connections. A malformed URL
// is returned as an error: dropping it silently would leave the agent's
// tokens counted both by the engine and by its transcript, a double count
// with no symptom the operator could trace back to a bad address.
func (w *Watcher) engineEndpoints() ([]netip.AddrPort, []string, error) {
	if w.engines == nil {
		return nil, nil, nil
	}
	raw := w.engines()
	eps := make([]netip.AddrPort, 0, len(raw))
	labels := make([]string, 0, len(raw))
	var bad []string
	for _, addr := range raw {
		ap, label, err := parseEngineAddr(addr)
		if err != nil {
			bad = append(bad, err.Error())
			continue
		}
		if ap == (netip.AddrPort{}) {
			continue
		}
		eps = append(eps, ap)
		labels = append(labels, label)
	}
	if len(bad) > 0 {
		return eps, labels, errors.New(strings.Join(bad, "; "))
	}
	return eps, labels, nil
}

// parseEngineAddr turns "http://127.0.0.1:11434" into an endpoint and a label.
// A bare "127.0.0.1:8080" is accepted as well. A hostname that is not an
// address is skipped, reported as the zero AddrPort and a nil error: it
// cannot be compared against a connection table, and resolving it would make
// a monitor do DNS on a timer. URLs that omit the port (http://127.0.0.1,
// https://…) use the scheme default: without it ParseAddrPort fails and an
// agent talking to that engine would not be labelled via, so its tokens would
// be counted twice. A string carrying a scheme that will not parse is an
// error, not a hostname: the operator wrote something malformed and needs to
// be told which. So is a port of 0, which parses but names no endpoint.
func parseEngineAddr(addr string) (netip.AddrPort, string, error) {
	host := addr
	// Only a scheme-bearing string is a URL. url.Parse rejects a bare
	// "127.0.0.1:8080" (a colon in the first path segment), and that spelling
	// is a documented input, so the parse is gated on the marker rather than
	// on whether it succeeds.
	if strings.Contains(addr, "://") {
		u, err := url.Parse(addr)
		if err != nil {
			// The library's message quotes the address back, so it carries
			// engine-supplied text to the dashboard banner. The snippet is
			// what keeps that text renderable and bounded.
			return netip.AddrPort{}, "", fmt.Errorf("engine address is not a URL: %s", core.Snippet([]byte(err.Error())))
		}
		if u.Host != "" {
			host = u.Host
			if u.Port() == "" {
				port := "80"
				if u.Scheme == "https" {
					port = "443"
				}
				host = net.JoinHostPort(u.Hostname(), port)
			}
		}
	}
	ap, err := netip.ParseAddrPort(host)
	if err != nil {
		return netip.AddrPort{}, "", nil
	}
	if ap.Port() == 0 {
		// Port 0 parses but is not an endpoint: no connection table holds it,
		// so the entry would sit in the sweep forever and match nothing,
		// labelling no agent and hiding the reason. An explicit :0 is a
		// misspelled address, which is the operator's to fix.
		return netip.AddrPort{}, "", fmt.Errorf("engine address %q names port 0, which is not a connectable endpoint", core.Snippet([]byte(addr)))
	}
	return ap, host, nil
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

func (w *Watcher) report(t *tracked, cur agentusage.Sample) {
	// A sample that reads empty is still a reading. A transcript rewritten
	// under the watcher republishes its counters at zero, and treating that
	// as "nothing to say" leaves the old baseline in place, so the growth
	// that follows is measured from figures the transcripts no longer hold
	// and is never reported. Sample.Delta already handles it: no growth is
	// reported and the empty sample becomes the baseline.
	w.mu.Lock()
	d, ok := cur.Delta(t.last)
	if !ok {
		// Nothing new, or a transcript rewritten under us replaced the
		// records already reported. Either way the next growth is measured
		// from this sample, not from figures the transcripts no longer hold.
		t.last = cur
		w.mu.Unlock()
		return // silence is not an event
	}
	t.last = cur
	proc := t.proc
	dir := t.dirNote
	via := t.viaEngine
	rec := w.rec
	w.mu.Unlock()
	if rec == nil {
		return
	}
	rec.RecordAgent(core.AgentEvent{
		At:             w.instant(),
		ID:             sampleID(proc, cur.At),
		Agent:          core.AgentNameField(proc.Tool),
		Kind:           core.AgentKindTurn,
		PromptTokens:   core.ClampEventTokens(int64(d.Input)),
		OutputTokens:   core.ClampEventTokens(int64(d.Output)),
		ThinkingTokens: core.ClampEventTokens(int64(d.Thinking)),
		Span:           d.Span,
		ViaEngine:      core.ClampField(core.SingleLine(via), core.AgentViaMax),
		Note:           core.ClampField(core.RedactHome(core.SingleLine(note(dir, d.Thinking, via))), core.AgentNoteMax),
	})
}

// sampleID is stable for one process at one sample instant, so a retried
// report of the same reading (final Poll after Run's last callback, a
// tracker that forgot its baseline) is ignored by the collector's id window.
func sampleID(proc agentusage.Process, at time.Time) string {
	return "aw:" + strconv.Itoa(proc.PID) + ":" +
		strconv.FormatInt(proc.Started.UnixNano(), 10) + ":" +
		strconv.FormatInt(at.UnixNano(), 10)
}

// note carries what the event cannot: where the agent is working (already
// shortened by the tracker, which resolved it once), how much of the output
// was reasoning when the agent says so, and which monitored engine already
// counts this output when one does.
func note(dir string, thinking int, via string) string {
	s := dir
	if thinking > 0 {
		s += " · " + strconv.Itoa(thinking) + " reasoning"
	}
	if via != "" {
		s += " · counted by engine " + via
	}
	return s
}
