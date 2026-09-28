// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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

// swapRootLists installs m as the process-global listing cache for the
// duration of the test and puts the previous map back on return. Leaving a
// fabricated listing behind would make the rest of the suite's results depend
// on which of these tests ran first.
func swapRootLists(t *testing.T, m map[string]rootListing) func() {
	t.Helper()
	rootListMu.Lock()
	saved := rootLists
	rootLists = m
	rootListMu.Unlock()
	return func() {
		rootListMu.Lock()
		rootLists = saved
		rootListMu.Unlock()
	}
}

// Expired listings must leave the shared map, or every (root, suffix) pair
// a long --agents run ever walked would stay for the process lifetime.
func TestRootListCacheDropsExpiredKeys(t *testing.T) {
	defer swapRootLists(t, map[string]rootListing{
		"stale\x00.jsonl": {files: []string{"gone"}, at: time.Now().Add(-rescanEvery - time.Second)},
	})()

	dir := t.TempDir()
	now := time.Now()
	_, _ = listTranscripts(dir, []string{".jsonl"}, now.Add(-recencyWindow), now, false)

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
	defer swapRootLists(t, map[string]rootListing{
		"stale\x00.jsonl":                    {files: []string{"gone"}, at: now.Add(-rescanEvery - time.Second)},
		rootListKey(dir, []string{".jsonl"}): {files: []string{"fresh.jsonl"}, at: now},
	})()

	got, _ := listTranscripts(dir, []string{".jsonl"}, now.Add(-recencyWindow), now, false)
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
	key := rootListKey(dir, []string{".jsonl"})
	defer swapRootLists(t, map[string]rootListing{})()

	if got, _ := listTranscripts(dir, []string{".jsonl"}, now.Add(-recencyWindow), now, false); len(got) != 0 {
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
		files, _ := listTranscripts(dir, []string{".jsonl"}, now.Add(-recencyWindow), now, false)
		done <- files
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
	defer swapRootLists(t, map[string]rootListing{
		rootListKey(dir, []string{".jsonl"}): {at: time.Now().Add(-rescanEvery - time.Second), walk: walk},
	})()

	done := make(chan []string, 1)
	entered := make(chan struct{})
	go func() {
		// Closed before the call, not after: a waiter that reaches the wait
		// branch parks on the claim, and the signal has to say the goroutine
		// is running rather than that it is parked. How long it parks is
		// bounded (walkWait), so a signal from inside the branch would race
		// that bound instead of reporting the wait.
		close(entered)
		now := time.Now()
		files, _ := listTranscripts(dir, []string{".jsonl"}, now.Add(-recencyWindow), now, false)
		done <- files
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never started")
	}

	// The claim is the wait branch: a caller that walks the tree a second
	// time takes ownership of the entry, replacing the claim this test
	// installed with one of its own. Ownership is therefore what says the
	// waiter parked, and it is visible in the map however long the wait
	// branch goes on holding.
	for deadline := time.Now().Add(50 * time.Millisecond); time.Now().Before(deadline); {
		rootListMu.Lock()
		held := rootLists[rootListKey(dir, []string{".jsonl"})].walk
		rootListMu.Unlock()
		if held != walk {
			t.Fatalf("walked the same tree a second time while one was in flight, returned %+v", <-done)
		}
		time.Sleep(time.Millisecond)
	}

	// Publish the way the walker holding the claim does, then release it, so
	// the waiter re-checks and finds the listing the walk it waited for left.
	// Ordering matters: the entry has to stop claiming a walk before the
	// waiter can leave the wait branch.
	rootListMu.Lock()
	rootLists[rootListKey(dir, []string{".jsonl"})] = rootListing{files: []string{"late.jsonl"}, at: time.Now()}
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
	// A root that exists but is not a directory is a walk that does not finish.
	// A path that is simply absent is an empty store, covered separately.
	root := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(root, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	key := rootListKey(root, []string{".jsonl"})
	if got, _ := listTranscripts(root, []string{".jsonl"}, now.Add(-recencyWindow), now, false); len(got) != 0 {
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

	// A store path that is a file: the open fails, and the error names the
	// path under $HOME the walk could not open.
	root := filepath.Join(home, ".claude", "projects", "not-a-directory")
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if got, _ := listTranscripts(root, []string{".jsonl"}, now.Add(-recencyWindow), now, false); len(got) != 0 {
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
	foldedRoot := "root=" + filepath.Join("~", ".claude", "projects", "not-a-directory")
	if !strings.Contains(got, foldedRoot) {
		t.Errorf("the audit line does not fold the root to ~:\n%s", got)
	}
}

// A transcript root that has not been created yet is an empty store. Clanker
// reads <project>/state, and a deleted zig-cache temp is the same shape:
// warning once a second per path is the flood this guards against. The empty
// answer is cached for one rescan, and a directory that appears after that
// is listed.
func TestMissingTranscriptRootIsAnEmptyStore(t *testing.T) {
	var lines bytes.Buffer
	old := audit
	SetLogger(slog.New(slog.NewTextHandler(&lines, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer func() { audit = old }()

	root := filepath.Join(t.TempDir(), "state")
	key := rootListKey(root, []string{"token_stats.jsonl"})
	t.Cleanup(func() {
		rootListMu.Lock()
		delete(rootLists, key)
		rootListMu.Unlock()
	})

	now := time.Now()
	cutoff := now.Add(-recencyWindow)
	if got, _ := listTranscripts(root, []string{"token_stats.jsonl"}, cutoff, now, false); len(got) != 0 {
		t.Fatalf("missing root listed %v", got)
	}
	if strings.Contains(lines.String(), "agent transcript walk failed") {
		t.Fatalf("a missing root was audited as a failed walk:\n%s", lines.String())
	}
	rootListMu.Lock()
	c := rootLists[key]
	rootListMu.Unlock()
	if c.at.IsZero() {
		t.Fatal("a missing root was left unstamped, so every rescan walks it again and warns")
	}

	// Still inside the rescan window: the cached empty listing stands, and
	// creating the directory does not show up yet.
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(root, "token_stats.jsonl")
	if err := os.WriteFile(name, []byte("{\"output_tokens\":3}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := listTranscripts(root, []string{"token_stats.jsonl"}, cutoff, now, false); len(got) != 0 {
		t.Fatalf("cached empty listing listed %v", got)
	}

	later := now.Add(rescanEvery)
	got, _ := listTranscripts(root, []string{"token_stats.jsonl"}, cutoff, later, false)
	if len(got) != 1 || got[0] != name {
		t.Fatalf("after rescan = %v, want [%s]", got, name)
	}
	if strings.Contains(lines.String(), "agent transcript walk failed") {
		t.Fatalf("the rescan audited a walk that finished:\n%s", lines.String())
	}
}

// Clanker's state directory is the project it was started in plus "state".
// A zig test leaves the working directory deleted. Watch and Poll must not
// warn, and a log that shows up afterwards is still counted.
func TestClankerMissingStateDirIsQuiet(t *testing.T) {
	var lines bytes.Buffer
	old := audit
	SetLogger(slog.New(slog.NewTextHandler(&lines, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer func() { audit = old }()

	work := t.TempDir()
	w := Watch("clanker", work, time.Now())
	if w == nil {
		t.Fatal("clanker watcher")
	}
	if s := w.Poll(); !s.Empty() {
		t.Fatalf("missing state dir counted %+v", s)
	}
	if strings.Contains(lines.String(), "agent transcript walk failed") {
		t.Fatalf("missing state dir warned:\n%s", lines.String())
	}

	dir := filepath.Join(work, "state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token_stats.jsonl"), []byte(
		"{\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := w.Poll()
	if s.Input != 10 || s.Output != 4 {
		t.Fatalf("sample = %+v, want input 10 output 4", s)
	}
	if strings.Contains(lines.String(), "agent transcript walk failed") {
		t.Fatalf("reading the new log warned:\n%s", lines.String())
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

// A walk that could not finish leaves the shared listing unstamped, and the
// watcher's own listing has to be left that way too. Stamping it would hold the
// watcher off its roots for a rescan window, and ageing its bookkeeping against
// a listing that missed the store would read the shortfall as files gone and
// release the read positions of transcripts still on disk: the next append to
// one of them would be read from byte zero and the session billed twice.
func TestFailedWalkIsNotStampedAsTheWatchersListing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Cleanup(func() {
		rootListMu.Lock()
		rootLists = map[string]rootListing{}
		rootListMu.Unlock()
	})

	// A root that exists but is not a directory is a walk that does not finish.
	// A path that is simply absent is an empty store, which is a whole answer.
	root := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(root, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := `{"unreadable": {"usage": {"roots": [` + jsonPath(root) + `], "suffix": ".jsonl"}}}`
	if err := LoadDefinitions(writeDefs(t, spec)); err != nil {
		t.Fatal(err)
	}
	dropDefs(t, "unreadable")

	work := t.TempDir()
	w := Watch("unreadable", work, time.Now())
	if w == nil {
		t.Fatal("the spec names a root, so Watch must return a watcher")
	}
	// Bookkeeping a transcript the walk never reached still holds its read
	// position and its committed-bytes carry.
	const path = "/store/session.jsonl.zstd"
	w.offsets[path] = 4096
	w.readFailed[path] = true
	w.zstdCarry[path] = []byte(`{"usage":`)

	if got := w.candidates(); len(got) != 0 {
		t.Fatalf("unreadable store listed %v, want nothing", got)
	}
	if !w.scanned.IsZero() {
		t.Fatal("the failed walk was stamped as this watcher's listing: the next poll would serve the empty store for a rescan window")
	}
	if w.cached != nil {
		t.Fatalf("the failed walk was kept as the cached listing: %v", w.cached)
	}
	if w.offsets[path] != 4096 {
		t.Fatal("a failed walk released the read position of a transcript it never reached: the next append to it would be read from byte zero")
	}
	if !w.readFailed[path] {
		t.Fatal("a failed walk cleared the read-failure latch of a transcript it never reached")
	}
	if string(w.zstdCarry[path]) != `{"usage":` {
		t.Fatal("a failed walk released the record carry of a transcript it never reached: the next window would start mid-record")
	}
}

// A store holding two extensions is walked once for both: the listing is keyed
// on the whole suffix set, so a dsh watcher pays one traversal and one stat per
// file per rescan rather than one of each per extension. The cache holds one
// entry for the root, which is what a single walk leaves behind.
func TestOneWalkServesTheWholeSuffixSet(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for _, name := range []string{"session.jsonl", "session.jsonl.zstd", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	suffixes := []string{".jsonl.zstd", ".jsonl"}
	got, complete := listTranscripts(dir, suffixes, now.Add(-recencyWindow), now, true)
	if !complete {
		t.Fatal("a walk of a readable store reported itself incomplete")
	}
	if len(got) != 2 {
		t.Fatalf("walk returned %v, want both .jsonl.zstd and .jsonl matches", got)
	}
	for _, want := range []string{filepath.Join(dir, "session.jsonl"), filepath.Join(dir, "session.jsonl.zstd")} {
		if !slices.Contains(got, want) {
			t.Errorf("walk returned %v, missing %s", got, want)
		}
	}
	rootListMu.Lock()
	entries := 0
	for k := range rootLists {
		if strings.HasPrefix(k, dir+"\x00") {
			entries++
		}
	}
	rootListMu.Unlock()
	if entries != 1 {
		t.Fatalf("one walk left %d listings for the root, want 1", entries)
	}
}
