// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build sqlite

package agentusage

import (
	"fmt"
	"testing"
)

// resetStoreReadState empties the shared outage latch between tests, so one
// test's failures are not the next test's already-reported outage.
func resetStoreReadState(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		storeReadState.Lock()
		storeReadState.states = map[string]*storeRead{}
		storeReadState.order = nil
		storeReadState.Unlock()
	})
}

// A table full of stores that are all still failing has no quiet entry to
// reclaim, and the key the caller is inserting is the one whose latch must not
// be the one lost: a latch evicted out of the table under the caller reads as
// a new outage on the next poll, and the audit line naming this store's
// corrupt database is written once per poll for as long as it stays corrupt.
func TestMarkStoreFailedKeepsLatchInAFullTableOfFailures(t *testing.T) {
	resetStoreReadState(t)
	for i := range maxStoreReads {
		key := storeReadKey("agent", fmt.Sprintf("/store/%d.db", i))
		if first := markStoreFailed(key); !first {
			t.Fatalf("first failure of %s reported as already reported", key)
		}
	}
	key := storeReadKey("agent", "/store/new.db")
	if first := markStoreFailed(key); !first {
		t.Fatal("first failure of the newest store reported as already reported")
	}
	if first := markStoreFailed(key); first {
		t.Error("second failure of the newest store reported as a new outage: its latch was evicted from the table")
	}

	storeReadState.Lock()
	size, held := len(storeReadState.states), storeReadState.states[key] != nil
	storeReadState.Unlock()
	if !held {
		t.Error("the newest store has no latch in the table")
	}
	if size != maxStoreReads {
		t.Errorf("latch table holds %d keys, want the cap %d", size, maxStoreReads)
	}
}

// The cap still holds when every key in the table is failing: the sweep takes
// the oldest of the rest rather than growing past maxStoreReads.
func TestMarkStoreFailedCapsAFullTableOfFailures(t *testing.T) {
	resetStoreReadState(t)
	for i := range maxStoreReads * 2 {
		markStoreFailed(storeReadKey("agent", fmt.Sprintf("/store/%d.db", i)))
		storeReadState.Lock()
		size := len(storeReadState.states)
		storeReadState.Unlock()
		if size > maxStoreReads {
			t.Fatalf("latch table holds %d keys after %d failures, cap is %d", size, i+1, maxStoreReads)
		}
	}
}
