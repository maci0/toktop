// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"fmt"
	"testing"
	"time"
)

// The ledger is the dedup half of an agent POST: an id it has forgotten is
// stored a second time, and the feed counts a replayed turn twice. It is
// keyed on the instant an id was recorded, never on the event's own stamp,
// so a forged or stale timestamp cannot decide how long its own duplicate
// stays suppressed.
func TestAgentIDLedgerSeesWhatItStored(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	var l AgentIDLedger

	if l.Seen("a", base) {
		t.Fatal("an empty ledger reported a duplicate")
	}
	if l.Len() != 0 || l.Tracked() != 0 {
		t.Fatalf("empty ledger holds %d ids in %d slots", l.Len(), l.Tracked())
	}

	l.Add("a", base)
	if !l.Seen("a", base.Add(AgentIDHorizon-time.Second)) {
		t.Fatal("an id recorded a second ago is not a duplicate")
	}
	if !l.Held("a") {
		t.Fatal("Held disagrees with Seen for an id inside the horizon")
	}
	if l.Len() != 1 || l.Tracked() != 1 {
		t.Fatalf("ledger holds %d ids in %d slots, want 1 and 1", l.Len(), l.Tracked())
	}

	// The horizon is on the recording instant, so the id is reusable again
	// past it, and the ledger does not keep growing on a replay.
	if l.Seen("a", base.Add(AgentIDHorizon+time.Second)) {
		t.Fatal("an id older than the horizon is still a duplicate")
	}
	if l.Len() != 0 || l.Tracked() != 0 {
		t.Fatalf("the window moved past it but the ledger holds %d ids in %d slots", l.Len(), l.Tracked())
	}
	if l.Held("a") {
		t.Fatal("Held reports an id Seen has already retired")
	}

	l.Add("a", base.Add(AgentIDHorizon+time.Minute))
	if !l.Seen("a", base.Add(AgentIDHorizon+2*time.Minute)) {
		t.Fatal("a reused id is not remembered")
	}
	if l.Len() != 1 || l.Tracked() != 1 {
		t.Fatalf("reuse left %d ids in %d slots, want 1 and 1", l.Len(), l.Tracked())
	}
}

// The count cap bounds the ledger for a fleet that emits faster than the
// horizon can retire: the oldest ids go, the newest stay, and the order
// never holds more than the cap.
func TestAgentIDLedgerCountCapEvictsOldest(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	var l AgentIDLedger
	id := func(i int) string { return fmt.Sprintf("e%d", i) }
	now := base
	for i := range AgentIDLedgerMax + 8 {
		now = base.Add(time.Duration(i) * time.Millisecond)
		l.Add(id(i), now)
	}
	if l.Len() > AgentIDLedgerMax || l.Tracked() > AgentIDLedgerMax {
		t.Fatalf("ledger holds %d ids in %d slots, want at most %d",
			l.Len(), l.Tracked(), AgentIDLedgerMax)
	}
	// Every Add sweeps, so a superseded record is retired the moment the
	// newer one lands: the order never carries a second entry for an id.
	if l.Tracked() != l.Len() {
		t.Fatalf("eviction order carries %d slots for %d ids, want one each",
			l.Tracked(), l.Len())
	}
	if l.Held(id(0)) {
		t.Fatal("the oldest id outlived the count cap")
	}
	if !l.Held(id(AgentIDLedgerMax + 7)) {
		t.Fatalf("the newest id %q was evicted", id(AgentIDLedgerMax+7))
	}
}
