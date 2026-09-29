// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build sqlite

package agentusage

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// resetOpenStores empties the shared handle table between tests, closing every
// handle in it: a test that leaves one open holds a file descriptor and a lock
// on a database a later test writes to. It empties the table now and again at
// the end of the test, because the table is process-global and a watcher test
// that ran earlier leaves its polled store cached for every test that follows.
func resetOpenStores(t *testing.T) {
	t.Helper()
	emptyOpenStores()
	t.Cleanup(emptyOpenStores)
}

func emptyOpenStores() {
	openStores.Lock()
	defer openStores.Unlock()
	for path, h := range openStores.byPath {
		_ = h.db.Close()
		delete(openStores.byPath, path)
	}
	openStores.order = nil
}

// The whole point of the table: a second read of a store the agent has not
// replaced goes through the handle already open, so the poll does not pay to
// rebuild the connection.
func TestOpenStoreReusesTheHandleOnTheSameFile(t *testing.T) {
	dir := t.TempDir()
	crushDB(t, dir, map[string][3]int64{"s1": {10, 20, 1789581724}})
	resetOpenStores(t)
	path := filepath.Join(dir, ".crush", "crush.db")

	first, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("a second open of an unchanged store made a new handle; every poll pays to rebuild the connection")
	}
}

// A store the agent replaces is a different file under the same path. The old
// handle would keep reading the unlinked inode, so the dashboard would report
// the deleted store's numbers for as long as the process ran.
func TestOpenStoreReplacesTheHandleWhenTheFileIsReplaced(t *testing.T) {
	dir := t.TempDir()
	crushDB(t, dir, map[string][3]int64{"s1": {10, 20, 1789581724}})
	resetOpenStores(t)
	path := filepath.Join(dir, ".crush", "crush.db")

	first, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// A new file at the same path, as an agent that recreates its store does.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	crushDB(t, dir, map[string][3]int64{"s2": {30, 40, 1789581724}})

	second, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("a replaced store kept its handle; the read would come from the unlinked inode")
	}
	sess, ok := readCrushSessions(path, time.Time{})
	if !ok {
		t.Fatal("read of the replaced store failed")
	}
	if _, stale := sess["s1"]; stale {
		t.Error("the read came from the replaced file, which still holds s1")
	}
	if _, fresh := sess["s2"]; !fresh {
		t.Error("the read did not come from the file now at the path")
	}
}

// A read that fails drops its handle, so a corrupt page or a database left
// mid-recovery costs one poll instead of every poll after it.
func TestCloseStoreDropsTheHandle(t *testing.T) {
	dir := t.TempDir()
	crushDB(t, dir, map[string][3]int64{"s1": {10, 20, 1789581724}})
	resetOpenStores(t)
	path := filepath.Join(dir, ".crush", "crush.db")

	first, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	closeStore(path)
	second, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("a dropped handle was handed back; a read that failed would be repeated against it forever")
	}
}

// A drop must not close a handle out from under a read that is still on it.
// The handle is shared, so a peer reader whose read failed calls closeStore
// while this one is between openStore and its query; the query then fails with
// "database is closed" for a reason the store never had, and the agent is
// audited as an unreadable store it read fine a moment earlier.
func TestCloseStoreLeavesAHeldHandleUsable(t *testing.T) {
	dir := t.TempDir()
	crushDB(t, dir, map[string][3]int64{"s1": {10, 20, 1789581724}})
	resetOpenStores(t)
	path := filepath.Join(dir, ".crush", "crush.db")

	h, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// A peer read fails and drops the handle while this one still holds it.
	closeStore(path)
	if err := h.db.QueryRowContext(context.Background(), "select 1").Scan(new(int)); err != nil {
		t.Fatalf("the handle was closed under the read holding it: %v", err)
	}
	// The last read to let go is the one that closes it, or the handle would
	// outlive the drop that retired it.
	releaseStore(h)
	if err := h.db.QueryRowContext(context.Background(), "select 1").Scan(new(int)); err == nil {
		t.Error("a dropped handle stayed open after the last read released it; every poll after would leak a connection")
	}
}

// A store that is not there, and a file that is not a database, both open
// lazily and fail on the first query. Handing such a handle out caches a
// connection nothing can read, and the stat it carries is nil, so every later
// poll drops it and builds another: an agent with no store at all pays a failed
// open on every poll for the life of the dashboard.
func TestOpenStoreRefusesAStoreItCannotRead(t *testing.T) {
	dir := t.TempDir()
	resetOpenStores(t)

	// The table is process-global and a watcher test's last poll can still be
	// caching its store, so this asserts on what these two opens added rather
	// than on the whole table: what must not be cached is an unreadable one.
	before := openStorePaths()

	missing := filepath.Join(dir, "missing.db")
	if _, err := openStore(missing); err == nil {
		t.Error("a path with no database opened a handle; every poll then pays for a read that cannot run")
	}
	garbage := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(garbage, []byte("not a sqlite database"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(garbage); err == nil {
		t.Error("a file that is not a database opened a handle")
	}
	for _, path := range openStorePaths() {
		if !slices.Contains(before, path) {
			t.Errorf("the handle table cached %q, an unreadable store; each one is rebuilt on the next poll", path)
		}
	}
}

// openStorePaths lists the paths the shared handle table holds.
func openStorePaths() []string {
	openStores.Lock()
	defer openStores.Unlock()
	return slices.Sorted(maps.Keys(openStores.byPath))
}

// A store that recovers is opened by the next read, which is the point of
// reporting an unreadable one instead of caching it: the failure costs the
// poll it happened on, and nothing after it.
func TestOpenStoreOpensAfterAStoreRecovers(t *testing.T) {
	dir := t.TempDir()
	resetOpenStores(t)
	// A file the open refuses, replaced by the store the agent writes.
	path := filepath.Join(dir, ".crush", "crush.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a sqlite database"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(path); err == nil {
		t.Fatal("a file that is not a database opened a handle")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	crushDB(t, dir, map[string][3]int64{"s1": {10, 20, 1789581724}})
	sess, ok := readCrushSessions(path, time.Time{})
	if !ok {
		t.Fatal("the read failed after the store was replaced by a database")
	}
	if _, found := sess["s1"]; !found {
		t.Errorf("read %+v from the recovered store, want s1", sess)
	}
}

// A project-local store is found by walking up from the agent's working
// directory, so the key set is every project an agent has run in. Without a cap
// the table would pin a handle for each one for the life of the dashboard.
func TestOpenStoreEvictsPastTheCap(t *testing.T) {
	resetOpenStores(t)
	paths := make([]string, 0, maxOpenStores+4)
	for range maxOpenStores + 4 {
		// A store per project directory: the key set is every project an agent
		// has run in, walked up from its working directory.
		dir := t.TempDir()
		crushDB(t, dir, map[string][3]int64{"s1": {10, 20, 1789581724}})
		p := filepath.Join(dir, ".crush", "crush.db")
		if _, err := openStore(p); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	openStores.Lock()
	n := len(openStores.byPath)
	openStores.Unlock()
	if n > maxOpenStores {
		t.Errorf("the handle table holds %d stores, cap %d; each one is an open database for the life of the process", n, maxOpenStores)
	}
	// The oldest were evicted, so its next open builds a handle and inserts it.
	if _, err := openStore(paths[0]); err != nil {
		t.Fatal(err)
	}
	openStores.Lock()
	_, cached := openStores.byPath[paths[0]]
	openStores.Unlock()
	if !cached {
		t.Error("the evicted store was not re-inserted on its next open")
	}
}
