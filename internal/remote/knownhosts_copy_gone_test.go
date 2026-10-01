// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package remote

import (
	"os"
	"strings"
	"testing"
)

// A store deleted without the copy beside it is the state where the copy holds
// the only record of every pin on the host. Nothing may rewrite it: an empty
// store written over it reads as holding no pins, and the next connect trusts
// whatever key the host presents, which is the interception the copy exists to
// refuse. The warning restoreStore leaves is the whole report this state gets.
//
// The copy is the last record because the store's own copy travels with the
// directory a restore of the directory brings back, and a store removed on its
// own leaves it behind. That is the deletion this pins: a cleanup that removed
// one file, not one that removed all three.
func TestACopyTheStoreIsGoneWithIsNotRewrittenOver(t *testing.T) {
	path := useStore(t)
	line := pinLine("h:22", "pinned")
	if err := writeKnownHosts(path, map[string]string{"h:22": line}); err != nil {
		t.Fatal(err)
	}
	// The store removed on its own: no interrupted-write mark beside it, which
	// is what tells a re-pin from a write that died.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	log := captureAudit(t)

	checkStoreCopy(path)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a store was written where there was none (stat err %v): %s", err, log.String())
	}
	b, err := os.ReadFile(backupPath(path))
	if err != nil {
		t.Fatalf("the only record of the pins is gone: %v", err)
	}
	copied, err := parseKnownHosts(backupPath(path), b)
	if err != nil {
		t.Fatalf("the copy no longer parses: %v", err)
	}
	if copied["h:22"] != line {
		t.Errorf("the copy holds %v, want the pins the store was deleted with", copied)
	}
}

// The whole connect, not the step on its own: a run whose store was removed on
// its own recovers nothing (restoreStore leaves a re-pin alone) and must leave
// the copy holding every pin, so the operator's `cp` from the warning still has
// something to put back.
func TestAConnectAfterTheStoreWasRemovedKeepsTheCopy(t *testing.T) {
	path := useStore(t)
	line := pinLine("h:22", "pinned")
	if err := writeKnownHosts(path, map[string]string{"h:22": line}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	log := captureAudit(t)

	if _, err := tofu(); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(backupPath(path))
	if err != nil {
		t.Fatalf("the copy the warning tells the operator to restore from is gone: %v", err)
	}
	copied, err := parseKnownHosts(backupPath(path), b)
	if err != nil {
		t.Fatalf("the copy no longer parses: %v", err)
	}
	if copied["h:22"] != line {
		t.Errorf("the copy holds %v, want the pins the removed store held", copied)
	}
	if !strings.Contains(log.String(), "every pin it held is dropped") {
		t.Errorf("a store removed with its copy intact was not reported: %s", log.String())
	}
}
