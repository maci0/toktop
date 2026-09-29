// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// A walk that missed part of the store cannot age anything out, since a file
// missing from it may still be on disk. It must still apply the caps, though:
// the caps are the only bound on the per-file bookkeeping, and a store that
// keeps failing to walk while its other root keeps starting sessions would
// otherwise grow every map keyed by path for the life of the run.
func TestIncompleteWalkStillCapsCountedBookkeeping(t *testing.T) {
	store := withStore(t, "dsh")
	work := t.TempDir()
	w := Watch("dsh", work, time.Now())

	// Counted transcripts the walk below does not reach. Each is a real file,
	// so a release cuts it loose at its current end rather than dropping it.
	for i := range countedCap + 8 {
		path := filepath.Join(store, fmt.Sprintf("aged-%04d.jsonl", i))
		append_(t, path, `{"type":"session"}`)
		w.seen[path] = values{total: 1}
		w.total[path] = 1
		w.stamps[path] = fileStamp{mtimeNanos: int64(i + 1)}
	}
	// A transcript the last complete walk saw. Losing its counts here would
	// read the rest of that session from byte zero and bill it twice, so it
	// has to survive a walk that never saw it.
	live := filepath.Join(store, "live.jsonl")
	append_(t, live, `{"type":"session"}`)
	w.cached = []string{live}
	w.seen[live] = values{total: 1}
	w.total[live] = 1
	w.stamps[live] = fileStamp{mtimeNanos: 1 << 40}

	// A walk already claimed for this root that never lands, so this one gives
	// up waiting and reports the store as incomplete.
	key := rootListKey(store, w.ad.fileSuffixes())
	stalled := make(chan struct{})
	rootListMu.Lock()
	rootLists[key] = rootListing{at: time.Now(), walk: stalled}
	rootListMu.Unlock()
	t.Cleanup(func() {
		rootListMu.Lock()
		delete(rootLists, key)
		rootListMu.Unlock()
	})
	w.scanned = time.Time{}

	w.walkCandidates(w.instant().Add(-recencyWindow), true)

	if got := len(w.seen); got > countedCap {
		t.Fatalf("a walk that missed part of the store left %d counted transcripts, over the %d cap", got, countedCap)
	}
	if _, ok := w.seen[live]; !ok {
		t.Error("the walk released the counts of a transcript the last complete listing still held")
	}
}

// The caps run on an incomplete walk precisely because the age-out sweep does
// not: a latch names a file that never opened or never resolved, so it sits in
// none of the maps the sweep keys on, and a store that keeps failing to walk
// would leave one per file it ever failed on.
func TestIncompleteWalkCapsFailureLatches(t *testing.T) {
	store := withStore(t, "dsh")
	w := Watch("dsh", t.TempDir(), time.Now())

	for i := range latchCap + 8 {
		path := filepath.Join(store, fmt.Sprintf("stale-%04d.jsonl", i))
		w.readFailed[path] = true
		w.stamps[path] = fileStamp{mtimeNanos: int64(i + 1)}
	}
	// A file the last complete listing still held, latched on both counts.
	live := filepath.Join(store, "live.jsonl")
	w.readFailed[live] = true
	w.ownsFailed[live] = true
	w.cached = []string{live}

	key := rootListKey(store, w.ad.fileSuffixes())
	stalled := make(chan struct{})
	rootListMu.Lock()
	rootLists[key] = rootListing{at: time.Now(), walk: stalled}
	rootListMu.Unlock()
	t.Cleanup(func() {
		rootListMu.Lock()
		delete(rootLists, key)
		rootListMu.Unlock()
	})
	w.scanned = time.Time{}

	w.walkCandidates(w.instant().Add(-recencyWindow), true)

	if got := len(w.readFailed) + len(w.ownsFailed) - 2; got > latchCap {
		t.Fatalf("a walk that missed part of the store left %d failure latches, over the %d cap", got, latchCap)
	}
	if !w.readFailed[live] || !w.ownsFailed[live] {
		t.Error("the walk cleared the latches of a transcript the last complete listing still held")
	}
}
