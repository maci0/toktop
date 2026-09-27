// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentwatch

import (
	"sync"
	"testing"
	"time"

	"github.com/maci0/toktop/agentusage"
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
