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
	now        func() time.Time // always non-nil: New sets time.Now, SetNow normalizes nil
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

// SetNow overrides the clock used to stamp recorded events. Call before Run.
// Demo mode passes the simulated clock so transcript-derived events stay on
// the seeded timeline. Attach "since" for database-backed agents stays wall
// time: session stores record real timestamps, not the simulated ones.
func (w *Watcher) SetNow(fn func() time.Time) {
	if fn == nil {
		fn = time.Now
	}
	w.now = fn
}

// SetOnError installs the sink for conditions Run cannot return. Pass nil to
// disable reporting. Call before Run.
func (w *Watcher) SetOnError(fn func(error)) { w.onError = fn }

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
	if repeat || w.onError == nil {
		return
	}
	w.onError(err)
}

// instant reads the injected clock, which the record path stamps from so a
// transcript event lands on the same timeline as the sample that carried it.
func (w *Watcher) instant() time.Time { return w.now() }

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
	live := make(map[int]bool, len(found))

	w.mu.Lock()
	var newProcs []agentusage.Process
	var replaced, exited []*tracked
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
	sortTracked(exited)
	// The replaced set is collected from the tracked map, so its order is the
	// map's. Stopping by PID keeps the sequence of stops, and the events they
	// emit, the same on every pass.
	sortTracked(replaced)
	slices.SortFunc(newProcs, func(a, b agentusage.Process) int {
		return cmp.Compare(a.PID, b.PID)
	})

	// Stop the reused-PID tracker before attaching its replacement: two
	// watchers tailing the same transcripts would double-count growth.
	for _, t := range replaced {
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
	w.mu.Lock()
	claimed := make(map[string]bool)
	for _, t := range w.tracked {
		if t.watch != nil {
			claimed[storeKey(t.proc)] = true
		}
	}
	w.mu.Unlock()

	// attach gives p its own watcher. follow inserts a tracker with none.
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
		if _, seen := w.tracked[p.PID]; seen {
			w.mu.Unlock()
			cancel()
			return
		}
		w.tracked[p.PID] = t
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
	var promote []agentusage.Process
	for _, t := range w.trackedList() {
		if t.watch == nil && !claimed[storeKey(t.proc)] {
			claimed[storeKey(t.proc)] = true
			promote = append(promote, t.proc)
		}
	}
	w.mu.Unlock()
	for _, p := range promote {
		attach(p)
	}

	for i, t := range started {
		go func(t *tracked, tctx context.Context) {
			defer close(t.done)
			t.watch.Run(tctx, w.readEvery, func(s agentusage.Sample) {
				w.report(t, s)
			})
		}(t, startCtx[i])
	}
	for _, t := range exited {
		w.stopOne(t)
	}

	w.mu.Lock()
	pids := slices.Sorted(maps.Keys(w.tracked))
	w.mu.Unlock()

	// Re-checked every pass rather than once at discovery: an agent connects
	// to its engine after it starts, and may switch engines mid-session.
	// One table read covers every tracked pid; matching each agent against
	// each engine separately would reread /proc/net/tcp per pair.
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

func (w *Watcher) stopOne(t *tracked) {
	if t.cancel != nil {
		t.cancel()
	}
	if t.done != nil {
		<-t.done
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
// be told which.
func parseEngineAddr(addr string) (netip.AddrPort, string, error) {
	host := addr
	// Only a scheme-bearing string is a URL. url.Parse rejects a bare
	// "127.0.0.1:8080" (a colon in the first path segment), and that spelling
	// is a documented input, so the parse is gated on the marker rather than
	// on whether it succeeds.
	if strings.Contains(addr, "://") {
		u, err := url.Parse(addr)
		if err != nil {
			return netip.AddrPort{}, "", fmt.Errorf("engine address %q is not a URL: %w", addr, err)
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
	if cur.Empty() {
		return
	}
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
		ViaEngine:      core.ClampField(core.SanitizeText(via), core.AgentViaMax),
		Note:           core.ClampField(core.RedactHome(core.SanitizeText(note(dir, d.Thinking, via))), core.AgentNoteMax),
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
