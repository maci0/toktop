// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// A verdict of false is released by the age-out sweep the moment its file
// leaves the walk, and the sweep cannot run on a walk that missed part of the
// store. So the foreign half of the owner map, with the stamps and skip
// positions behind it, was bounded by nothing at all while a store kept
// failing to walk. The stores behind the per-file-owner adapters are per-user,
// so every session another project on the host started is a key here.
func TestIncompleteWalkCapsForeignVerdicts(t *testing.T) {
	store := withStore(t, "copilot")
	other := t.TempDir() // a different checkout: every session in it is foreign
	w := Watch("copilot", t.TempDir(), time.Now())
	if w == nil {
		t.Fatal("no copilot adapter")
	}

	for i := range foreignCap + 8 {
		copilotSession(t, store, "foreign"+strconv.Itoa(i), other)
	}
	w.Poll()
	n := foreignCap + 8
	if got := len(w.owner); got != n {
		t.Fatalf("%d verdicts recorded, want %d", got, n)
	}
	for _, mine := range w.owner {
		if mine {
			t.Fatalf("a session in %s was judged ours", other)
		}
	}

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
	// The last complete listing is the store's contents, all of them, so it
	// would spare every verdict below. A store that has been failing to walk
	// since has one that is short or gone, which is the case the cap exists
	// for.
	w.cached = nil

	w.walkCandidates(w.instant().Add(-recencyWindow), true)

	if got := len(w.owner); got > foreignCap {
		t.Fatalf("a walk that missed part of the store left %d foreign verdicts, over the %d cap", got, foreignCap)
	}
	if got := len(w.offsets); got > foreignCap {
		t.Fatalf("a walk that missed part of the store left %d skip positions, over the %d cap", got, foreignCap)
	}
	if got := len(w.stamps); got > foreignCap {
		t.Fatalf("a walk that missed part of the store left %d stamps, over the %d cap", got, foreignCap)
	}
}

// A cap on the foreign verdicts is a cap on the map, not a change of verdict:
// a session released by it is judged again on its next sighting, and since a
// foreign path short-circuits on its size alone, re-judging reads the header
// and re-offers nothing. What it must not do is release a verdict for a
// transcript the last complete listing still held.
func TestIncompleteWalkSpareIsAVerdictTheLastListingHeld(t *testing.T) {
	store := withStore(t, "copilot")
	other := t.TempDir()
	w := Watch("copilot", t.TempDir(), time.Now())

	live := copilotSession(t, store, "live", other)
	for i := range foreignCap + 8 {
		copilotSession(t, store, "foreign"+strconv.Itoa(i), other)
	}
	w.Poll()
	if _, ok := w.owner[live]; !ok {
		t.Fatal("the live session was not judged at all")
	}
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

	if mine, ok := w.owner[live]; !ok {
		t.Error("the walk released the verdict of a transcript the last complete listing still held")
	} else if mine {
		t.Error("the verdict flipped to ours")
	}
}

// A walk that could not read part of the store has a list, not a whole one,
// and the caller caches nothing on a list it cannot vouch for. A file that
// merely vanished between the walk and the stat is the ordinary race of a
// session rotating, and is not one.
func TestWalkTranscriptsReportsAnUnreadableSubtree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which reads a 0-mode directory")
	}
	root := t.TempDir()
	for _, name := range []string{"a.jsonl", "sub/b.jsonl"} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sub := filepath.Join(root, "sub")
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	if err := os.Chmod(sub, 0); err != nil {
		t.Skipf("cannot drop read permission on a directory: %v", err)
	}
	if _, err := walkTranscripts(root, []string{".jsonl"}, time.Now().Add(-time.Hour)); err == nil {
		t.Fatal("an unreadable subtree reported as a complete walk")
	}
}

func TestWalkTranscriptsIgnoresAVanishedFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := walkTranscripts(root, []string{".jsonl"}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("a plain walk failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("listed %d transcripts, want 1", len(got))
	}
}
