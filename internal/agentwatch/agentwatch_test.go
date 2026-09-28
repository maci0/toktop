// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentwatch

import (
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maci0/toktop/agentusage"
	"github.com/maci0/toktop/internal/core"
)

// waitCeiling bounds every poll in this file. It is a ceiling on a broken
// watcher's runtime, not a budget for a working one: conditions are polled
// every 20ms and return as soon as they hold, so a loaded machine only
// lengthens the failing path, never a passing one.
const waitCeiling = 30 * time.Second

// Stub agents sleep this long. Every test that starts one kills it in a
// defer, so the sleep only has to outlast waitCeiling rather than pace the
// assertion: a stub that exits on its own can vanish before discovery sees
// it on a machine that is running the whole suite at once.
const stubSleep = 120

// recorder collects what the watcher reports.
type recorder struct {
	mu     sync.Mutex
	events []core.AgentEvent
}

func (r *recorder) RecordAgent(ev core.AgentEvent) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return true
}

func (r *recorder) all() []core.AgentEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.AgentEvent(nil), r.events...)
}

// forPID returns the events one discovered process produced. Discovery reads
// the real /proc, so a developer's own agents, or another checkout's test
// stubs, are followed and reported alongside; an assertion about one stub
// must not see them. sampleID leads with the PID, which is what the test
// holds.
func (r *recorder) forPID(pid int) []core.AgentEvent {
	prefix := "aw:" + strconv.Itoa(pid) + ":"
	var out []core.AgentEvent
	for _, ev := range r.all() {
		if strings.HasPrefix(ev.ID, prefix) {
			out = append(out, ev)
		}
	}
	return out
}

func (w *Watcher) following(pid int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.tracked[pid]
	return ok
}

// watching reports whether the tracker for pid has a watcher of its own, the
// difference between following a store and merely being on the dashboard.
func (w *Watcher) watching(pid int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.tracked[pid]
	return ok && t.watch != nil
}

// watchedPIDs returns the PIDs currently holding a store, in PID order so the
// choice of holder does not depend on map iteration.
func (w *Watcher) watchedPIDs() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []int
	for pid, t := range w.tracked {
		if t.watch != nil {
			out = append(out, pid)
		}
	}
	slices.Sort(out)
	return out
}

func claudeHome(t *testing.T) (work, transcript string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	work = t.TempDir()
	transcript = filepath.Join(home, ".claude", "projects", "p")
	if err := os.MkdirAll(transcript, 0o755); err != nil {
		t.Fatal(err)
	}
	return work, transcript
}

func followClaude(t *testing.T, work string) (*Watcher, *recorder, *tracked) {
	t.Helper()
	rec := &recorder{}
	w := New(rec, nil)
	tr := &tracked{
		proc:  agentusage.Process{PID: 1, Tool: "claude", Dir: work},
		watch: agentusage.Watch("claude", work, time.Now()),
	}
	if tr.watch == nil {
		t.Fatal("no claude adapter")
	}
	w.tracked[tr.proc.PID] = tr
	return w, rec, tr
}

// TestWatchesARunningAgent is the whole feature in one test: a process that
// looks like claude is running somewhere, it writes tokens into its own
// transcript, and toktop reports them without anyone pushing anything.
func TestWatchesARunningAgent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("discovery reads /proc")
	}
	work, transcript := claudeHome(t)

	// A process named like the agent, working in the directory the transcript
	// claims. Nothing about it cooperates with toktop.
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\nsleep " + strconv.Itoa(stubSleep) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin)
	cmd.Dir = work
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	rec := &recorder{}
	w := New(rec, nil)
	w.discoverEvery, w.readEvery = 200*time.Millisecond, 100*time.Millisecond
	ctx := t.Context()
	go w.Run(ctx)

	// Give discovery a chance to find the process, then let the agent "spend".
	waitFor(t, waitCeiling, func() bool { return w.following(cmd.Process.Pid) })
	appendLine(t, filepath.Join(transcript, "s.jsonl"), usageLine(work, 120))
	appendLine(t, filepath.Join(transcript, "s.jsonl"), usageLine(work, 240))

	pid := cmd.Process.Pid
	// Both lines land in one read or split across two, so the first event can
	// carry only the 120. Wait for the full spend before summing. Scoped to
	// this process, because discovery also follows whatever else is running.
	waitFor(t, waitCeiling, func() bool {
		var n int64
		for _, ev := range rec.forPID(pid) {
			n += ev.OutputTokens
		}
		return n == 360
	})

	var total int64
	for _, ev := range rec.forPID(pid) {
		if ev.Agent != "claude" {
			t.Fatalf("wrong agent: %+v", ev)
		}
		if ev.At.IsZero() {
			t.Fatalf("event without a timestamp cannot become a rate: %+v", ev)
		}
		total += ev.OutputTokens
	}
	if total != 360 {
		t.Fatalf("reported %d output tokens, want 360", total)
	}
}

// TestSurvivorTakesOverTheStore is the handover: two agents share one
// transcript store, only the first watches it, and when that one exits the
// survivor must pick the store up rather than stay on the dashboard with no
// watcher and no tokens.
func TestSurvivorTakesOverTheStore(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("discovery reads /proc")
	}
	work, transcript := claudeHome(t)

	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep "+strconv.Itoa(stubSleep)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var cmds []*exec.Cmd
	pids := make([]int, 0, 2)
	for range 2 {
		cmd := exec.Command(bin)
		cmd.Dir = work
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
		pids = append(pids, cmd.Process.Pid)
		defer func() {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}()
	}

	rec := &recorder{}
	w := New(rec, nil)
	w.discoverEvery, w.readEvery = 100*time.Millisecond, 50*time.Millisecond
	ctx := t.Context()
	go w.Run(ctx)

	// Exactly one of them watches the shared store; the other is tracked with
	// no watcher of its own. Scoped to these two PIDs: discovery reads the
	// real /proc, so the developer's own agents are followed alongside.
	holder := func() int {
		for _, p := range w.watchedPIDs() {
			if p == pids[0] || p == pids[1] {
				return p
			}
		}
		return 0
	}
	waitFor(t, waitCeiling, func() bool {
		return holder() != 0 && w.following(pids[0]) && w.following(pids[1])
	})
	holderPID, survivor := holder(), pids[0]
	if holderPID == survivor {
		survivor = pids[1]
	}
	holderCmd := cmds[0]
	if cmds[0].Process.Pid != holderPID {
		holderCmd = cmds[1]
	}
	if err := holderCmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = holderCmd.Process.Wait()

	// Written after the promotion, so only a watcher that attached after the
	// follower exited can report it.
	waitFor(t, waitCeiling, func() bool { return w.watching(survivor) })
	appendLine(t, filepath.Join(transcript, "s.jsonl"), usageLine(work, 700))
	waitFor(t, waitCeiling, func() bool {
		for _, ev := range rec.forPID(survivor) {
			if ev.OutputTokens >= 700 {
				return true
			}
		}
		return false
	})
}

func TestSameProcess(t *testing.T) {
	t1 := time.Unix(100, 0)
	t2 := time.Unix(200, 0)
	a := agentusage.Process{PID: 1, Tool: "claude", Dir: "/a", Started: t1}
	if !sameProcess(a, a) {
		t.Fatal("identical process not the same")
	}
	reused := a
	reused.Started = t2
	if sameProcess(a, reused) {
		t.Fatal("reused PID with a new start time treated as the same process")
	}
	otherPID := a
	otherPID.PID = 2
	if sameProcess(a, otherPID) {
		t.Fatal("different PIDs treated as the same process")
	}

	// Platforms that do not report a start time (Darwin) compare tool and dir.
	a.Started = time.Time{}
	if !sameProcess(a, a) {
		t.Fatal("zero start time rejected an identical process")
	}
	otherTool := a
	otherTool.Tool = "codex"
	if sameProcess(a, otherTool) {
		t.Fatal("zero start time ignored a tool change")
	}
	otherDir := a
	otherDir.Dir = "/b"
	if sameProcess(a, otherDir) {
		t.Fatal("zero start time ignored a directory change")
	}
}

// The directory a process is attributed to is compared the way the file
// system compares it, so two spellings of one checkout are one session on the
// platforms whose volumes fold and two on the platforms whose do not.
func TestSameProcessFoldsDirectorySpelling(t *testing.T) {
	a := agentusage.Process{PID: 1, Tool: "claude", Dir: filepath.Join("a", "b", "café")}
	for _, other := range []string{a.Dir + string(filepath.Separator), filepath.Join("a", "b", "café"), "a/b/other"} {
		b := a
		b.Dir = other
		if sameProcess(a, b) != agentusage.SameDir(a.Dir, other) {
			t.Errorf("sameProcess(%q, %q) disagrees with the platform's directory identity", a.Dir, other)
		}
	}
}

// A store is keyed by tool and working directory, and the key has to fold the
// directory the way the platform does: two spellings of one checkout that key
// apart let two watchers tail the same transcripts and count every event
// twice.
func TestStoreKeyFoldsDirectorySpelling(t *testing.T) {
	a := agentusage.Process{Tool: "claude", Dir: filepath.Join(t.TempDir(), "proj")}
	for _, other := range []string{a.Dir + string(filepath.Separator), filepath.ToSlash(a.Dir), a.Dir + "-other"} {
		b := a
		b.Dir = other
		if got, want := storeKey(a) == storeKey(b), agentusage.SameDir(a.Dir, other); got != want {
			t.Errorf("storeKey(%q) == storeKey(%q) is %v, want %v: the platform calls those one directory or two",
				a.Dir, other, got, want)
		}
	}
	if storeKey(a) == storeKey(agentusage.Process{Tool: "codex", Dir: a.Dir}) {
		t.Error("two tools writing to one directory are two stores")
	}
}

// A kernel-reused PID must not keep the previous process's tracker: events
// would carry the old tool/dir, and two watchers would double-count.
func TestPIDReuseRetargetsWatcher(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	work1 := t.TempDir()
	work2 := t.TempDir()
	pid := 4242
	first := agentusage.Process{PID: pid, Tool: "claude", Dir: work1, Started: time.Unix(100, 0)}
	next := agentusage.Process{PID: pid, Tool: "codex", Dir: work2, Started: time.Unix(200, 0)}
	var mu sync.Mutex
	cur := first

	w := New(&recorder{}, nil)
	w.readEvery = time.Hour
	w.listAgents = func() []agentusage.Process {
		mu.Lock()
		defer mu.Unlock()
		return []agentusage.Process{cur}
	}
	ctx := t.Context()
	defer w.stopAll()

	w.discover(ctx)
	if !w.following(pid) {
		t.Fatal("first process not followed")
	}
	w.mu.Lock()
	old := w.tracked[pid]
	w.mu.Unlock()
	if old == nil {
		t.Fatal("missing tracker")
	}

	mu.Lock()
	cur = next
	mu.Unlock()
	w.discover(ctx)

	select {
	case <-old.done:
	case <-time.After(2 * time.Second):
		t.Fatal("old watcher still running after PID reuse")
	}
	w.mu.Lock()
	got := w.tracked[pid]
	w.mu.Unlock()
	if got == nil {
		t.Fatal("replacement process not followed")
	}
	if got == old {
		t.Fatal("kept the stale tracker for a reused PID")
	}
	if got.proc.Tool != "codex" || got.proc.Dir != work2 {
		t.Fatalf("still following the old process: %+v", got.proc)
	}
}

func TestSamePIDKeepsTracker(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := agentusage.Process{PID: 7, Tool: "claude", Dir: t.TempDir(), Started: time.Unix(1, 0)}
	w := New(&recorder{}, nil)
	w.readEvery = time.Hour
	w.listAgents = func() []agentusage.Process { return []agentusage.Process{p} }
	ctx := t.Context()
	defer w.stopAll()

	w.discover(ctx)
	w.mu.Lock()
	first := w.tracked[p.PID]
	w.mu.Unlock()
	if first == nil {
		t.Fatal("not followed")
	}
	w.discover(ctx)
	w.mu.Lock()
	second := w.tracked[p.PID]
	w.mu.Unlock()
	if first != second {
		t.Fatal("replaced tracker for the same process")
	}
}

// TestForgetsExitedAgents keeps the tracking table from growing for the life
// of the process.
func TestForgetsExitedAgents(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("discovery reads /proc")
	}
	t.Setenv("HOME", t.TempDir())

	// The fake agent stays up until this test has seen it followed. One that
	// exits on its own races the watcher's first discover tick: the process
	// can be gone from /proc before the tick lands, and the test then fails
	// on a machine that is merely slow to start a goroutine.
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep "+strconv.Itoa(stubSleep)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin)
	cmd.Dir = t.TempDir()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	w := New(&recorder{}, nil)
	w.discoverEvery, w.readEvery = 100*time.Millisecond, time.Hour
	ctx := t.Context()
	go w.Run(ctx)

	// Scoped to this process: a developer machine usually has real agents
	// running, so a global count would never reach zero.
	pid := cmd.Process.Pid
	waitFor(t, waitCeiling, func() bool { return w.following(pid) })
	// Killed, not waited out: the test needs the process gone, and a
	// wall-clock exit would put a stub lifetime between "followed" and
	// "exited".
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()
	waitFor(t, waitCeiling, func() bool { return !w.following(pid) })
}

// TestSilentAgentProducesNoEvents is the honesty half: an agent that writes no
// usage must not appear as zero throughput.
func TestSilentAgentProducesNoEvents(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("discovery reads /proc")
	}
	t.Setenv("HOME", t.TempDir())

	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep "+strconv.Itoa(stubSleep)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin)
	cmd.Dir = t.TempDir()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	rec := &recorder{}
	w := New(rec, nil)
	w.discoverEvery, w.readEvery = 100*time.Millisecond, 50*time.Millisecond
	ctx := t.Context()
	go w.Run(ctx)

	waitFor(t, waitCeiling, func() bool { return w.following(cmd.Process.Pid) })
	// Followed, with nothing written to the transcript. The window has to
	// cover discovery as well as reads: it is a re-discovery that can
	// re-add the agent, so a few read cycles alone can end before the tick
	// under test. Two discovery ticks plus a few reads keeps a slow runner
	// from ending the window on a single delayed tick.
	deadline := time.Now().Add(2*w.discoverEvery + 3*w.readEvery)
	for time.Now().Before(deadline) {
		for _, ev := range rec.all() {
			if ev.Note == core.ShortDir(cmd.Dir) {
				t.Fatalf("invented an event for a silent agent: %+v", ev)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !w.following(cmd.Process.Pid) {
		t.Fatal("agent was forgotten before the silence check finished")
	}
}

// The path is quoted the way a real transcript quotes it: on Windows the
// separator is JSON's escape character, so a spliced-in path is not JSON.
func usageLine(cwd string, out int) string {
	quoted, err := json.Marshal(cwd)
	if err != nil {
		panic(err)
	}
	return `{"type":"assistant","cwd":` + string(quoted) +
		`,"message":{"usage":{"output_tokens":` + strconv.Itoa(out) + `}}}`
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, file, line, _ := runtime.Caller(1)
	t.Fatalf("condition not met within %s (%s:%d)", limit, filepath.Base(file), line)
}

// TestEngineTakesPrecedence is the double-counting guard: an agent generating
// through an engine toktop already measures is still reported (the dashboard
// has to show who is working) but every event is marked ViaEngine so
// aggregates can skip those tokens instead of adding them on top of the
// engine's.
func TestEngineTakesPrecedence(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("connection attribution reads /proc")
	}
	work, transcript := claudeHome(t)

	// Something that looks like a local inference engine.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	engine := "http://" + ln.Addr().String()

	// An agent that holds a connection to that engine while it works.
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\nexec 3<>/dev/tcp/127.0.0.1/" + port(t, ln) + "\nsleep " + strconv.Itoa(stubSleep) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/bash", bin) // /dev/tcp is a bash feature
	cmd.Dir = work
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	rec := &recorder{}
	w := New(rec, func() []string { return []string{engine} })
	w.discoverEvery, w.readEvery = 150*time.Millisecond, 100*time.Millisecond
	ctx := t.Context()
	go w.Run(ctx)

	waitFor(t, waitCeiling, func() bool { return w.following(cmd.Process.Pid) })
	waitFor(t, waitCeiling, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		tr := w.tracked[cmd.Process.Pid]
		return tr != nil && tr.viaEngine != ""
	})
	appendLine(t, filepath.Join(transcript, "s.jsonl"), usageLine(work, 500))

	pid := cmd.Process.Pid
	waitFor(t, waitCeiling, func() bool { return len(rec.forPID(pid)) > 0 })
	var out int64
	for _, ev := range rec.forPID(pid) {
		if ev.ViaEngine == "" {
			t.Fatalf("agent using the engine was not attributed: %+v", ev)
		}
		if !strings.Contains(ev.Note, "counted by engine") {
			t.Fatalf("attribution missing from the note: %+v", ev)
		}
		out += ev.OutputTokens
	}
	if out == 0 {
		t.Fatal("attributed agent produced no token events to display")
	}
}

func port(t *testing.T, ln net.Listener) string {
	t.Helper()
	_, p, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestAttributedAgentKeepsReporting pins display vs aggregate: an agent
// generating through a monitored engine still emits a turn per reading (the
// dashboard needs the deltas) and each event names the engine so totals can
// skip them. Switching engines updates the label on the next growth.
func TestAttributedAgentKeepsReporting(t *testing.T) {
	work, transcript := claudeHome(t)
	w, rec, tr := followClaude(t, work)

	path := filepath.Join(transcript, "s.jsonl")
	appendLine(t, path, usageLine(work, 100))
	tr.viaEngine = "http://127.0.0.1:11434"
	w.report(tr, tr.watch.Poll())

	appendLine(t, path, usageLine(work, 150))
	w.report(tr, tr.watch.Poll())

	appendLine(t, path, usageLine(work, 180))
	tr.viaEngine = "http://127.0.0.1:8080"
	w.report(tr, tr.watch.Poll())

	evs := rec.all()
	if len(evs) != 3 {
		t.Fatalf("got %d events, want one turn per reading: %+v", len(evs), evs)
	}
	// Claude transcripts are per-message: each line adds, it is not a running total.
	wantOut := []int64{100, 150, 180}
	wantVia := []string{"http://127.0.0.1:11434", "http://127.0.0.1:11434", "http://127.0.0.1:8080"}
	for i, ev := range evs {
		if ev.Kind != core.AgentKindTurn {
			t.Fatalf("event %d kind = %q, want turn: %+v", i, ev.Kind, ev)
		}
		if ev.OutputTokens != wantOut[i] {
			t.Fatalf("event %d output = %d, want %d: %+v", i, ev.OutputTokens, wantOut[i], ev)
		}
		if ev.ViaEngine != wantVia[i] {
			t.Fatalf("event %d ViaEngine = %q, want %q", i, ev.ViaEngine, wantVia[i])
		}
	}
}

// TestReportsPromptAndThinking is the input-side counterpart: a transcript
// that names prompt and reasoning tokens must not drop them, or the dashboard
// can only show completions.
func TestReportsPromptAndThinking(t *testing.T) {
	work, transcript := claudeHome(t)
	w, rec, tr := followClaude(t, work)

	quoted, err := json.Marshal(work)
	if err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","cwd":` + string(quoted) +
		`,"message":{"usage":{"input_tokens":900,"output_tokens":120,` +
		`"output_tokens_details":{"thinking_tokens":40}}}}`
	appendLine(t, filepath.Join(transcript, "s.jsonl"), line)
	w.report(tr, tr.watch.Poll())

	evs := rec.all()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(evs), evs)
	}
	ev := evs[0]
	if ev.PromptTokens != 900 || ev.OutputTokens != 120 || ev.ThinkingTokens != 40 {
		t.Fatalf("prompt/output/thinking = %d/%d/%d, want 900/120/40: %+v",
			ev.PromptTokens, ev.OutputTokens, ev.ThinkingTokens, ev)
	}
	if !strings.Contains(ev.Note, "40 reasoning") {
		t.Fatalf("reasoning missing from the note: %+v", ev)
	}
}

// Event stamps follow an injected clock so demo mode's simulated instant
// is what lands in the feed, not the transcript watcher's wall-clock read.
func TestReportStampsWithInjectedClock(t *testing.T) {
	work, transcript := claudeHome(t)
	w, rec, tr := followClaude(t, work)
	frozen := time.Unix(1_700_000_000, 0).UTC()
	w.SetNow(func() time.Time { return frozen })
	appendLine(t, filepath.Join(transcript, "s.jsonl"), usageLine(work, 120))
	w.report(tr, tr.watch.Poll())
	evs := rec.all()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(evs), evs)
	}
	if !evs[0].At.Equal(frozen) {
		t.Fatalf("At = %v, want injected %v", evs[0].At, frozen)
	}
}

// idRecorder drops a repeat of an id still in the list, matching the
// collector's window. Agentwatch events used to have no id, so a retried
// report of the same sample (final Poll after Run's last callback, a
// tracker that forgot its baseline) double-counted.
type idRecorder struct {
	mu     sync.Mutex
	events []core.AgentEvent
}

func (r *idRecorder) RecordAgent(ev core.AgentEvent) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if core.HasAgentID(r.events, ev.ID) {
		return false
	}
	r.events = append(r.events, ev)
	return true
}

func (r *idRecorder) all() []core.AgentEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.AgentEvent(nil), r.events...)
}

func TestReplayOfSameSampleKeptOnce(t *testing.T) {
	work, transcript := claudeHome(t)
	rec := &idRecorder{}
	w := New(rec, nil)
	tr := &tracked{
		proc:  agentusage.Process{PID: 1, Tool: "claude", Dir: work, Started: time.Unix(100, 0)},
		watch: agentusage.Watch("claude", work, time.Now()),
	}
	if tr.watch == nil {
		t.Fatal("no claude adapter")
	}
	w.tracked[tr.proc.PID] = tr

	appendLine(t, filepath.Join(transcript, "s.jsonl"), usageLine(work, 80))
	w.report(tr, tr.watch.Poll())
	evs := rec.all()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(evs), evs)
	}
	id := evs[0].ID
	if !strings.HasPrefix(id, "aw:1:") {
		t.Fatalf("id = %q, want aw:1:…", id)
	}

	// The delta guard would also skip this if last were kept; clearing it
	// is the replay: same sample, same instant, a second RecordAgent.
	tr.last = agentusage.Sample{}
	w.report(tr, tr.watch.Poll())
	if got := rec.all(); len(got) != 1 {
		t.Fatalf("replay recorded %d events, want 1: %+v", len(got), got)
	}
}

func TestSampleIDDistinguishesProcessAndInstant(t *testing.T) {
	at := time.Unix(50, 7)
	a := agentusage.Process{PID: 1, Started: time.Unix(10, 0)}
	b := agentusage.Process{PID: 2, Started: time.Unix(10, 0)}
	if sampleID(a, at) == sampleID(b, at) {
		t.Fatal("different PIDs produced the same id")
	}
	if sampleID(a, at) == sampleID(a, at.Add(time.Nanosecond)) {
		t.Fatal("different instants produced the same id")
	}
	got := sampleID(a, at)
	if again := sampleID(a, at); got != again {
		t.Fatalf("same process and instant must be stable: %q vs %q", got, again)
	}
}

// Equal-timestamp reports must follow PID order, not map iteration, so a
// replay of the same process set emits events in the same sequence.
func TestTrackedListOrdersByPID(t *testing.T) {
	w := New(nil, nil)
	w.tracked[20] = &tracked{proc: agentusage.Process{PID: 20, Tool: "z"}}
	w.tracked[7] = &tracked{proc: agentusage.Process{PID: 7, Tool: "a"}}
	w.tracked[13] = &tracked{proc: agentusage.Process{PID: 13, Tool: "m"}}
	w.mu.Lock()
	got := w.trackedList()
	w.mu.Unlock()
	want := []int{7, 13, 20}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, tr := range got {
		if tr.proc.PID != want[i] {
			t.Fatalf("index %d PID = %d, want %d", i, tr.proc.PID, want[i])
		}
	}
}

// URLs that omit the port still name a TCP endpoint (scheme default). Without
// that, an agent generating through http://127.0.0.1 would not be labelled
// via and its tokens would be added on top of the engine's.
func TestParseEngineAddrDefaultPorts(t *testing.T) {
	must := func(s string) netip.AddrPort {
		t.Helper()
		ap, err := netip.ParseAddrPort(s)
		if err != nil {
			t.Fatal(err)
		}
		return ap
	}
	tests := []struct {
		in    string
		want  netip.AddrPort
		label string
		ok    bool
		err   bool
	}{
		{in: "http://127.0.0.1:11434", want: must("127.0.0.1:11434"), label: "127.0.0.1:11434", ok: true},
		{in: "http://127.0.0.1", want: must("127.0.0.1:80"), label: "127.0.0.1:80", ok: true},
		{in: "https://127.0.0.1", want: must("127.0.0.1:443"), label: "127.0.0.1:443", ok: true},
		{in: "http://[::1]", want: must("[::1]:80"), label: "[::1]:80", ok: true},
		{in: "127.0.0.1:8080", want: must("127.0.0.1:8080"), label: "127.0.0.1:8080", ok: true},
		{in: "http://localhost:11434", label: "", ok: false}, // hostname: skip rather than DNS
		{in: "http://[::1", err: true}, // malformed: reported, not skipped
	}
	for _, tt := range tests {
		ap, label, err := parseEngineAddr(tt.in)
		if tt.err {
			if err == nil {
				t.Errorf("parseEngineAddr(%q) err=nil, want a parse error", tt.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseEngineAddr(%q) err=%v, want nil", tt.in, err)
			continue
		}
		if tt.ok {
			if ap != tt.want || label != tt.label {
				t.Errorf("parseEngineAddr(%q) = %v %q, want %v %q", tt.in, ap, label, tt.want, tt.label)
			}
			continue
		}
		// A host that is not a literal IP has no address to match on, and
		// resolving it here would turn an attribution lookup into a DNS
		// query. It must come back empty, and saying so keeps a regression
		// to a zero parse from passing as this case.
		if ap != (netip.AddrPort{}) || label != "" {
			t.Errorf("parseEngineAddr(%q) = %v %q, want an empty result", tt.in, ap, label)
		}
	}
}

func loadDefs() error {
	path := agentusage.DefinitionsPath()
	if path == "" {
		return nil
	}
	return agentusage.LoadDefinitions(path)
}

func TestLoadDefinitions(t *testing.T) {
	t.Run("missing file is success", func(t *testing.T) {
		t.Setenv("GAUNTLET_HOME", t.TempDir())
		if err := loadDefs(); err != nil {
			t.Fatalf("LoadDefinitions() = %v, want nil", err)
		}
	})
	t.Run("malformed file names itself", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "agents.json"), []byte("{oops"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GAUNTLET_HOME", dir)
		err := loadDefs()
		if err == nil {
			t.Fatal("LoadDefinitions() = nil, want error")
		}
		if !strings.Contains(err.Error(), "agents.json") {
			t.Fatalf("LoadDefinitions() = %v, want mention of agents.json", err)
		}
	})
	t.Run("valid file registers the agent", func(t *testing.T) {
		dir := t.TempDir()
		body := `{"deftest-agentwatch":{"usage":{"roots":["~/.deftest/sessions"]}}}`
		if err := os.WriteFile(filepath.Join(dir, "agents.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GAUNTLET_HOME", dir)
		if err := loadDefs(); err != nil {
			t.Fatalf("LoadDefinitions() = %v, want nil", err)
		}
		if !slices.Contains(agentusage.Agents(), "deftest-agentwatch") {
			t.Fatalf("defined agent missing from %v", agentusage.Agents())
		}
	})
}

func TestReportMixedScriptToolName(t *testing.T) {
	rec := &recorder{}
	w := New(rec, nil)
	tr := &tracked{
		proc: agentusage.Process{
			PID:  1234,
			Tool: "\u0441laude", // Cyrillic \u0441 + laude
		},
	}
	w.report(tr, agentusage.Sample{Output: 100})
	events := rec.all()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Agent != "anonymous" {
		t.Errorf("Agent = %q, want %q for mixed-script name", events[0].Agent, "anonymous")
	}
}

func TestEngineErrorRepeatsAfterRecovery(t *testing.T) {
	w := New(nil, func() []string { return nil })
	var got []string
	w.SetOnError(func(err error) { got = append(got, err.Error()) })
	bad := errors.New(`engine address "http://" is not a URL`)

	w.engineError(bad)
	w.engineError(bad) // still broken every tick: one report
	w.engineError(nil) // a clean tick: the condition went away
	w.engineError(bad) // and came back

	want := []string{bad.Error(), bad.Error()}
	if !slices.Equal(got, want) {
		t.Errorf("reported %q, want %q", got, want)
	}
}

// A store is followed once, and the process that loses the race is tracked
// without a watcher. When the follower's process exits, the survivor takes
// the store over instead of sitting on the dashboard reporting nothing.
func TestStoreHandoverFollowsTheSurvivor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	leader := agentusage.Process{PID: 9101, Tool: "claude", Dir: dir, Started: time.Unix(10, 0)}
	follower := agentusage.Process{PID: 9102, Tool: "claude", Dir: dir, Started: time.Unix(11, 0)}
	var mu sync.Mutex
	live := []agentusage.Process{leader, follower}

	w := New(&recorder{}, nil)
	w.readEvery = time.Hour
	w.listAgents = func() []agentusage.Process {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(live)
	}
	ctx := t.Context()
	defer w.stopAll()

	w.discover(ctx)
	w.mu.Lock()
	gotLeader, gotFollower := w.tracked[leader.PID], w.tracked[follower.PID]
	w.mu.Unlock()
	if gotLeader == nil || gotLeader.watch == nil {
		t.Fatal("the first process for a store must follow it")
	}
	if gotFollower == nil || gotFollower.watch != nil {
		t.Fatal("a second process on the same store must be tracked without a watcher")
	}

	mu.Lock()
	live = []agentusage.Process{follower}
	mu.Unlock()
	w.discover(ctx)

	w.mu.Lock()
	promoted := w.tracked[follower.PID]
	w.mu.Unlock()
	if promoted != gotFollower || promoted.watch == nil {
		t.Fatalf("the survivor did not take the store over: %+v", promoted)
	}
	if w.following(leader.PID) {
		t.Error("the exited follower is still tracked")
	}
	// The promoted watcher's goroutine is live: its done channel is open
	// while it runs and closes when it is stopped.
	select {
	case <-promoted.done:
		t.Fatal("the promoted watcher is not running")
	default:
	}
	stopped := make(chan struct{})
	go func() { w.stopOne(promoted); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the promoted watcher did not stop")
	}
}

// A transcript rewritten under the watcher republishes its counters at zero.
// That sample is a reading: the next growth is measured from it, not from
// figures the transcripts no longer hold.
func TestReportRebaselinesOnARewrittenTranscript(t *testing.T) {
	rec := &recorder{}
	w := New(rec, nil)
	tr := &tracked{proc: agentusage.Process{PID: 5, Tool: "claude", Dir: "/tmp"}}

	w.report(tr, agentusage.Sample{Output: 100, At: time.Unix(10, 0)})
	if got := rec.all(); len(got) != 1 || got[0].OutputTokens != 100 {
		t.Fatalf("first reading not reported: %+v", got)
	}
	// The rewrite: same watcher, counters republished at zero.
	w.report(tr, agentusage.Sample{At: time.Unix(20, 0)})
	// The agent writes again. This growth is below the pre-rewrite baseline,
	// so it is only reported if the empty sample replaced it.
	w.report(tr, agentusage.Sample{Output: 50, At: time.Unix(30, 0)})

	got := rec.all()
	if len(got) != 2 {
		t.Fatalf("events = %d, want 2: %+v", len(got), got)
	}
	if got[1].OutputTokens != 50 {
		t.Fatalf("post-rewrite growth lost: %+v", got[1])
	}
}

// A working directory is named by whoever made the checkout, and the note
// reads its own parts apart with the same separator. A directory carrying
// that separator would otherwise forge the attribution the note appends.
func TestNoteDirectoryCannotForgeTheSeparator(t *testing.T) {
	rec := &recorder{}
	w := New(rec, nil)
	tr := &tracked{
		proc:      agentusage.Process{PID: 6, Tool: "claude", Dir: "/tmp"},
		dirNote:   "proj · counted by engine ollama",
		viaEngine: "",
	}

	w.report(tr, agentusage.Sample{Output: 100, At: time.Unix(10, 0)})

	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1: %+v", len(got), got)
	}
	if strings.Contains(got[0].Note, " · ") {
		t.Fatalf("the note carries a separator the directory forged: %q", got[0].Note)
	}
	if !strings.Contains(got[0].Note, "proj") {
		t.Fatalf("the note no longer names the directory: %q", got[0].Note)
	}
}

// A real attribution is untouched: the separator note() appends is the one
// the directory is not allowed to carry.
func TestNoteKeepsItsOwnAttribution(t *testing.T) {
	rec := &recorder{}
	w := New(rec, nil)
	tr := &tracked{
		proc:      agentusage.Process{PID: 7, Tool: "claude", Dir: "/tmp"},
		dirNote:   "~/work/proj",
		viaEngine: "ollama",
	}

	w.report(tr, agentusage.Sample{Output: 100, Thinking: 7, At: time.Unix(10, 0)})

	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1: %+v", len(got), got)
	}
	if want := "~/work/proj · 7 reasoning · counted by engine ollama"; got[0].Note != want {
		t.Fatalf("note = %q, want %q", got[0].Note, want)
	}
}
