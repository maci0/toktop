// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentwatch

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/maci0/toktop/agentusage"
	"github.com/maci0/toktop/internal/core"
)

func trackerFor(t *testing.T, w *Watcher, pid int) *tracked {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tracked[pid]
}

// A store is followed once, and the process that claimed it is the one
// tailing its transcripts. When that process exits, the remaining processes
// working against the same store must take the store over: a live agent left
// unwatched reports no tokens at all.
func TestFollowerTakesStoreWhenClaimantExits(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	work := t.TempDir()
	first := agentusage.Process{PID: 101, Tool: "claude", Dir: work, Started: time.Unix(100, 0)}
	second := agentusage.Process{PID: 102, Tool: "claude", Dir: work, Started: time.Unix(200, 0)}

	var mu sync.Mutex
	live := []agentusage.Process{first, second}
	w := New(&recorder{}, nil)
	w.readEvery = time.Hour
	w.listAgents = func() []agentusage.Process {
		mu.Lock()
		defer mu.Unlock()
		return append([]agentusage.Process(nil), live...)
	}
	ctx := t.Context()
	defer w.stopAll()

	w.discover(ctx)
	if tr := trackerFor(t, w, first.PID); tr == nil || tr.watch == nil {
		t.Fatal("the lowest PID did not take the store")
	}
	if tr := trackerFor(t, w, second.PID); tr == nil || tr.watch != nil {
		t.Fatal("the second process on the same store should not have a watcher of its own")
	}

	mu.Lock()
	live = []agentusage.Process{second}
	mu.Unlock()
	w.discover(ctx)

	if tr := trackerFor(t, w, second.PID); tr == nil || tr.watch == nil {
		t.Fatal("the surviving process never took the store over; its tokens go unreported")
	}
}

// blockingRecorder holds the first event it is handed until released, so a
// test can stop a discover pass partway through and inspect what has and has
// not happened by then.
type blockingRecorder struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	inner   recorder
}

func (b *blockingRecorder) RecordAgent(ev core.AgentEvent) bool {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
	return b.inner.RecordAgent(ev)
}

// The handover must not overlap two watchers on one store. The dead tracker's
// poll loop is still running when its process is gone, so promoting the
// survivor before stopping it leaves both tailing the same transcripts: each
// reports the same growth under its own PID, the collector's id window cannot
// merge two different ids, and the tokens in the overlap count twice.
//
// discover stops the exited tracker before installing any watcher, so the
// survivor's baseline is taken only after the dead watcher's final growth has
// been reported. The test holds that report open and asserts nothing has been
// promoted onto the store while it is pending.
func TestHandoverWaitsForTheExitedWatcherToStop(t *testing.T) {
	work, transcript := claudeHome(t)
	first := agentusage.Process{PID: 201, Tool: "claude", Dir: work, Started: time.Unix(100, 0)}
	second := agentusage.Process{PID: 202, Tool: "claude", Dir: work, Started: time.Unix(200, 0)}

	var mu sync.Mutex
	live := []agentusage.Process{first, second}
	rec := &blockingRecorder{entered: make(chan struct{}), release: make(chan struct{})}
	w := New(rec, nil)
	w.readEvery = time.Hour
	w.listAgents = func() []agentusage.Process {
		mu.Lock()
		defer mu.Unlock()
		return append([]agentusage.Process(nil), live...)
	}
	ctx := t.Context()
	defer w.stopAll()

	w.discover(ctx)
	if tr := trackerFor(t, w, first.PID); tr == nil || tr.watch == nil {
		t.Fatal("the lowest PID did not take the store")
	}
	if tr := trackerFor(t, w, second.PID); tr == nil || tr.watch != nil {
		t.Fatal("the second process on the same store should not have a watcher of its own")
	}

	// Growth the exiting watcher still has to report, so stopping it produces
	// an event the recorder can hold open.
	appendLine(t, filepath.Join(transcript, "s.jsonl"), usageLine(work, 400))
	appendLine(t, filepath.Join(transcript, "s.jsonl"), usageLine(work, 900))

	mu.Lock()
	live = []agentusage.Process{second}
	mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.discover(ctx)
	}()

	select {
	case <-rec.entered:
	case <-time.After(waitCeiling):
		t.Fatal("stopping the exited watcher reported nothing to hold open")
	}
	if w.watching(second.PID) {
		t.Fatal("a follower was promoted onto a store the exited watcher is still tailing; growth in the overlap is counted twice")
	}
	close(rec.release)
	<-done

	if tr := trackerFor(t, w, second.PID); tr == nil || tr.watch == nil {
		t.Fatal("the surviving process never took the store over; its tokens go unreported")
	}
}
