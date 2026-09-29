// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// pinLine renders a stored record for a host, the shape writeKnownHosts writes.
func pinLine(host, label string) string {
	return host + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(fakePublicKey(label))))
}

// useStore points the store path at a fresh directory for one test.
func useStore(t *testing.T) string {
	t.Helper()
	old := knownHostsPath
	path := filepath.Join(t.TempDir(), "known_hosts")
	knownHostsPath = func() string { return path }
	t.Cleanup(func() { knownHostsPath = old })
	return path
}

// A displaced copy whose replacement landed holds the pins from before a write
// the store already carries, so beside a store that is there it recovers
// nothing. Left behind, its name still answers interruptedWrite, and the store
// the operator later deletes on purpose reads as a write that died rather than
// as the re-pin it is.
func TestConnectRemovesACopyTheStoreHasSuperseded(t *testing.T) {
	path := useStore(t)
	store := map[string]string{"h:22": pinLine("h:22", "pinned")}
	if err := writeKnownHosts(path, store); err != nil {
		t.Fatal(err)
	}
	// What replaceFile leaves when the replacement landed and the leftover
	// could not be deleted: the content from before that write.
	if err := os.WriteFile(displacedPath(path), []byte(pinLine("h:22", "pinned")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	captureAudit(t)

	if _, err := tofu(); err != nil {
		t.Fatalf("connect: %v", err)
	}

	if _, err := os.Stat(displacedPath(path)); !os.IsNotExist(err) {
		t.Errorf("the displaced copy is still beside the store (stat err %v), so it keeps reading as a write that did not finish", err)
	}
	got, err := readKnownHosts(path)
	if err != nil {
		t.Fatalf("read the store back: %v", err)
	}
	if got["h:22"] != store["h:22"] {
		t.Errorf("store = %v, want the pin it held before the copy was cleared", got)
	}
	if interruptedWrite(path) {
		t.Error("a store left with a mark beside it is one deletion away from a re-pin that cannot happen")
	}
}

// The copy is the store's only record while the store is missing, and a
// connect that has a store to put back is a connect that spends it. Clearing a
// superseded copy must not take the fallback the restore is about to read.
func TestConnectKeepsACopyThatStillHoldsTheOnlyRecord(t *testing.T) {
	path := useStore(t)
	line := pinLine("h:22", "pinned")
	if err := writeKnownHosts(path, map[string]string{"h:22": line}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(backupPath(path)); err != nil {
		t.Fatal(err)
	}
	// The store is gone and nothing else holds it, so the displaced copy is the
	// evidence an interrupted write left rather than a superseded leftover.
	if err := os.WriteFile(displacedPath(path), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	captureAudit(t)

	if _, err := tofu(); err != nil {
		t.Fatalf("connect: %v", err)
	}

	got, err := readKnownHosts(path)
	if err != nil {
		t.Fatalf("the store a killed write left under its copy was not recovered: %v", err)
	}
	if got["h:22"] != line {
		t.Errorf("store = %v, want the pins the displaced copy held", got)
	}
}

// The re-pin gesture, run after a connect that cleared a superseded copy. The
// operator deletes the store to accept a host's new key, and the next connect
// has to treat that as what it is.
func TestDeletingTheStoreAfterAClearedCopyStillRepins(t *testing.T) {
	path := useStore(t)
	rejected := pinLine("h:22", "rejected")
	if err := writeKnownHosts(path, map[string]string{"h:22": rejected}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(displacedPath(path), []byte(rejected+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	captureAudit(t)
	if _, err := tofu(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	store, err := readKnownHosts(path)
	if err != nil {
		t.Fatalf("a store deleted on purpose must read as empty, not fail: %v", err)
	}
	if len(store) != 0 {
		t.Fatalf("store = %v, want empty: a copy the store had already superseded re-pinned the key the operator rejected", store)
	}
	restoreStore(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the rejected pin was written back to the store (stat err %v); the delete was a re-pin request, not a loss", err)
	}
}
