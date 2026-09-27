// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Kimi Code CLI is the one adapter whose ownership comes from a file beside
// the transcript rather than a header inside it, so its fixtures build the
// store's real shape: <workDirKey>/<session>/state.json, and one
// agents/<agentId>/wire.jsonl per agent in the session.

func kimiState(cwd string) string {
	return `{"title":"session","cwd":` + jsonPath(cwd) + `}`
}

func kimiUsageLine(out, prompt, cached int) string {
	return `{"type":"usage.record","usage":{"inputOther":` + strconv.Itoa(prompt) +
		`,"inputCacheRead":` + strconv.Itoa(cached) + `,"inputCacheCreation":0,"output":` +
		strconv.Itoa(out) + `}}`
}

// kimiSession lays out one session directory under store and returns the path
// of one agent's wire log inside it.
func kimiSession(t *testing.T, store, session, agent, cwd string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(store, "-home-dev-proj", session, "agents", agent)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if cwd != "" {
		if err := os.WriteFile(filepath.Join(store, "-home-dev-proj", session, "state.json"),
			[]byte(kimiState(cwd)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "wire.jsonl")
	append_(t, path, lines...)
	return path
}

// kimiSessionCwd has to climb past the agents/<agentId> pair to the session
// directory state.json sits in. Reading one level short found no file there,
// so every kimi session came back unowned and the adapter reported no tokens
// at all rather than the wrong ones.
func TestKimiSessionCwdReadsTheStateFileBesideAgents(t *testing.T) {
	work := t.TempDir()
	store := t.TempDir()
	path := kimiSession(t, store, "s1", "a1", work)

	got, ok := kimiSessionCwd(path)
	if !ok {
		t.Fatal("kimiSessionCwd: no verdict for a session with a readable state.json")
	}
	if got != work {
		t.Fatalf("kimiSessionCwd = %q, want %q", got, work)
	}
}

// An unwritten or still-being-written state.json answers "no verdict" so the
// next poll retries, rather than refusing the session for good.
func TestKimiSessionCwdUndecidedWithoutAUsableStateFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		cwd  string
	}{
		{"never-written", ""},
		{"empty-cwd", `{"title":"session","cwd":""}`},
		{"not-json", "{not json"},
		{"truncated", `{"title":"session","cwd":"/home/de`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := t.TempDir()
			path := kimiSession(t, store, "s1", "a1", "")
			if tc.cwd != "" {
				session := filepath.Join(store, "-home-dev-proj", "s1")
				if err := os.WriteFile(filepath.Join(session, "state.json"), []byte(tc.cwd), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got, ok := kimiSessionCwd(path); ok {
				t.Fatalf("kimiSessionCwd = (%q, true), want no verdict", got)
			}
		})
	}
}

func TestKimiStoreHonorsAnAbsoluteHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KIMI_CODE_HOME", dir)
	if got, want := kimiStore(), filepath.Join(dir, "sessions"); got != want {
		t.Fatalf("kimiStore = %q, want %q", got, want)
	}
	// A relative value would place the store under whatever directory the run
	// started in, where it is empty and every session reads as no tokens.
	t.Setenv("KIMI_CODE_HOME", "relative/path")
	if got, want := kimiStore(), home(".kimi-code", "sessions"); got != want {
		t.Fatalf("kimiStore with a relative KIMI_CODE_HOME = %q, want %q", got, want)
	}
}

func TestKimiSessionUsageIsSummedAcrossCallsAndSubagents(t *testing.T) {
	store := withStore(t, "kimi")
	work := t.TempDir()

	w := Watch("kimi", work, time.Now())
	kimiSession(t, store, "s1", "a1", work, kimiUsageLine(120, 900, 100), kimiUsageLine(30, 40, 0))
	// A subagent's log sits under the same session and its own state.json
	// covers it, so it counts on its own tokens.
	kimiSession(t, store, "s1", "a2", work, kimiUsageLine(50, 0, 0))
	w.poll(nil)

	s := w.Sample()
	if s.Output != 200 {
		t.Fatalf("output tokens %d, want 200 (120 + 30 + 50)", s.Output)
	}
	// Cached input was billed too, so it folds into the prompt side: 900 + 100
	// for the first call, 40 for the second.
	if want := 1040; s.Input != want {
		t.Fatalf("input tokens %d, want %d", s.Input, want)
	}
}

func TestKimiIgnoresSessionsFromOtherDirectories(t *testing.T) {
	store := withStore(t, "kimi")
	work, other := t.TempDir(), t.TempDir()

	w := Watch("kimi", work, time.Now())
	kimiSession(t, store, "s1", "a1", other, kimiUsageLine(9999, 12345, 0))
	w.poll(nil)
	if got := w.Sample().Output; got != 0 {
		t.Fatalf("another directory's session was counted: %d", got)
	}
}

// The store is machine-wide, so an existing kimi session is baselined rather
// than read in full: attaching to a long-running project must not report its
// whole history as this watcher's tokens.
func TestKimiPreexistingSessionIsNotCountedInFull(t *testing.T) {
	store := withStore(t, "kimi")
	work := t.TempDir()
	kimiSession(t, store, "s1", "a1", work, kimiUsageLine(5000, 1000, 0))

	w := Watch("kimi", work, time.Now())
	w.poll(nil)
	if got := w.Sample().Output; got != 0 {
		t.Fatalf("a session that predated the watch reported %d tokens", got)
	}
	append_(t, filepath.Join(store, "-home-dev-proj", "s1", "agents", "a1", "wire.jsonl"), kimiUsageLine(80, 10, 0))
	w.poll(nil)
	if got := w.Sample().Output; got != 80 {
		t.Fatalf("output tokens %d, want 80: only the calls since the attach count", got)
	}
}
