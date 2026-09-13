// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentwatch

import (
	"encoding/json"
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

// recorder collects what the watcher reports.
type recorder struct {
	mu     sync.Mutex
	events []core.AgentEvent
}

func (r *recorder) RecordAgent(ev core.AgentEvent) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recorder) all() []core.AgentEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.AgentEvent(nil), r.events...)
}

func (w *Watcher) following(pid int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.tracked[pid]
	return ok
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
	script := "#!/bin/sh\nsleep 5\n"
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
	waitFor(t, 3*time.Second, func() bool { return w.following(cmd.Process.Pid) })
	appendLine(t, filepath.Join(transcript, "s.jsonl"), usageLine(work, 120))
	appendLine(t, filepath.Join(transcript, "s.jsonl"), usageLine(work, 240))

	waitFor(t, 3*time.Second, func() bool { return len(rec.all()) > 0 })

	var total int64
	for _, ev := range rec.all() {
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

	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 0.6\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin)
	cmd.Dir = t.TempDir()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	w := New(&recorder{}, nil)
	w.discoverEvery, w.readEvery = 100*time.Millisecond, time.Hour
	ctx := t.Context()
	go w.Run(ctx)

	// Scoped to this process: a developer machine usually has real agents
	// running, so a global count would never reach zero.
	pid := cmd.Process.Pid
	waitFor(t, 3*time.Second, func() bool { return w.following(pid) })
	_, _ = cmd.Process.Wait()
	waitFor(t, 3*time.Second, func() bool { return !w.following(pid) })
}

// TestSilentAgentProducesNoEvents is the honesty half: an agent that writes no
// usage must not appear as zero throughput.
func TestSilentAgentProducesNoEvents(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("discovery reads /proc")
	}
	t.Setenv("HOME", t.TempDir())

	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
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

	waitFor(t, 3*time.Second, func() bool { return w.following(cmd.Process.Pid) })
	// Followed, with nothing written to the transcript. A few read cycles
	// must stay silent; checking before discovery would pass even if a
	// silent agent invented events once it was tracked.
	deadline := time.Now().Add(3 * w.readEvery)
	for time.Now().Before(deadline) {
		for _, ev := range rec.all() {
			if ev.Note == shortDir(cmd.Dir) {
				t.Fatalf("invented an event for a silent agent: %+v", ev)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !w.following(cmd.Process.Pid) {
		t.Fatal("agent was forgotten before the silence check finished")
	}
}

func TestShortDirKeepsTheIdentifyingPart(t *testing.T) {
	// Pin home away from the fixture paths so this is the last-two-components
	// rule alone, not the home-stripping one.
	elsewhere := t.TempDir()
	t.Setenv("HOME", elsewhere)
	t.Setenv("USERPROFILE", elsewhere)
	cases := map[string]string{
		"/home/dev/src/project": "src/project",
		"/home/dev/project":     "dev/project",
		"project":               "project",
		"/":                     "/",
		// A backslash path is only a path where backslash is the separator.
		// Elsewhere it is one filename, kept whole minus the two components
		// the scan still finds in it.
		`C:\Users\dev\src\app`: `src\app`,
	}
	if runtime.GOOS == "windows" {
		cases[`C:\Users\dev\src\app`] = "src/app" // separators fold to '/'
		cases[`C:/Users/dev/project`] = "dev/project"
	}
	for in, want := range cases {
		if got := shortDir(in); got != want {
			t.Errorf("shortDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShortDirHidesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if got := shortDir(home); got != "~" {
		t.Errorf("home itself = %q, want ~", got)
	}
	if got := shortDir(""); got != "" {
		t.Errorf("empty = %q, want empty", got)
	}
	one := filepath.Join(home, "toktop")
	if got := shortDir(one); got != "~/toktop" {
		t.Errorf("project in home = %q, want ~/toktop", got)
	}
	two := filepath.Join(home, "src", "toktop")
	if got := shortDir(two); got != "src/toktop" {
		t.Errorf("nested under home = %q, want src/toktop", got)
	}
	outside := filepath.Join(filepath.Dir(home), "other", "proj")
	if got := shortDir(outside); strings.Contains(got, filepath.Base(home)) {
		t.Errorf("path outside home still names home: %q", got)
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
	t.Fatalf("condition not met within %s", limit)
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
	script := "#!/bin/sh\nexec 3<>/dev/tcp/127.0.0.1/" + port(t, ln) + "\nsleep 5\n"
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

	waitFor(t, 3*time.Second, func() bool { return w.following(cmd.Process.Pid) })
	waitFor(t, 3*time.Second, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		tr := w.tracked[cmd.Process.Pid]
		return tr != nil && tr.viaEngine != ""
	})
	appendLine(t, filepath.Join(transcript, "s.jsonl"), usageLine(work, 500))

	waitFor(t, 3*time.Second, func() bool { return len(rec.all()) > 0 })
	var out int64
	for _, ev := range rec.all() {
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
	w.read()

	appendLine(t, path, usageLine(work, 150))
	w.read()

	appendLine(t, path, usageLine(work, 180))
	tr.viaEngine = "http://127.0.0.1:8080"
	w.read()

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
	w, rec, _ := followClaude(t, work)

	quoted, err := json.Marshal(work)
	if err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","cwd":` + string(quoted) +
		`,"message":{"usage":{"input_tokens":900,"output_tokens":120,` +
		`"output_tokens_details":{"thinking_tokens":40}}}}`
	appendLine(t, filepath.Join(transcript, "s.jsonl"), line)
	w.read()

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
	w, rec, _ := followClaude(t, work)
	frozen := time.Unix(1_700_000_000, 0).UTC()
	w.SetNow(func() time.Time { return frozen })
	appendLine(t, filepath.Join(transcript, "s.jsonl"), usageLine(work, 120))
	w.read()
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

func (r *idRecorder) RecordAgent(ev core.AgentEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if core.HasAgentID(r.events, ev.ID) {
		return
	}
	r.events = append(r.events, ev)
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
	w.read()
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
	w.read()
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
	}{
		{in: "http://127.0.0.1:11434", want: must("127.0.0.1:11434"), label: "127.0.0.1:11434", ok: true},
		{in: "http://127.0.0.1", want: must("127.0.0.1:80"), label: "127.0.0.1:80", ok: true},
		{in: "https://127.0.0.1", want: must("127.0.0.1:443"), label: "127.0.0.1:443", ok: true},
		{in: "http://[::1]", want: must("[::1]:80"), label: "[::1]:80", ok: true},
		{in: "127.0.0.1:8080", want: must("127.0.0.1:8080"), label: "127.0.0.1:8080", ok: true},
		{in: "http://localhost:11434"}, // hostname: skip rather than DNS
	}
	for _, tt := range tests {
		ap, label, ok := parseEngineAddr(tt.in)
		if ok != tt.ok {
			t.Errorf("parseEngineAddr(%q) ok=%v, want %v", tt.in, ok, tt.ok)
			continue
		}
		if !tt.ok {
			continue
		}
		if ap != tt.want || label != tt.label {
			t.Errorf("parseEngineAddr(%q) = %v %q, want %v %q", tt.in, ap, label, tt.want, tt.label)
		}
	}
}

// A directory that is not on disk (removed mid-session, or not created yet)
// still lives under home, and the note must say so rather than printing the
// operator's username. The spellings only diverge when home is reached
// through a symlink, which on macOS is every temporary directory.
func TestShortDirHidesHomeForAPathNotOnDisk(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("HOME", link)
	t.Setenv("USERPROFILE", link)

	if got := shortDir(filepath.Join(link, "toktop")); got != "~/toktop" {
		t.Errorf("project in home = %q, want ~/toktop", got)
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
