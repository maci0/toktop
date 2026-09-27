// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"os"
	"path/filepath"
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
