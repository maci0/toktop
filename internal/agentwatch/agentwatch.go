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
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
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
	now        func() time.Time // event stamps; nil means time.Now

	mu      sync.Mutex
	tracked map[int]*tracked
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
	cancel    context.CancelFunc
	done      chan struct{}
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

// instant reads w.now. Kept as a method so poll/record sites stay one short
// call instead of repeating the field dereference.

func (w *Watcher) instant() time.Time { return w.now() }

// Run follows agents until the context is canceled. Call LoadDefinitions
// before Run so a malformed definitions file is reported where the operator
// can see it, not swallowed inside a goroutine behind the alt screen.
func (w *Watcher) Run(ctx context.Context) {
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
func sameProcess(tracked, found agentusage.Process) bool {
	if tracked.PID != found.PID {
		return false
	}
	if !tracked.Started.IsZero() && !found.Started.IsZero() {
		return tracked.Started.Equal(found.Started)
	}
	return tracked.Tool == found.Tool && tracked.Dir == found.Dir
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
	slices.SortFunc(newProcs, func(a, b agentusage.Process) int {
		return cmp.Compare(a.PID, b.PID)
	})

	// Stop the reused-PID tracker before attaching its replacement: two
	// watchers tailing the same transcripts would double-count growth.
	for _, t := range replaced {
		w.stopOne(t)
	}

	// Watch walks transcript stores; doing that under w.mu would stall
	// report() for agents already being followed. cancel is set before
	// the map insert so stopOne never observes a nil cancel.
	var started []*tracked
	var startCtx []context.Context
	for _, p := range newProcs {
		// A watcher counts only what is written after it attaches, so an agent
		// already halfway through a task contributes from here on rather than
		// retroactively. That keeps the rate honest at the cost of the first
		// part of a session toktop was not running for.
		//
		// since is wall time even when SetNow injects a simulated clock:
		// session stores record real timestamps, and comparing them to a
		// demo origin would count the whole history as this run.
		watch := p.Watch(time.Now())
		if watch == nil {
			continue // this agent keeps nothing readable
		}
		tctx, cancel := context.WithCancel(ctx)
		t := &tracked{proc: p, watch: watch, done: make(chan struct{}), cancel: cancel}
		w.mu.Lock()
		if _, seen := w.tracked[p.PID]; seen {
			w.mu.Unlock()
			cancel()
			continue
		}
		w.tracked[p.PID] = t
		w.mu.Unlock()
		started = append(started, t)
		startCtx = append(startCtx, tctx)
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
	pids := make([]int, 0, len(w.tracked))
	for pid := range w.tracked {
		pids = append(pids, pid)
	}
	w.mu.Unlock()
	slices.Sort(pids)

	// Re-checked every pass rather than once at discovery: an agent connects
	// to its engine after it starts, and may switch engines mid-session.
	// One table read covers every tracked pid; matching each agent against
	// each engine separately would reread /proc/net/tcp per pair.
	endpoints, labels := w.engineEndpoints()
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
// that can be compared against a process's open connections.
func (w *Watcher) engineEndpoints() ([]netip.AddrPort, []string) {
	if w.engines == nil {
		return nil, nil
	}
	raw := w.engines()
	eps := make([]netip.AddrPort, 0, len(raw))
	labels := make([]string, 0, len(raw))
	for _, addr := range raw {
		ap, label, ok := parseEngineAddr(addr)
		if !ok {
			continue
		}
		eps = append(eps, ap)
		labels = append(labels, label)
	}
	return eps, labels
}

// parseEngineAddr turns "http://127.0.0.1:11434" into an endpoint and a label.
// A hostname that is not an address cannot be compared against a connection
// table, so it is skipped rather than resolved: resolving would make a monitor
// do DNS on a timer. URLs that omit the port (http://127.0.0.1, https://…)
// use the scheme default: without it ParseAddrPort fails and an agent talking
// to that engine would not be labelled via, so its tokens would be counted
// twice.
func parseEngineAddr(addr string) (netip.AddrPort, string, bool) {
	host := addr
	if u, err := url.Parse(addr); err == nil && u.Host != "" {
		host = u.Host
		if u.Port() == "" {
			port := "80"
			if u.Scheme == "https" {
				port = "443"
			}
			host = net.JoinHostPort(u.Hostname(), port)
		}
	}
	ap, err := netip.ParseAddrPort(host)
	if err != nil {
		return netip.AddrPort{}, "", false
	}
	return ap, host, true
}

// trackedList is the followed agents in PID order. Report and shutdown
// sequences must not depend on map iteration, or equal-timestamp events
// land in a different order across replays. Caller holds w.mu.
func (w *Watcher) trackedList() []*tracked {
	out := make([]*tracked, 0, len(w.tracked))
	for _, t := range w.tracked {
		out = append(out, t)
	}
	sortTracked(out)
	return out
}

func sortTracked(ts []*tracked) {
	slices.SortFunc(ts, func(a, b *tracked) int {
		return cmp.Compare(a.proc.PID, b.proc.PID)
	})
}

func (w *Watcher) report(t *tracked, cur agentusage.Sample) {
	if cur.Empty() {
		return
	}
	w.mu.Lock()
	out := cur.Output - t.last.Output
	think := cur.Thinking - t.last.Thinking
	prompt := cur.Input - t.last.Input
	if out <= 0 && think <= 0 && prompt <= 0 {
		w.mu.Unlock()
		return // nothing new: silence is not an event
	}
	t.last = cur
	proc := t.proc
	via := t.viaEngine
	rec := w.rec
	w.mu.Unlock()
	if rec == nil {
		return
	}
	agent := core.ClampField(core.SanitizeText(proc.Tool), 64)
	if agent == "" {
		agent = "anonymous"
	}
	rec.RecordAgent(core.AgentEvent{
		At:             w.instant(),
		ID:             sampleID(proc, cur.At),
		Agent:          agent,
		Kind:           core.AgentKindTurn,
		PromptTokens:   int64(max(prompt, 0)),
		OutputTokens:   int64(max(out, 0)),
		ThinkingTokens: int64(max(think, 0)),
		ViaEngine:      core.ClampField(core.SanitizeText(via), 128),
		Note:           core.ClampField(core.SanitizeText(note(proc, think, via)), 512),
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

// note carries what the event cannot: where the agent is working, how much of
// the output was reasoning when the agent says so, and which monitored engine
// already counts this output when one does.
func note(p agentusage.Process, thinking int, via string) string {
	s := shortDir(p.Dir)
	if thinking > 0 {
		s += " · " + strconv.Itoa(thinking) + " reasoning"
	}
	if via != "" {
		s += " · counted by engine " + via
	}
	return s
}

// shortDir keeps the last two path components, which is what identifies a
// checkout without filling the row. A path under the operator's home is
// rewritten with ~ first so a username sitting in those last two components
// (home itself, or a project directly in it) never becomes the note.
// Separators are folded to '/' so a Windows path is shortened the same way
// as a Unix one.
func shortDir(dir string) string {
	if dir == "" {
		return ""
	}
	if stripped, ok := stripHome(dir); ok {
		dir = stripped
	}
	return lastTwoComponents(dir)
}

func stripHome(dir string) (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	cleanDir, cleanHome := resolvePath(dir), resolvePath(home)
	rel, err := filepath.Rel(cleanHome, cleanDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if rel == "." {
		return "~", true
	}
	return "~/" + filepath.ToSlash(rel), true
}

// maxPathWalk bounds how many ancestors resolvePath climbs. A path of
// one-byte components would otherwise recurse thousands of times; a real
// directory is far shorter.
const maxPathWalk = 256

// resolvePath cleans p and resolves the symlinks in it. A path that is not on
// disk resolves too: symlinks are evaluated on the deepest ancestor that does
// exist and the rest is appended, so a directory removed mid-session (or one
// an agent names before creating it) still compares equal to the same
// directory spelled another way.
func resolvePath(p string) string {
	p = filepath.Clean(p)
	var missing []string
	cur := p
	for range maxPathWalk {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return joinMissing(resolved, missing)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return joinMissing(cur, missing)
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
	return joinMissing(cur, missing)
}

// joinMissing rebuilds a path from an existing ancestor and the components
// that did not resolve, deepest first as resolvePath collected them.
func joinMissing(root string, missing []string) string {
	if len(missing) == 0 {
		return root
	}
	parts := make([]string, 0, 1+len(missing))
	parts = append(parts, root)
	for i := len(missing) - 1; i >= 0; i-- {
		parts = append(parts, missing[i])
	}
	return filepath.Join(parts...)
}

func lastTwoComponents(dir string) string {
	dir = filepath.ToSlash(dir)
	cut := 0
	for i := len(dir) - 1; i >= 0; i-- {
		if dir[i] == '/' || dir[i] == '\\' {
			cut++
			if cut == 2 {
				return dir[i+1:]
			}
		}
	}
	return dir
}
