// Copyright (C) 2026 Marcel Wysocki
// SPDX-License-Identifier: MIT

package remote

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The state an operator's own restore leaves: a store put back by hand with a
// copy behind it and the marks of the write that lost it still beside it. Both
// are answered on the next connect, because a connect is the only thing that
// runs on a host whose store was repaired by hand.
func TestAConnectFinishesARestoreTheOperatorPerformed(t *testing.T) {
	path := useStore(t)
	pinned := pinLine("h:22", "pinned")
	if err := writeKnownHosts(path, map[string]string{"h:22": pinned}); err != nil {
		t.Fatal(err)
	}
	// The loss: the store is gone, and both kinds of mark a write that did not
	// finish leaves are beside it.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	stageInterrupted(t, path)
	if err := os.WriteFile(displacedPath(path), []byte(pinned+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The operator runs the command the warning printed.
	if err := os.WriteFile(path, []byte(pinned+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The copy is now older than the store it was copied into, which is the
	// state a plain cp always leaves.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(backupPath(path), old, old); err != nil {
		t.Fatal(err)
	}
	logs := captureAudit(t)

	if _, err := tofu(); err != nil {
		t.Fatalf("connect: %v", err)
	}

	if interruptedWrite(path) {
		t.Error("the marks a killed write left are still beside the restored store, so the next rm reads as a loss instead of a re-pin")
	}
	if why := staleStoreCopy(path); why != "" {
		t.Errorf("the copy beside the restored store %s, so every connect reports a gap the operator cannot close", why)
	}
	if got := linesWith(logs, "restored from a copy beside it"); len(got) != 1 {
		t.Errorf("audit lines for a restored store = %d, want 1: %v", len(got), got)
	}
	// The pins are still the pins: settling the restore copies the store, and
	// never reads the copy into it.
	store, err := readKnownHosts(path)
	if err != nil {
		t.Fatalf("read the restored store back: %v", err)
	}
	if len(store) != 1 || store["h:22"] != pinned {
		t.Errorf("store = %v, want the pin the operator restored", store)
	}
}

// The re-pin gesture is the reason the marks are spent. A store an operator
// deleted on purpose to accept a host's new key reads as empty, and a mark left
// beside it hands that rejected key back; the restore an operator performed has
// to leave the gesture working, because the rm that follows a restore is how
// the next key change is accepted.
func TestARestoreAnOperatorPerformedLeavesTheRepinGestureWorking(t *testing.T) {
	path := useStore(t)
	rejected := pinLine("h:22", "rejected")
	if err := writeKnownHosts(path, map[string]string{"h:22": rejected}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	stageInterrupted(t, path)
	// The operator puts the store back from the copy, as the warning says.
	if err := os.WriteFile(path, []byte(rejected+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	captureAudit(t)
	if _, err := tofu(); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// The re-pin gesture, run after the restore the operator performed.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	store, err := readKnownHosts(path)
	if err != nil {
		t.Fatalf("a store deleted after a restore must read as empty, not fail: %v", err)
	}
	if len(store) != 0 {
		t.Fatalf("store = %v, want empty: a mark the restore did not spend re-pinned the host the operator un-pinned", store)
	}
	restoreStore(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the rejected pin was written back to the store (stat err %v); the delete was a re-pin request, not a loss", err)
	}
}

// A healthy host takes neither step: there is nothing behind the store, no
// marks, and the store is there. A connect that cannot be repaired costs
// nothing, so it has to say nothing.
func TestAConnectWithNothingToSettleStaysQuiet(t *testing.T) {
	path := useStore(t)
	if err := writeKnownHosts(path, map[string]string{"h:22": pinLine("h:22", "pinned")}); err != nil {
		t.Fatal(err)
	}
	logs := captureAudit(t)

	if _, err := tofu(); err != nil {
		t.Fatalf("connect: %v", err)
	}

	for _, line := range linesWith(logs, "restored from a copy beside it") {
		t.Errorf("a healthy connect reported a restore that did not happen: %q", line)
	}
	if got := linesWith(logs, "restore not settled"); len(got) != 0 {
		t.Errorf("a healthy connect reported a failed settle: %v", got)
	}
}

// A store an operator is repairing by hand is the one toktop cannot refuse:
// it is the file a hand edit is aimed at, and refusing to read it because it
// was not written by writeKnownHosts would strand the repair. The settle step
// therefore reads it the way the rest of the package does and reports what it
// cannot do, rather than holding the store hostage.
func TestTheSettleStepLeavesAStoreItCannotCopyAlone(t *testing.T) {
	path := useStore(t)
	pinned := pinLine("h:22", "pinned")
	if err := writeKnownHosts(path, map[string]string{"h:22": pinned}); err != nil {
		t.Fatal(err)
	}
	stageInterrupted(t, path)
	// A hand edit that does not parse: readKnownHosts refuses the store, and
	// the copy is the file the operator puts it back from.
	if err := os.WriteFile(path, []byte("h:22 not-a-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := captureAudit(t)

	settleOperatorRestore(path)

	if got := linesWith(logs, "restore not settled"); len(got) != 1 {
		t.Errorf("audit lines for a store that could not be settled = %d, want 1: %v", len(got), got)
	}
	if got := linesWith(logs, "restored from a copy beside it"); len(got) != 0 {
		t.Errorf("a store that could not be settled reported itself restored: %v", got)
	}
	if !interruptedWrite(path) {
		t.Error("the marks were spent on a store that is still damaged, so the next read answers from the copy it was supposed to be repaired from")
	}
	// The copy is still the operator's way out.
	b, err := os.ReadFile(backupPath(path))
	if err != nil {
		t.Fatalf("the copy beside a damaged store is gone: %v", err)
	}
	if _, err := parseKnownHosts(backupPath(path), b); err != nil {
		t.Errorf("the copy beside a damaged store no longer parses: %v", err)
	}
}

// The copy a restore refreshes is written from the store, so a restore that
// copied the file the operator handed back into the store again cannot roll the
// store back to whatever the copy held.
func TestTheSettleStepCopiesTheStoreNotTheCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	store := pinLine("h:22", "in the store")
	if err := os.WriteFile(path, []byte(store+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A copy older and different from the store: it is what a lost store is
	// recovered from, and it is not what the store says now.
	if err := os.WriteFile(backupPath(path), []byte(pinLine("h:22", "older")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Aged so the copy reads as behind the store, which is the state a plain
	// cp always leaves and the one settleOperatorRestore exists to close.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(backupPath(path), old, old); err != nil {
		t.Fatal(err)
	}
	captureAudit(t)

	settleOperatorRestore(path)

	got, err := readKnownHosts(backupPath(path))
	if err != nil {
		t.Fatalf("read the refreshed copy back: %v", err)
	}
	if got["h:22"] != store {
		t.Errorf("the copy = %v, want the store's own record; the restore rolled the copy forward from itself", got)
	}
}
