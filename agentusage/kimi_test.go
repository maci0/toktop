// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// The fixtures below are the real record shapes, reduced to the fields this
// package reads: a usage.record event per model call, taken from a wire log
// Kimi Code CLI wrote itself.

// kimiUsageLine is one usage.record event, as the CLI writes it.
func kimiUsageLine(inOther, out, cacheRead, cacheCreate int) string {
	return `{"type":"usage.record","agentId":"main","model":"kimi/k2","usage":{"inputOther":` +
		strconv.Itoa(inOther) + `,"output":` + strconv.Itoa(out) + `,"inputCacheRead":` + strconv.Itoa(cacheRead) +
		`,"inputCacheCreation":` + strconv.Itoa(cacheCreate) + `},"usageScope":"turn","time":1788071978567}`
}

// kimiHome points the adapter at a temporary store and returns it. The store
// is the store; the per-project directory inside it is what the roots derive,
// so the layout here is the one the CLI writes.
func kimiHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("KIMI_CODE_HOME", home)
	return filepath.Join(home, "sessions")
}

// kimiWorkDir is a working directory in the spelling the watcher compares in,
// which is also the spelling the key is hashed from.
func kimiWorkDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// kimiSession creates one session directory for work under store: the
// project's key directory, its state.json, and a wire log per named agent
// (main when none is named). It returns the session directory.
func kimiSession(t *testing.T, store, work, session string, agents ...string) string {
	t.Helper()
	dir := filepath.Join(store, "wd_proj_"+kimiWorkDirKey(work), session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state.json")
	if err := os.WriteFile(state, []byte(`{"id":"`+session+`","version":2,"cwd":`+jsonPath(work)+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(agents) == 0 {
		agents = []string{"main"}
	}
	for _, a := range agents {
		agentDir := filepath.Join(dir, "agents", a)
		if err := os.MkdirAll(agentDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(agentDir, "wire.jsonl"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// kimiWire is one agent's wire log inside a session directory.
func kimiWire(dir, agent string) string {
	return filepath.Join(dir, "agents", agent, "wire.jsonl")
}

// The key is the CLI's own naming, with the slug it prints in front of the
// hash: both pairs were read off a live store.
func TestKimiWorkDirKeyMatchesTheCLI(t *testing.T) {
	for _, tc := range []struct{ cwd, key string }{
		{"/home/maci/Desktop/7dtd/zdtd", "a0a18387af9f"},
		{"/home/maci/gauntlet/.gauntlet/worktrees/20260825T180119Z-5eaf-l2-25-llm-review", "ba62bd4208d9"},
	} {
		if got := kimiWorkDirKey(tc.cwd); got != tc.key {
			t.Errorf("kimiWorkDirKey(%q) = %q, want %q", tc.cwd, got, tc.key)
		}
	}
}

// kimiStoreTree builds the store a Kimi Code CLI session writes, the shape the
// CLI uses on this platform: sessions/<workDirKey>/<session>/state.json beside
// an agents/ directory of wire logs, one per agent.
func kimiStoreTree(t *testing.T, cwd string, agents ...string) (wire string) {
	t.Helper()
	store := t.TempDir()
	session := filepath.Join(store, "wd_key", "session-1")
	if err := os.MkdirAll(filepath.Join(session, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session, "state.json"), []byte(`{"cwd":`+jsonPath(cwd)+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(agents) == 0 {
		agents = []string{"main"}
	}
	for _, a := range agents {
		dir := filepath.Join(session, "agents", a)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "wire.jsonl"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if a == agents[0] {
			wire = filepath.Join(dir, "wire.jsonl")
		}
	}
	return wire
}

// A wire log's session directory is above the agents/ directory, not beside
// it. Reading two levels up finds agents/ instead, so the cwd is never read and
// every kimi session reports nothing, which the dashboard renders as an agent
// that ran and spent nothing.
func TestKimiSessionCwdReadsTheCLILayout(t *testing.T) {
	work := t.TempDir()
	wire := kimiStoreTree(t, work, "main")
	cwd, ok := kimiSessionCwd(wire)
	if !ok || cwd != work {
		t.Fatalf("kimiSessionCwd(%q) = %q, %v; want %q, true", wire, cwd, ok, work)
	}
}

// A subagent's log is a session of its own and counts its own tokens, so the
// cwd is read for every agent directory, not only the main one.
func TestKimiSessionCwdReadsSubagentLogs(t *testing.T) {
	work := t.TempDir()
	wire := kimiStoreTree(t, work, "main", "agent-3", "agent-7")
	for _, a := range []string{"main", "agent-3", "agent-7"} {
		path := filepath.Join(filepath.Dir(filepath.Dir(wire)), a, "wire.jsonl")
		if cwd, ok := kimiSessionCwd(path); !ok || cwd != work {
			t.Fatalf("kimiSessionCwd(%q) = %q, %v; want %q, true", path, cwd, ok, work)
		}
	}
}

// Every usage.record is one call's own counts, so they are added up; the
// cached shares are billed prompt tokens and are folded into the input; and
// the token_counting.measured event beside them is a context size, not usage.
func TestKimiUsageRecordsAreSummed(t *testing.T) {
	store := kimiHome(t)
	work := kimiWorkDir(t)
	path := kimiWire(kimiSession(t, store, work, "session_a"), "main")

	w := Watch("kimi", work, time.Now())
	if w == nil {
		t.Fatal("kimi should be supported")
	}
	append_(t, path,
		kimiUsageLine(12854, 318, 14848, 0),
		`{"type":"token_counting.measured","agentId":"main","length":3,"tokens":23134,"time":1788071978568}`,
		kimiUsageLine(832, 312, 28928, 0))
	w.poll(nil)

	s := w.Sample()
	if s.Output != 630 {
		t.Fatalf("output tokens %d, want 630 (318+312)", s.Output)
	}
	if s.Input != 57462 {
		t.Fatalf("input tokens %d, want 57462 (12854+14848 + 832+28928)", s.Input)
	}
	// Total is the largest per-request context size, not a sum.
	if s.Total != 30072 {
		t.Fatalf("total tokens %d, want 30072 (832+28928+312)", s.Total)
	}
}

// Another project's session must not land in this run.
func TestKimiIgnoresOtherProjects(t *testing.T) {
	store := kimiHome(t)
	work, other := kimiWorkDir(t), kimiWorkDir(t)
	mine := kimiWire(kimiSession(t, store, work, "session_mine"), "main")
	theirs := kimiWire(kimiSession(t, store, other, "session_theirs"), "main")

	w := Watch("kimi", work, time.Now())
	append_(t, mine, kimiUsageLine(10, 70, 0, 0))
	append_(t, theirs, kimiUsageLine(10, 5000, 0, 0))
	w.poll(nil)
	if got := w.Sample().Output; got != 70 {
		t.Fatalf("output tokens %d, want 70: another project's session leaked in", got)
	}
}

// A subagent writes its own wire log under the same session directory, and
// its tokens are its own, so both logs count.
func TestKimiSubagentWireLogsCount(t *testing.T) {
	store := kimiHome(t)
	work := kimiWorkDir(t)
	dir := kimiSession(t, store, work, "session_s", "main", "agent-0")

	w := Watch("kimi", work, time.Now())
	append_(t, kimiWire(dir, "main"), kimiUsageLine(100, 20, 0, 0))
	append_(t, kimiWire(dir, "agent-0"), kimiUsageLine(300, 40, 0, 0))
	w.poll(nil)
	if got := w.Sample().Output; got != 60 {
		t.Fatalf("output tokens %d, want 60 (20+40): a subagent's log was missed", got)
	}
}

// A dashboard started before the agent's first session in a directory still
// finds it: the project's directory appears after the watcher attached, which
// is the ordinary order for a fresh working directory.
func TestKimiFindsASessionDirectoryThatAppearsLater(t *testing.T) {
	store := kimiHome(t)
	work := kimiWorkDir(t)

	w := Watch("kimi", work, time.Now())
	w.poll(nil)
	if got := w.Sample().Output; got != 0 {
		t.Fatalf("output tokens %d, want 0 before any session exists", got)
	}
	path := kimiWire(kimiSession(t, store, work, "session_late"), "main")
	append_(t, path, kimiUsageLine(10, 70, 0, 0))
	w.Poll() // a final read: forces the walk, and the root derivation with it
	if got := w.Sample().Output; got != 70 {
		t.Fatalf("output tokens %d, want 70: the project directory was not picked up", got)
	}
}

// KIMI_CODE_HOME moves the store, but only when absolute: a relative value
// would put it under whatever directory the run started in.
func TestKimiStoreHonorsOnlyAnAbsoluteHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KIMI_CODE_HOME", dir)
	if got, want := kimiStore(), filepath.Join(dir, "sessions"); got != want {
		t.Fatalf("kimiStore() = %q, want %q", got, want)
	}
	t.Setenv("KIMI_CODE_HOME", "relative/home")
	if got, want := kimiStore(), home(".kimi-code", "sessions"); got != want {
		t.Fatalf("kimiStore() = %q, want the default %q", got, want)
	}
}

// A session with no state.json yet yields no verdict, so the transcript is
// retried on a later poll rather than refused for the life of the session. The
// walk must stop at the store, not climb into the user's home and read some
// other program's state.json.
func TestKimiSessionCwdWithoutAStateFile(t *testing.T) {
	store := t.TempDir()
	session := filepath.Join(store, "wd_key", "session-1")
	wire := filepath.Join(session, "agents", "main", "wire.jsonl")
	if err := os.MkdirAll(filepath.Dir(wire), 0o755); err != nil {
		t.Fatal(err)
	}
	if cwd, ok := kimiSessionCwd(wire); ok {
		t.Fatalf("a session with no state.json answered %q", cwd)
	}
}

// The session directory is agent-writable, so a state.json planted as a
// symlink out of it is refused the same way a transcript symlink is.
func TestKimiSessionCwdRefusesALinkedStateFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs a privilege Windows does not grant by default")
	}
	outside := t.TempDir()
	target := filepath.Join(outside, "state.json")
	if err := os.WriteFile(target, []byte(`{"cwd":"/somewhere/else"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	store := t.TempDir()
	session := filepath.Join(store, "wd_key", "session-1")
	wire := filepath.Join(session, "agents", "main", "wire.jsonl")
	if err := os.MkdirAll(filepath.Dir(wire), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(session, "state.json")); err != nil {
		t.Fatal(err)
	}
	if cwd, ok := kimiSessionCwd(wire); ok {
		t.Fatalf("a linked state.json answered %q", cwd)
	}
}
