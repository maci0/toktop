// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A final Poll must see a transcript that appeared after the watcher started,
// however briefly ago. A short watch can finish inside the rescan interval,
// and then the file holding everything it spent is younger than the cached
// listing: reusing that listing reports zero for the whole attach.
func TestPollSeesATranscriptCreatedAfterTheWatchBegan(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on windows
	work := t.TempDir()
	other := t.TempDir()

	// Another project's transcript, present before the watch starts: it is
	// what fills the cached listing, and it must not be all Poll ever sees.
	elsewhere := filepath.Join(home, ".claude", "projects", "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","cwd":` + jsonPath(other) + `,"message":{"usage":{"output_tokens":99999}}}` + "\n"
	if err := os.WriteFile(filepath.Join(elsewhere, "s.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	w := Watch("claude", work, time.Now())
	if w == nil {
		t.Fatal("claude is readable, so Watch must return a watcher")
	}

	mine := filepath.Join(home, ".claude", "projects", "mine")
	if err := os.MkdirAll(mine, 0o755); err != nil {
		t.Fatal(err)
	}
	line = `{"type":"assistant","cwd":` + jsonPath(work) + `,"message":{"usage":{"output_tokens":42}}}` + "\n"
	if err := os.WriteFile(filepath.Join(mine, "s.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := w.Poll(); got.Output != 42 {
		t.Fatalf("final poll read %d output tokens, want the 42 this attach spent", got.Output)
	}
}

// Expired listings must leave the shared map, or every (root, suffix) pair
// a long --agents run ever walked would stay for the process lifetime.
func TestRootListCacheDropsExpiredKeys(t *testing.T) {
	rootListMu.Lock()
	rootLists = map[string]rootListing{
		"stale\x00.jsonl": {files: []string{"gone"}, at: time.Now().Add(-rescanEvery - time.Second)},
	}
	rootListMu.Unlock()
	t.Cleanup(func() {
		rootListMu.Lock()
		rootLists = map[string]rootListing{}
		rootListMu.Unlock()
	})

	dir := t.TempDir()
	now := time.Now()
	_ = listTranscripts(dir, ".jsonl", now.Add(-recencyWindow), now, false)

	rootListMu.Lock()
	_, still := rootLists["stale\x00.jsonl"]
	rootListMu.Unlock()
	if still {
		t.Fatal("expired listing still in the cache")
	}
}

func TestRootListCacheDropsExpiredKeysOnHit(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	rootListMu.Lock()
	rootLists = map[string]rootListing{
		"stale\x00.jsonl":          {files: []string{"gone"}, at: now.Add(-rescanEvery - time.Second)},
		rootListKey(dir, ".jsonl"): {files: []string{"fresh.jsonl"}, at: now},
	}
	rootListMu.Unlock()
	t.Cleanup(func() {
		rootListMu.Lock()
		rootLists = map[string]rootListing{}
		rootListMu.Unlock()
	})

	got := listTranscripts(dir, ".jsonl", now.Add(-recencyWindow), now, false)
	if len(got) != 1 || got[0] != "fresh.jsonl" {
		t.Fatalf("cached hit = %+v, want [fresh.jsonl]", got)
	}

	rootListMu.Lock()
	_, still := rootLists["stale\x00.jsonl"]
	rootListMu.Unlock()
	if still {
		t.Fatal("expired listing still in cache after cache hit")
	}
}

// Every caller that loses the race for a root's walk parks on the winner's
// claim channel, which nobody reads and nobody times out. The claim is
// therefore released on the way out of the walk, not on the normal path alone,
// and the entry it leaves behind must not still name it.
func TestListTranscriptsReleasesTheWalkClaim(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	key := rootListKey(dir, ".jsonl")
	t.Cleanup(func() {
		rootListMu.Lock()
		rootLists = map[string]rootListing{}
		rootListMu.Unlock()
	})

	if got := listTranscripts(dir, ".jsonl", now.Add(-recencyWindow), now, false); len(got) != 0 {
		t.Fatalf("empty store listed %v, want nothing", got)
	}
	rootListMu.Lock()
	c, ok := rootLists[key]
	rootListMu.Unlock()
	if !ok {
		t.Fatal("the walk left no cached listing behind")
	}
	if c.walk != nil {
		t.Fatal("the cached listing still holds a walk claim: later callers for this root would park forever")
	}

	// A caller arriving after the claim is released is served from the cache
	// rather than parking on a channel nobody will close.
	done := make(chan []string, 1)
	go func() {
		done <- listTranscripts(dir, ".jsonl", now.Add(-recencyWindow), now, false)
	}()
	select {
	case got := <-done:
		if len(got) != 0 {
			t.Fatalf("cached read = %v, want nothing", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a caller arriving after the walk parked on the claim: it was never released")
	}
}

// A walk older than the rescan interval is still in flight, and its claim is
// the only thing keeping a second goroutine off the same tree. Age alone
// cannot tell it from a stale result: the placeholder carries the walk's start
// instant, so a store slow enough to walk for longer than rescanEvery looks
// expired. Dropping it drops the claim with it, and the two walks then race to
// publish, so whichever started earlier can overwrite the newer listing and
// stamp a later at, suppressing a real refresh for a whole interval.
func TestRootListCacheKeepsTheClaimOfAWalkOlderThanTheRescanInterval(t *testing.T) {
	dir := t.TempDir()
	walk := make(chan struct{})
	rootListMu.Lock()
	rootLists = map[string]rootListing{
		rootListKey(dir, ".jsonl"): {at: time.Now().Add(-rescanEvery - time.Second), walk: walk},
	}
	rootListMu.Unlock()
	t.Cleanup(func() {
		rootListMu.Lock()
		rootLists = map[string]rootListing{}
		rootListMu.Unlock()
	})

	done := make(chan []string, 1)
	go func() {
		now := time.Now()
		done <- listTranscripts(dir, ".jsonl", now.Add(-recencyWindow), now, false)
	}()

	// The claim is the wait branch: a second walk would answer at once.
	select {
	case got := <-done:
		t.Fatalf("walked the same tree a second time while one was in flight, returned %+v", got)
	case <-time.After(50 * time.Millisecond):
	}

	// Publish the way the walker holding the claim does, then release it, so
	// the waiter re-checks and finds the listing the walk it waited for left.
	// Ordering matters: the entry has to stop claiming a walk before the
	// waiter can leave the wait branch.
	rootListMu.Lock()
	rootLists[rootListKey(dir, ".jsonl")] = rootListing{files: []string{"late.jsonl"}, at: time.Now()}
	rootListMu.Unlock()
	close(walk)

	select {
	case got := <-done:
		if len(got) != 1 || got[0] != "late.jsonl" {
			t.Fatalf("waiter got %+v, want the listing the in-flight walk published", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("waiter never returned after the walk it was waiting on finished")
	}
}

// The recency window belongs to the watcher's own clock. A caller that
// injects one has to be able to age a transcript out by stepping time, not by
// waiting it out: on the wall clock a replay that reaches this point within a
// second still sees every file a full-length run would have dropped, and the
// two runs read different session stores.
func TestRecencyWindowFollowsInjectedClock(t *testing.T) {
	store := withStore(t, "claude")
	work := t.TempDir()
	append_(t, filepath.Join(store, "session.jsonl"), claudeLine(work, 7))

	origin := time.Now()
	now := origin
	w := Watch("claude", work, origin)
	if w == nil {
		t.Fatal("claude should be supported")
	}
	w.SetNow(func() time.Time { return now })

	if got := w.candidates(); len(got) != 1 {
		t.Fatalf("candidates at attach = %v, want the one fresh transcript", got)
	}

	// A second of wall time passes here; the clock does not move, so the
	// listing must still be the cached one and the file still in window.
	time.Sleep(time.Millisecond)
	if got := w.candidates(); len(got) != 1 {
		t.Fatalf("candidates %s after the wall clock moved = %v, want the transcript still in window", rescanEvery, got)
	}

	// Step past the window. The file's mtime is unchanged, so only a clock
	// the watcher consults can drop it.
	now = origin.Add(recencyWindow + time.Second)
	if got := w.candidates(); len(got) != 0 {
		t.Fatalf("candidates after stepping the clock past the window = %v, want none", got)
	}
}

// A definition reloaded under a running watcher replaces the adapter, and the
// cached listing is the old one's answer: which roots and suffixes to walk. A
// reload that redirects the roots must be walked in the poll that read it, not
// a rescan window later, or the new spec's transcripts are read by nobody and
// the old tree's are still being read as if the spec had not changed.
func TestReloadWalksTheNewRootsInTheSamePoll(t *testing.T) {
	oldRoot, newRoot, work := t.TempDir(), t.TempDir(), t.TempDir()
	spec := func(root string) string {
		return `{"myagent": {"usage": {"roots": [` + jsonPath(root) + `], "suffix": ".jsonl"}}}`
	}
	path := writeDefs(t, spec(oldRoot))
	if err := LoadDefinitions(path); err != nil {
		t.Fatal(err)
	}
	dropDefs(t, "myagent")

	w := Watch("myagent", work, time.Now())
	if w == nil {
		t.Fatal("myagent is defined with a root, so Watch must return a watcher")
	}
	oldTranscript := filepath.Join(oldRoot, "s.jsonl")
	if err := os.WriteFile(oldTranscript, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	appendLine(t, oldTranscript, 7)
	// read, not Poll: a final Poll forces a fresh walk, which is the point
	// of Poll and would hide the listing this is about.
	if got, _ := w.read(false); got.Output != 7 {
		t.Fatalf("poll = %d output tokens, want the 7 under the root the spec named", got.Output)
	}

	// Redirect the definition, then write into the new root. The poll that
	// observes the reload is the one that has to list it.
	if err := os.WriteFile(path, []byte(spec(newRoot)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := LoadDefinitions(path); err != nil {
		t.Fatal(err)
	}
	newTranscript := filepath.Join(newRoot, "s.jsonl")
	if err := os.WriteFile(newTranscript, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	appendLine(t, newTranscript, 5)

	// 7 is still this attach's own spend and stays counted; 5 is the new
	// root's, and is what a stale listing would have missed for a rescan
	// window.
	if got, _ := w.read(false); got.Output != 12 {
		t.Errorf("poll after reload = %d output tokens, want 12: 7 already counted and 5 from the new root", got.Output)
	}
}

// appendLine adds one per-message record naming no working directory.
func appendLine(t *testing.T, path string, out int) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	line := `{"type":"assistant","message":{"usage":{"output_tokens":` + fmt.Sprint(out) + `}}}` + "\n"
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
}

// A walk that cannot finish is not an empty store. Caching its partial result
// under a fresh stamp would report every session under the failed subtree as
// absent for a whole rescan window, and the only symptom on screen would be an
// agent that produces no tokens. The entry must be left unstamped so the next
// caller re-walks, and the failure must be audited.
func TestWalkFailureIsNotCachedAsAFreshListing(t *testing.T) {
	var lines bytes.Buffer
	old := audit
	SetLogger(slog.New(slog.NewTextHandler(&lines, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer func() { audit = old }()
	t.Cleanup(func() {
		rootListMu.Lock()
		rootLists = map[string]rootListing{}
		rootListMu.Unlock()
	})

	now := time.Now()
	key := rootListKey("/nonexistent/transcript/root", ".jsonl")
	// A root that cannot be opened is the simplest walk that does not finish.
	if got := listTranscripts("/nonexistent/transcript/root", ".jsonl", now.Add(-recencyWindow), now, false); len(got) != 0 {
		t.Fatalf("failed walk listed %v, want nothing", got)
	}

	rootListMu.Lock()
	c, ok := rootLists[key]
	rootListMu.Unlock()
	if !ok {
		t.Fatal("the failed walk left no entry at all: the claim was never released")
	}
	if c.walk != nil {
		t.Fatal("the failed walk still holds its claim: later callers would park forever")
	}
	if !c.at.IsZero() {
		t.Fatalf("the failed walk cached a listing stamped %s; the next caller serves it as a fresh empty store", c.at)
	}
	if !strings.Contains(lines.String(), "agent transcript walk failed") {
		t.Fatalf("the failed walk wrote no audit line:\n%s", lines.String())
	}
}

// A failed walk is the one line this package writes, and the account name is
// in it twice: the root, and the path the walk failed on inside that root.
// Folding only the root would leave the home directory spelled out in the
// error beside it, and a host that installs its own logger has no fold of its
// own to catch that. The line is a diagnostic meant to be pasted into an
// issue, so neither value may carry the path under $HOME.
func TestWalkFailureAuditFoldsTheHomeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on windows

	var lines bytes.Buffer
	old := audit
	SetLogger(slog.New(slog.NewTextHandler(&lines, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer func() { audit = old }()

	// A store that is not there: the error names the path under $HOME the
	// walk could not open.
	root := filepath.Join(home, ".claude", "projects", "gone")
	now := time.Now()
	if got := listTranscripts(root, ".jsonl", now.Add(-recencyWindow), now, false); len(got) != 0 {
		t.Fatalf("failed walk listed %v, want nothing", got)
	}
	got := lines.String()
	if !strings.Contains(got, "agent transcript walk failed") {
		t.Fatalf("the failed walk wrote no audit line:\n%s", got)
	}
	if strings.Contains(got, home) {
		t.Errorf("the audit line spells out the home directory %q:\n%s", home, got)
	}
	// RedactHome folds with the platform separator, so the folded root is
	// spelled the way this platform spells a path.
	foldedRoot := "root=" + filepath.Join("~", ".claude", "projects", "gone")
	if !strings.Contains(got, foldedRoot) {
		t.Errorf("the audit line does not fold the root to ~:\n%s", got)
	}
}

// SetLogger is the seam a host program writes its audit lines through, and
// nil is its documented way back to the process logger. A restore that left
// the previous logger installed would keep a handler the host has let go of
// wired into every later walk.
func TestSetLoggerInstallsAndRestores(t *testing.T) {
	old := audit
	defer func() { audit = old }()
	var lines bytes.Buffer
	host := slog.New(slog.NewTextHandler(&lines, &slog.HandlerOptions{Level: slog.LevelDebug}))

	SetLogger(host)
	if got := audit(); got != host {
		t.Fatal("SetLogger did not install the logger it was given")
	}
	SetLogger(nil)
	if got := audit(); got != slog.Default() {
		t.Fatal("SetLogger(nil) did not restore the process logger")
	}
}
