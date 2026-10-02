// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package remote

import (
	"os"
	"testing"
	"time"
)

// CheckStore has to reach the state a connect used to be the only thing that
// reached. Before it existed, the store's integrity pass ran inside tofu(), so
// a run that never dialed an ssh host -- a run that only attached to http://
// endpoints, ran --demo, or did a local --once -- never restored a store an
// interrupted write had left missing, never rebuilt a copy that could not
// recover one, and never settled a restore the operator had done by hand. The
// store is the only state toktop writes and its copy the only backup of it, so
// the run that found the backup unusable was not the run that should have
// reported it: that was whichever connect happened to come next, which is
// never in a deployment that only watches local engines.
func TestCheckStoreSettlesAStoreNoConnectWouldHaveReached(t *testing.T) {
	path := useStore(t)
	pinned := pinLine("h:22", "pinned")
	if err := writeKnownHosts(path, map[string]string{"h:22": pinned}); err != nil {
		t.Fatal(err)
	}
	// The loss an interrupted write leaves: the store is gone, a displaced
	// copy behind it holds the pins, and the marks of the write that failed
	// are beside them. Nothing else reads this before a connect.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	stageInterrupted(t, path)
	if err := os.WriteFile(displacedPath(path), []byte(pinned+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A plain cp of the store back, which is what the operator does: the copy
	// is now older than the store, so every later run reports a gap.
	if err := os.WriteFile(path, []byte(pinned+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(backupPath(path), old, old); err != nil {
		t.Fatal(err)
	}
	logs := captureAudit(t)

	if !CheckStore() {
		t.Fatal("CheckStore reported no store where useStore named one")
	}

	if interruptedWrite(path) {
		t.Error("the marks a killed write left are still beside the store, so the next rm reads as a loss instead of a re-pin")
	}
	if why := staleStoreCopy(path); why != "" {
		t.Errorf("the copy beside the store %s, so every run reports a gap the operator cannot close", why)
	}
	store, err := readKnownHosts(path)
	if err != nil {
		t.Fatalf("the store is no longer readable after the pass: %v", err)
	}
	if got := store["h:22"]; got != pinned {
		t.Errorf("pin for h:22 = %q, want %q; settling the pass copied the store and must never read the copy into it", got, pinned)
	}
	if got := linesWith(logs, "backup rewritten from the store"); len(got) != 1 {
		t.Errorf("audit lines for the rebuilt copy = %d, want 1: %v", len(got), got)
	}
}

// A store copy that cannot recover a loss is the one thing the operator has to
// be told about, and the check is what finds it. The pass runs on every run
// now, so this is reported on the next run rather than on the next connect
// that happened to follow a write whose backup did not land.
func TestCheckStoreRebuildsACopyThatCannotRecoverTheStore(t *testing.T) {
	path := useStore(t)
	pinned := pinLine("h:22", "pinned")
	if err := writeKnownHosts(path, map[string]string{"h:22": pinned}); err != nil {
		t.Fatal(err)
	}
	// A backup that is not the store: the shape a truncated write leaves, and
	// the one an operator's hand-edited copy leaves.
	if err := os.WriteFile(backupPath(path), []byte(pinned[:len(pinned)/2]), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := captureAudit(t)

	CheckStore()

	bak, err := readKnownHosts(backupPath(path))
	if err != nil {
		t.Fatalf("the copy is unreadable, so the store can never be recovered from it: %v", err)
	}
	if got := bak["h:22"]; got != pinned {
		t.Errorf("pin in the copy = %q, want %q", got, pinned)
	}
	if got := linesWith(logs, "backup rewritten from the store"); len(got) != 1 {
		t.Errorf("audit lines for the rebuilt copy = %d, want 1: %v", len(got), got)
	}
}

// Where the environment names no store, the pass has nothing to settle and
// says so by returning false, which is the caller's cue to print the line that
// names the consequence (warnNoHostKeyStore). It must not panic on the empty
// path, and it must not invent a store to create.
func TestCheckStoreReportsAnEnvironmentWithNoStore(t *testing.T) {
	old := knownHostsPath
	knownHostsPath = func() string { return "" }
	t.Cleanup(func() { knownHostsPath = old })

	if CheckStore() {
		t.Error("CheckStore reported a store where the environment names none")
	}
}

// A healthy store is the common case and has to stay silent: a run that
// reported something every time would train the operator to skip the lines.
func TestCheckStoreSaysNothingAboutAHealthyStore(t *testing.T) {
	path := useStore(t)
	if err := writeKnownHosts(path, map[string]string{"h:22": pinLine("h:22", "pinned")}); err != nil {
		t.Fatal(err)
	}
	logs := captureAudit(t)

	CheckStore()

	if got := logs.String(); got != "" {
		t.Errorf("a healthy store produced audit output, so every run would have something new to ignore: %s", got)
	}
}
