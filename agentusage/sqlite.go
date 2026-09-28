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
// An entry is never removed. The keys are the agent's stores, one per project
// for a project-local store, so the table is bounded by what is on disk.
var storeReadState sync.Map // agent + "\x00" + path -> *storeRead

type storeRead struct {
	mu     sync.Mutex
	failed bool
}

func storeReadKey(agent, path string) string { return agent + "\x00" + path }

// auditStoreRead records a read failure against a store that does exist. It
// names the agent and the store so the operator can tell a corrupt or
// permission-denied database from an idle agent, and keeps the driver's message
// as the cause rather than restating the failure without it. A store already
// recorded as failing adds nothing: the line naming the start of the outage
// said the reason, and every later poll would only repeat it.
func auditStoreRead(agent, path string, err error) {
	s, _ := storeReadState.LoadOrStore(storeReadKey(agent, path), &storeRead{})
	r := s.(*storeRead)
	r.mu.Lock()
	first := !r.failed
	r.failed = true
	r.mu.Unlock()
	if !first {
		return
	}
	audit().Warn("agent usage store read failed",
		"agent", agent,
		"path", core.RedactHome(path),
		"error", core.RedactHome(core.Snippet([]byte(err.Error()))))
}

// noteStoreReadOK clears a recorded outage, so a store that reads again is
// distinguishable on the log from one that never did, and a failure after it
// is reported as the new thing it is.
func noteStoreReadOK(agent, path string) {
	s, ok := storeReadState.Load(storeReadKey(agent, path))
	if !ok {
		return
	}
	r := s.(*storeRead)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = false
}
