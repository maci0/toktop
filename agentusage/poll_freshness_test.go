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
	_ = listTranscripts(dir, ".jsonl", time.Now().Add(-recencyWindow), false)

	rootListMu.Lock()
	_, still := rootLists["stale\x00.jsonl"]
	rootListMu.Unlock()
	if still {
		t.Fatal("expired listing still in the cache")
	}
}

func TestRootListCacheDropsExpiredKeysOnHit(t *testing.T) {
	dir := t.TempDir()
	rootListMu.Lock()
	rootLists = map[string]rootListing{
		"stale\x00.jsonl":          {files: []string{"gone"}, at: time.Now().Add(-rescanEvery - time.Second)},
		rootListKey(dir, ".jsonl"): {files: []string{"fresh.jsonl"}, at: time.Now()},
	}
	rootListMu.Unlock()
	t.Cleanup(func() {
		rootListMu.Lock()
		rootLists = map[string]rootListing{}
		rootListMu.Unlock()
	})

	got := listTranscripts(dir, ".jsonl", time.Now().Add(-recencyWindow), false)
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
