// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build sqlite

package agentusage

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/core"

	_ "modernc.org/sqlite" // pure Go driver: no cgo, so cross-compilation still works
)

// registerSource makes an agent readable through a source instead of files.
// A zero tokenSource removes it, which is how the runtime switch turns one off.
func registerSource(tool string, s tokenSource) {
	sourcesMu.Lock()
	defer sourcesMu.Unlock()
	if !s.present() {
		delete(sources, tool)
		return
	}
	sources[tool] = s
}

const (
	// dbQueryTimeout bounds a single read against an agent database. The
	// dashboard polls several times a second; a hung or recovering store
	// must not freeze it. A timed-out read keeps the last sample.
	dbQueryTimeout = time.Second
	// dbBusyTimeout is how long a reader waits for a writer. Failing the
	// read keeps the last sample rather than inventing a zero.
	dbBusyTimeout = 250 * time.Millisecond
)

// readOnlyDSN builds the read-only URI DSN for a session database. SQLite
// parses file: URIs itself, so the three characters that carry URI syntax must
// travel percent-encoded inside the path, as its own documentation requires;
// anything else, Windows drive letters and separators included, passes through
// untouched. A raw % would fail the parse outright, and ? or # would end the
// filename early, either way reading nothing.
//
// The query parameters pin the connection so a live store cannot be written:
// mode=ro is the file-open flag, _query_only rejects write statements, and
// _defensive turns off the SQL-level knobs that can rewrite the file.
// _dqs=0 and trusted_schema=OFF disable double-quoted string literals and
// application functions in views and triggers, which _defensive does not.
// A short busy timeout waits out a writer instead of failing on the first lock.
func readOnlyDSN(path string) string {
	var b strings.Builder
	b.WriteString("file:")
	for i := 0; i < len(path); i++ {
		switch path[i] {
		case '%':
			b.WriteString("%25")
		case '?':
			b.WriteString("%3F")
		case '#':
			b.WriteString("%23")
		default:
			b.WriteByte(path[i])
		}
	}
	fmt.Fprintf(&b, "?mode=ro&_query_only=1&_busy_timeout=%d&_defensive=1&_dqs=0&_pragma=trusted_schema=OFF",
		dbBusyTimeout.Milliseconds())
	return b.String()
}

// openReadOnly opens a session database for a single short read. The pool is
// one connection: SQLite serializes writers, and this process never writes.
func openReadOnly(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

// maxOpenStores bounds the open-handle table. The key set is the set of agent
// stores the running watchers read, which is one per project an agent has been
// started in, and a dashboard left running across a churn of worktrees grows it
// the way storeReadState does. The cap is the same trade for the same reason:
// an evicted handle is a store no watcher read recently, and its next read
// opens one.
const maxOpenStores = 32

// storeHandle is one open store and the file it was opened on. The identity
// rides along because the handle is now kept open across polls: a store the
// agent replaces (a fresh crush.db after a reinstall, a database rebuilt by a
// test) leaves the old handle reading an unlinked inode, and the dashboard
// would report the deleted store's numbers for as long as the process ran.
type storeHandle struct {
	db *sql.DB
	fi os.FileInfo
}

// openStores holds one read-only handle per store, so a poll that runs several
// times a second does not reopen the database each time. Opening one is not
// cheap: SQLite parses the DSN, applies six connection parameters and prepares
// the statement, and a store holding a handful of sessions measures 132µs to
// open, query and close against 42µs to query on a handle already open. That
// is a third of a 250ms poll interval spent re-establishing a connection whose
// target has not moved.
var openStores = struct {
	sync.Mutex
	byPath map[string]*storeHandle
	order  []string // the same keys in insertion order, oldest first
}{byPath: map[string]*storeHandle{}}

// openStore returns a read-only handle on the database at path, reusing the one
// already open on that exact file. The stat is what decides reuse: a path whose
// file is no longer the one the handle was opened on (same inode, same device)
// is a replaced store, and the old handle is closed rather than served. A read
// that failed drops its handle through closeStore, so a corrupt page or a
// database left mid-recovery costs one poll rather than every poll after it.
//
// Caller must not hold openStores' lock; the handle is shared, so a caller that
// wants it released says so with closeStore, not by closing the *sql.DB.
func openStore(path string) (*sql.DB, error) {
	openStores.Lock()
	defer openStores.Unlock()
	if h, ok := openStores.byPath[path]; ok {
		if sameFile(h.fi, statFile(path)) {
			return h.db, nil
		}
		_ = h.db.Close()
		delete(openStores.byPath, path)
		openStores.order = removeKey(openStores.order, path)
	}
	db, err := openReadOnly(path)
	if err != nil {
		return nil, err
	}
	openStores.byPath[path] = &storeHandle{db: db, fi: statFile(path)}
	openStores.order = append(openStores.order, path)
	for len(openStores.order) > maxOpenStores {
		victim := openStores.order[0]
		openStores.order = openStores.order[1:]
		if h, ok := openStores.byPath[victim]; ok {
			_ = h.db.Close()
			delete(openStores.byPath, victim)
		}
	}
	return db, nil
}

// closeStore drops the cached handle for path, and is what a caller calls after
// a read that failed: the handle is not the thing to keep alive once a read
// through it has gone wrong. A path with no cached handle is a no-op, since a
// read that never opened one has nothing to drop.
func closeStore(path string) {
	openStores.Lock()
	defer openStores.Unlock()
	if h, ok := openStores.byPath[path]; ok {
		_ = h.db.Close()
		delete(openStores.byPath, path)
		openStores.order = removeKey(openStores.order, path)
	}
}

// removeKey drops key from a FIFO order list. The lists hold at most
// maxOpenStores entries, so the linear scan costs nothing against the open it
// saves.
func removeKey(order []string, key string) []string {
	for i, k := range order {
		if k == key {
			return append(order[:i], order[i+1:]...)
		}
	}
	return order
}

// statFile stats path, reporting a nil FileInfo for a failure. A store that is
// not there is a case the callers already handle, and a nil here fails
// sameFile, so the handle is replaced and the open that follows reports the
// absence with its own error.
func statFile(path string) os.FileInfo {
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return fi
}

// sameFile reports whether two stats name the same file, with a missing stat
// on either side meaning they do not.
func sameFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b)
}

// storeAbsent reports whether a store that failed to read is simply not there.
// A machine without the agent installed has no database, and that is an answer
// rather than a fault worth a log line. Any other cause is not: a store that
// exists and cannot be read reports no usage forever, which reads on the
// dashboard exactly like an idle agent.
func storeAbsent(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// storeReadState records which stores are currently failing, so a store that
// does exist and will not read is reported once per outage rather than once
// per poll. The read runs several times a second, and a corrupt page or a
// database under continuous write fails every time, so an unlatched line is
// terminal spam for as long as the dashboard runs and unbounded growth for a
// host that redirects its log.
//
// The table is capped, not merely reasoned about. A project-local store is
// found by walking up from the agent's working directory, so the key set is
// every project an agent has ever run in, not every project on disk: a
// dashboard left running across a churn of worktrees, containers and scratch
// checkouts accumulates a row for a directory that is long gone, and nothing
// read ever names it again. The count cap is the same trade the collector's id
// ledger makes, in the same shape: an evicted store is a store that has failed
// once and stayed quiet since, and its next failure is a new outage to report
// anyway.
var storeReadState = struct {
	sync.Mutex
	states map[string]*storeRead
	order  []string // the same keys in insertion order, oldest first
}{states: map[string]*storeRead{}}

// maxStoreReads bounds the latch table. Real usage is a handful of agents in
// a handful of projects; the cap only bites on a host churning project
// directories faster than any of them recovers.
const maxStoreReads = 256

type storeRead struct {
	failed bool
}

func storeReadKey(agent, path string) string { return agent + "\x00" + path }

// storeReadForLocked returns the latch for one store, recording its first
// sighting so the order carries every live key. An eviction drops the oldest
// key that is not currently failing rather than the entry the caller is about
// to touch, so a store at the cap keeps its own latch. Caller holds
// storeReadState.
func storeReadForLocked(key string) *storeRead {
	if r, ok := storeReadState.states[key]; ok {
		return r
	}
	r := &storeRead{}
	storeReadState.states[key] = r
	storeReadState.order = append(storeReadState.order, key)
	// The index the key just appended sits at, so the sweep below cannot pick
	// it. Its latch is the one being handed out: evicting it would return a
	// *storeRead no longer in the table, and every later failure of the same
	// store would miss the latch, read as a new outage, and log the same line
	// again for as long as the store stays broken.
	held := len(storeReadState.order) - 1
	for len(storeReadState.order) > maxStoreReads {
		evictStoreReadLocked(held)
	}
	return r
}

// evictStoreReadLocked drops one key from the latch table, the oldest one
// whose latch is not set. A store that has failed once and stayed quiet since
// is the entry the cap exists to reclaim, and it is the only one whose loss
// costs nothing: its next failure is a new outage to report anyway.
//
// A store that is still failing is the entry that must survive, because the
// whole point of the latch is to hold it for the length of the outage. The
// oldest key alone is not safe to drop when the table is churning: a host
// running many agent projects accumulates keys for directories that are long
// gone, and one sweep through them evicts the latch of the store that is
// failing right now, whose next poll then reads as a new outage and logs the
// same line again. Every other key in the table is either failing too or has
// nothing to lose, so the oldest is the fallback when there is no quiet one.
//
// held is the index of the caller's own key, appended before this runs and
// never a candidate. A table where every other key is still failing has no
// quiet one to drop, and the entry that must not be lost is the one the caller
// is holding, so the oldest of the rest is taken: without the exclusion a full
// table of failing stores evicted the caller's key instead, leaving a latch
// that is in no table and a cap the sweep can no longer bring back under.
func evictStoreReadLocked(held int) {
	drop := -1
	for i, key := range storeReadState.order {
		if i != held && !storeReadState.states[key].failed {
			drop = i
			break
		}
	}
	if drop < 0 {
		for i := range storeReadState.order {
			if i != held {
				drop = i
				break
			}
		}
	}
	key := storeReadState.order[drop]
	delete(storeReadState.states, key)
	storeReadState.order = append(storeReadState.order[:drop], storeReadState.order[drop+1:]...)
	// The caller's key shifted down with everything above the drop.
	if drop < held {
		held--
	}
}

// markStoreFailed latches one store's failure and reports whether this call
// opened the outage, so the caller logs a line once per outage. The lookup, the
// write and the eviction sweep run under one lock: a caller that released the
// lock between finding the latch and writing it could be holding an entry the
// sweep has since evicted, and the next failure of that store would then find
// a fresh latch and log the same outage again.
func markStoreFailed(key string) (first bool) {
	storeReadState.Lock()
	defer storeReadState.Unlock()
	r := storeReadForLocked(key)
	first = !r.failed
	r.failed = true
	return first
}

// auditStoreRead records a read failure against a store that does exist. It
// names the agent and the store so the operator can tell a corrupt or
// permission-denied database from an idle agent, and keeps the driver's message
// as the cause rather than restating the failure without it. A store already
// recorded as failing adds nothing: the line naming the start of the outage
// said the reason, and every later poll would only repeat it.
func auditStoreRead(agent, path string, err error) {
	if !markStoreFailed(storeReadKey(agent, path)) {
		return
	}
	auditLogger().Warn("agent usage store read failed",
		"agent", agent,
		"path", core.RedactHome(path),
		"error", core.RedactHome(core.Snippet([]byte(err.Error()))))
}

// noteStoreReadOK clears a recorded outage, so a store that reads again is
// distinguishable on the log from one that never did, and a failure after it
// is reported as the new thing it is.
func noteStoreReadOK(agent, path string) {
	storeReadState.Lock()
	defer storeReadState.Unlock()
	if r, ok := storeReadState.states[storeReadKey(agent, path)]; ok {
		r.failed = false
	}
}
