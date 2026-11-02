// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build sqlite

package agentusage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// opencodeDB builds a session store shaped like the real one and points the
// source at it.
func opencodeDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createOpenCodeSchema(t, db)
	return path
}

// createOpenCodeSchema matches opencode's session/message tables and the
// message index the usage query is written against. The index is the one
// opencode declares, message_session_time_created_id_idx: session_id leading,
// then time_created, then id. A fixture narrowed to session_id alone made the
// query look like it scanned messages it does not, and the plan assertions
// below were checking a shape no real store has.
func createOpenCodeSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE TABLE session (id text PRIMARY KEY, directory text NOT NULL)`,
		`CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL,
			time_created integer NOT NULL, data text NOT NULL,
			FOREIGN KEY (session_id) REFERENCES session(id))`,
		`CREATE INDEX message_session_time_created_id_idx ON message (session_id, time_created, id)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
}

func addSession(t *testing.T, path, id, dir string) {
	t.Helper()
	execSQL(t, path, `INSERT INTO session (id, directory) VALUES (?, ?)`, id, dir)
}

func addMessage(t *testing.T, path, id, session string, at time.Time, data string) {
	t.Helper()
	execSQL(t, path, `INSERT INTO message (id, session_id, time_created, data) VALUES (?, ?, ?, ?)`,
		id, session, at.UnixMilli(), data)
}

func execSQL(t *testing.T, path, stmt string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(stmt, args...); err != nil {
		t.Fatal(err)
	}
}

// withOpenCodeDB points the source at a test database for one test.
func withOpenCodeDB(t *testing.T, path string) {
	t.Helper()
	registerSource("opencode", tokenSource{usage: openCodeDBSource{path: path}})
	t.Cleanup(func() { registerSource("opencode", tokenSource{}) })
}

func TestOpenCodeDBCountsThisReviewOnly(t *testing.T) {
	path := opencodeDB(t)
	work, other := t.TempDir(), t.TempDir()
	addSession(t, path, "s-mine", work)
	addSession(t, path, "s-theirs", other)

	start := time.Now()
	// Everything spent in the same directory before this attach.
	addMessage(t, path, "m0", "s-mine", start.Add(-time.Hour),
		`{"role":"assistant","tokens":{"output":9000,"reasoning":100,"total":50000}}`)
	withOpenCodeDB(t, path)

	w := Watch("opencode", work, start)
	if w == nil {
		t.Fatal("opencode should be readable once the database is enabled")
	}
	w.poll(nil)
	if got := w.Sample().Output; got != 0 {
		t.Fatalf("counted tokens from before attach: %d", got)
	}

	addMessage(t, path, "m1", "s-mine", start.Add(time.Second),
		`{"role":"assistant","tokens":{"output":324,"reasoning":52,"total":131769,"input":900}}`)
	addMessage(t, path, "m2", "s-mine", start.Add(2*time.Second),
		`{"role":"assistant","tokens":{"output":120,"reasoning":8,"total":131900,"input":40}}`)
	// A concurrent session in another directory, which must not be counted.
	addMessage(t, path, "m3", "s-theirs", start.Add(time.Second),
		`{"role":"assistant","tokens":{"output":7777,"total":900000}}`)

	w.poll(nil)
	s := w.Sample()
	if s.Output != 444 {
		t.Fatalf("output tokens %d, want 444 (324+120)", s.Output)
	}
	if s.Thinking != 60 {
		t.Fatalf("thinking tokens %d, want 60 (52+8)", s.Thinking)
	}
	if s.Total != 131900 {
		t.Fatalf("total %d, want the largest context reported, 131900", s.Total)
	}
	if s.Input != 940 {
		t.Fatalf("input tokens %d, want 940 (900+40)", s.Input)
	}
}

func TestOpenCodeV2Usage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT, fork_session_id TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, type TEXT, created INTEGER, data TEXT)`)
	start := time.Now().Truncate(time.Millisecond)
	execSQL(t, path, `INSERT INTO session_v2 VALUES ('s', '/work', NULL, ?)`, start.UnixMilli())
	execSQL(t, path, `INSERT INTO session_v2 VALUES ('other', '/elsewhere', NULL, ?)`, start.UnixMilli())
	execSQL(t, path, `INSERT INTO session_v2 VALUES ('fork', '/work', 's', ?)`, start.Add(time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO session_message VALUES ('old', 's', 'assistant', ?, '{"tokens":{"output":999}}')`, start.Add(-time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO session_message VALUES ('crossing', 's', 'assistant', ?, ?)`, start.Add(-time.Second).UnixMilli(), fmt.Sprintf(`{"time":{"completed":%d},"tokens":{"output":10}}`, start.Add(time.Second).UnixMilli()))
	execSQL(t, path, `INSERT INTO session_message VALUES ('copy', 'fork', 'assistant', ?, '{"tokens":{"output":999}}')`, start.UnixMilli())
	execSQL(t, path, `INSERT INTO session_message VALUES ('prompt', 's', 'user', ?, '{"tokens":{"output":999}}')`, start.Add(time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO session_message VALUES ('new', 's', 'assistant', ?, '{"tokens":{"input":12,"output":34,"reasoning":5,"cache":{"read":100,"write":10}}}')`, start.Add(time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', ?, '{"source":"compaction","tokens":{"input":3,"output":7,"reasoning":2}}')`, start.Add(time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', ?, '{"source":"title","tokens":{"output":999}}')`, start.Add(-time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('other', 'session.usage.recorded.1', ?, '{"source":"title","tokens":{"output":999}}')`, start.Add(time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', ?, 'broken')`, start.Add(time.Second).UnixMilli())
	v, ok := (openCodeDBSource{path: path}).read([]string{"/work"}, start)
	if !ok || v.input != 15 || v.output != 58 || v.thinking != 7 || v.total != 161 {
		t.Fatalf("read = %+v, %v; want input 15, output 58, thinking 7, total 161", v, ok)
	}
}

func TestOpenCodeV2EventContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, type TEXT, created INTEGER, data TEXT)`)
	start := time.Now().Truncate(time.Millisecond)
	execSQL(t, path, `INSERT INTO session_v2 VALUES ('s', '/work', ?)`, start.UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', ?, '{"source":"compaction","tokens":{"input":100,"output":5,"reasoning":2,"cache":{"read":50,"write":3}}}'), ('s', 'session.usage.recorded.1', ?, '{"source":"title","tokens":{"input":20,"output":4}}')`, start.Add(time.Second).UnixMilli(), start.Add(2*time.Second).UnixMilli())
	for _, since := range []time.Time{start, {}} {
		v, ok := (openCodeDBSource{path: path}).read([]string{"/work"}, since)
		if !ok || v.total != 160 || v.input != 120 || v.output != 11 {
			t.Fatalf("since %v: usage = %+v, %v; want peak context 160", since, v, ok)
		}
	}
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', ?, '{"source":"title","tokens":{"input":1e30}}')`, start.Add(3*time.Second).UnixMilli())
	if v, ok := (openCodeDBSource{path: path}).read([]string{"/work"}, start); !ok || v.total != 160 {
		t.Fatalf("oversized event hid valid context: %+v, %v", v, ok)
	}
}

func TestOpenCodeV2OversizedMessageDoesNotHideContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, type TEXT, created INTEGER, data TEXT)`)
	start := time.Now().Truncate(time.Millisecond)
	execSQL(t, path, `INSERT INTO session_v2 VALUES ('s', '/work', ?)`, start.UnixMilli())
	execSQL(t, path, `INSERT INTO session_message VALUES ('valid', 's', 'assistant', ?, '{"tokens":{"input":100,"output":5}}'), ('invalid', 's', 'assistant', ?, '{"tokens":{"input":1e30,"output":1e30}}')`, start.Add(time.Second).UnixMilli(), start.Add(time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', ?, '{"source":"title","tokens":{"input":1e30,"output":1e30}}')`, start.Add(time.Second).UnixMilli())
	v, ok := (openCodeDBSource{path: path}).read([]string{"/work"}, start)
	if !ok || v.total != 105 || v.output != 5 || v.input != 100 {
		t.Fatalf("usage = %+v, %v; want context 105, input 100, output 5", v, ok)
	}
}

func TestOpenCodeV2WatcherEventContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, type TEXT, created INTEGER, data TEXT)`)
	start := time.Now().Truncate(time.Millisecond)
	execSQL(t, path, `INSERT INTO session_v2 VALUES ('s', '/work', ?)`, start.UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', ?, '{"source":"title","tokens":{"input":999}}')`, start.Add(-time.Second).UnixMilli())
	t.Setenv(OpenCodeDBEnv, path)
	if !EnableOpenCodeDB(true) {
		t.Fatal("SQLite source unavailable")
	}
	t.Cleanup(func() { EnableOpenCodeDB(false) })
	w := Watch("opencode", "/work", start)
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', ?, '{"source":"compaction","tokens":{"input":100,"output":5,"reasoning":2,"cache":{"read":50,"write":3}}}')`, start.Add(time.Second).UnixMilli())
	for range 2 {
		got := w.Poll()
		if got.Input != 100 || got.Output != 7 || got.Thinking != 2 || got.Total != 160 {
			t.Fatalf("watcher usage = %+v, want input 100, output 7, thinking 2, context 160", got)
		}
	}
}

func TestOpenCodeV2NullIDsDoNotBreakWatcher(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (id TEXT PRIMARY KEY, aggregate_id TEXT, type TEXT, created INTEGER, data TEXT)`)
	start := time.Now().Truncate(time.Millisecond)
	at := start.UnixMilli()
	execSQL(t, path, `INSERT INTO session_v2 VALUES ('s', '/work', ?)`, at)
	execSQL(t, path, `INSERT INTO event VALUES (NULL, 's', 'session.usage.recorded.1', ?, '{"source":"title"}')`, at)
	execSQL(t, path, `INSERT INTO session_message VALUES (NULL, 's', 'assistant', ?, '{}')`, at)
	t.Setenv(OpenCodeDBEnv, path)
	if !EnableOpenCodeDB(true) {
		t.Fatal("SQLite source unavailable")
	}
	t.Cleanup(func() { EnableOpenCodeDB(false) })
	w := Watch("opencode", "/work", start)
	execSQL(t, path, `INSERT INTO session_message VALUES ('valid', 's', 'assistant', ?, '{"tokens":{"input":10,"output":5}}')`, at)
	execSQL(t, path, `INSERT INTO event VALUES (NULL, 's', 'session.usage.recorded.1', ?, '{"source":"title","tokens":{"input":900,"output":900}}')`, at)
	execSQL(t, path, `INSERT INTO session_message VALUES (NULL, 's', 'assistant', ?, '{"tokens":{"input":900,"output":900}}')`, at)
	execSQL(t, path, `INSERT INTO event VALUES (NULL, 's', 'session.usage.recorded.1', ?, '{"source":"title","tokens":{"input":900,"output":900}}')`, start.Add(time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO session_message VALUES (NULL, 's', 'assistant', ?, '{"tokens":{"input":900,"output":900}}')`, start.Add(time.Second).UnixMilli())
	if got := w.Poll(); got.Input != 10 || got.Output != 5 || got.Total != 15 {
		t.Fatalf("usage with null IDs = %+v, want input 10, output 5, context 15", got)
	}
}

func TestOpenCodeV2ProductionSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	work := t.TempDir()
	// The relevant columns and indexes from OpenCode 2's generated schema.
	for _, stmt := range []string{
		`CREATE TABLE session_v2 (id TEXT PRIMARY KEY, project_id TEXT NOT NULL, parent_id TEXT, fork_session_id TEXT, slug TEXT NOT NULL, directory TEXT NOT NULL, version TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL)`,
		`CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, type TEXT NOT NULL, seq INTEGER NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE UNIQUE INDEX session_message_session_seq_idx ON session_message (session_id, seq)`,
		`CREATE INDEX session_message_session_type_seq_idx ON session_message (session_id, type, seq)`,
		`CREATE INDEX session_message_session_time_created_id_idx ON session_message (session_id, time_created, id)`,
		`CREATE TABLE event (id TEXT PRIMARY KEY, aggregate_id TEXT NOT NULL, seq INTEGER NOT NULL, created INTEGER DEFAULT 0 NOT NULL, type TEXT NOT NULL, data TEXT NOT NULL)`,
		`CREATE INDEX event_aggregate_type_seq_idx ON event (aggregate_id, type, seq)`,
	} {
		execSQL(t, path, stmt)
	}
	start := time.Now().Add(-time.Second).Truncate(time.Millisecond)
	for _, row := range []struct{ id, parent string }{{"root", ""}, {"child", "root"}} {
		execSQL(t, path, `INSERT INTO session_v2 (id, project_id, parent_id, slug, directory, version, time_created, time_updated) VALUES (?, 'p', NULLIF(?, ''), 's', ?, '2', ?, ?)`, row.id, row.parent, work, start.UnixMilli(), start.UnixMilli())
	}
	withOpenCodeDB(t, path)
	// Use the public registration path so the watcher snapshots auxiliary events.
	registerSource("opencode", tokenSource{usage: openCodeDBSource{path: path}, auxiliary: openCodeUntimedSource{path: path}})
	w := Watch("opencode", work, start)
	at := start.Add(500 * time.Millisecond).UnixMilli()
	execSQL(t, path, `INSERT INTO session_message VALUES ('root-msg', 'root', 'assistant', 1, ?, ?, '{"tokens":{"input":10,"output":20,"reasoning":3}}'), ('child-msg', 'child', 'assistant', 1, ?, ?, '{"tokens":{"input":4,"output":5,"reasoning":2}}')`, at, at, at, at)
	execSQL(t, path, `INSERT INTO event VALUES ('title', 'root', 1, ?, 'session.usage.recorded.1', '{"source":"title","tokens":{"input":2,"output":1,"reasoning":1}}')`, at)
	got := w.Poll()
	if got.Input != 16 || got.Output != 32 || got.Thinking != 6 {
		t.Fatalf("production schema usage = %+v, want input 16, output 32, thinking 6", got)
	}
}

func TestOpenCodeV2CountsChildSessionUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY, parent_id TEXT, directory TEXT, fork_session_id TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, type TEXT, created INTEGER, data TEXT)`)
	start := time.Now().Truncate(time.Millisecond)
	execSQL(t, path, `INSERT INTO session_v2 VALUES ('root', NULL, '/work', NULL, ?), ('child', 'root', '/work', NULL, ?), ('other', NULL, '/other', NULL, ?)`, start.UnixMilli(), start.UnixMilli(), start.UnixMilli())
	for _, row := range []struct{ id, session, tokens string }{
		{"root-message", "root", `{"output":40,"reasoning":7}`},
		{"child-message", "child", `{"output":20,"reasoning":3}`},
		{"other-message", "other", `{"output":999}`},
	} {
		execSQL(t, path, `INSERT INTO session_message VALUES (?, ?, 'assistant', ?, ?)`, row.id, row.session, start.Add(time.Second).UnixMilli(), `{"tokens":`+row.tokens+`}`)
	}
	v, ok := (openCodeDBSource{path: path}).read([]string{"/work"}, start)
	if !ok || v.output != 70 || v.thinking != 10 {
		t.Fatalf("usage = %+v, %v; want root and child output 70, thinking 10", v, ok)
	}
}

func TestOpenCodeV2CountsUsageAtAttachMillisecond(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT, fork_session_id TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (id TEXT PRIMARY KEY, aggregate_id TEXT, type TEXT, created INTEGER, data TEXT)`)
	start := time.Now().Truncate(time.Millisecond)
	execSQL(t, path, `INSERT INTO session_v2 VALUES ('s', '/work', NULL, ?)`, start.UnixMilli())
	execSQL(t, path, `INSERT INTO session_message VALUES ('old', 's', 'assistant', ?, '{"tokens":{"output":100}}')`, start.UnixMilli())
	execSQL(t, path, `INSERT INTO session_message VALUES ('crossing', 's', 'assistant', ?, '{}')`, start.Add(-time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('old', 's', 'session.usage.recorded.1', ?, '{"source":"title","tokens":{"output":200}}')`, start.UnixMilli())
	t.Setenv(OpenCodeDBEnv, path)
	if !EnableOpenCodeDB(true) {
		t.Fatal("SQLite source unavailable")
	}
	t.Cleanup(func() { EnableOpenCodeDB(false) })
	w := Watch("opencode", "/work", start)
	execSQL(t, path, `INSERT INTO session_message VALUES ('new', 's', 'assistant', ?, '{"tokens":{"output":4,"reasoning":1}}')`, start.UnixMilli())
	execSQL(t, path, `UPDATE session_message SET data = ? WHERE id = 'crossing'`, fmt.Sprintf(`{"time":{"completed":%d},"tokens":{"output":3}}`, start.UnixMilli()))
	execSQL(t, path, `INSERT INTO event VALUES ('new', 's', 'session.usage.recorded.1', ?, '{"source":"compaction","tokens":{"input":100,"output":2,"reasoning":1,"cache":{"read":57}}}')`, start.UnixMilli())
	for range 2 {
		if got := w.Poll(); got.Output != 11 || got.Thinking != 2 || got.Total != 160 {
			t.Fatalf("same-millisecond usage = %+v, want output 11, thinking 2 and context 160 without old usage", got)
		}
	}
	execSQL(t, path, `INSERT INTO session_message VALUES ('later', 's', 'assistant', ?, '{"tokens":{"input":1000,"output":1,"cache":{"read":499}}}')`, start.UnixMilli())
	if got := w.Poll(); got.Output != 12 || got.Total != 1500 {
		t.Fatalf("same-millisecond message context = %+v, want output 12 and context 1500", got)
	}
}

func TestOpenCodeV2CountsUsageFromRetriedSteps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT, fork_session_id TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, seq INTEGER, type TEXT, created INTEGER, data TEXT)`)
	start := time.Now().Truncate(time.Millisecond)
	execSQL(t, path, `INSERT INTO session_v2 VALUES ('s', '/work', NULL, ?)`, start.UnixMilli())
	t.Setenv(OpenCodeDBEnv, path)
	if !EnableOpenCodeDB(true) {
		t.Fatal("SQLite source unavailable")
	}
	t.Cleanup(func() { EnableOpenCodeDB(false) })
	w := Watch("opencode", "/work", start)
	execSQL(t, path, `INSERT INTO session_message VALUES ('m', 's', 'assistant', ?, '{"tokens":{"output":5,"reasoning":2}}')`, start.Add(time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 1, 'session.step.failed.1', ?, '{"assistantMessageID":"m","tokens":{"output":3,"reasoning":1}}')`, start.Add(time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 2, 'session.step.ended.1', ?, '{"assistantMessageID":"m","tokens":{"output":5,"reasoning":2}}')`, start.Add(2*time.Second).UnixMilli())
	if got := w.Poll(); got.Output != 11 || got.Thinking != 3 {
		t.Fatalf("retried usage = %+v, want output 11 and thinking 3", got)
	}
	execSQL(t, path, `INSERT INTO session_message VALUES ('m2', 's', 'assistant', ?, '{"tokens":{"output":1}}'), ('m3', 's', 'assistant', ?, '{"tokens":{"output":4}}')`, start.Add(time.Second).UnixMilli(), start.Add(time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 3, 'session.step.failed.1', ?, '{"assistantMessageID":"m2","tokens":{"output":2,"reasoning":1}}')`, start.UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 4, 'session.step.ended.1', ?, '{"assistantMessageID":"m2","tokens":{"output":1}}')`, start.Add(time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 5, 'session.step.failed.1', ?, '{"assistantMessageID":"m3","tokens":{"output":4}}')`, start.Add(time.Second).UnixMilli())
	if got := w.Poll(); got.Output != 19 || got.Thinking != 4 {
		t.Fatalf("boundary and final failure usage = %+v, want output 19 and thinking 4", got)
	}
	if got := Watch("opencode", "/work", time.Time{}).Poll(); got.Output != 19 || got.Thinking != 4 {
		t.Fatalf("unbounded retried usage = %+v, want output 19 and thinking 4", got)
	}
	execSQL(t, path, `INSERT INTO session_message VALUES ('m4', 's', 'assistant', ?, '{"tokens":{"output":6,"reasoning":1}}')`, start.Add(time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 6, 'session.step.failed.1', ?, '{"assistantMessageID":"m4","tokens":{"output":6,"reasoning":1}}')`, start.Add(time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 7, 'session.step.started.1', ?, '{"assistantMessageID":"m4"}')`, start.Add(2*time.Second).UnixMilli())
	if got := w.Poll(); got.Output != 26 || got.Thinking != 5 {
		t.Fatalf("running retry = %+v, want output 26 and thinking 5", got)
	}
	execSQL(t, path, `UPDATE session_message SET data = ? WHERE id = 'm4'`, fmt.Sprintf(`{"time":{"completed":%d},"tokens":{"output":3}}`, start.Add(3*time.Second).UnixMilli()))
	execSQL(t, path, `INSERT INTO event VALUES ('s', 8, 'session.step.ended.1', ?, '{"assistantMessageID":"m4","tokens":{"output":3}}')`, start.Add(3*time.Second).UnixMilli())
	if got := w.Poll(); got.Output != 29 || got.Thinking != 5 {
		t.Fatalf("completed retry = %+v, want output 29 and thinking 5", got)
	}
}

func TestOpenCodeV2RetryFailureWithoutUsageDoesNotRepeatOldTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, seq INTEGER, type TEXT, created INTEGER, data TEXT)`)
	start := time.Now().Add(-time.Second).Truncate(time.Millisecond)
	at := start.Add(500 * time.Millisecond).UnixMilli()
	execSQL(t, path, `INSERT INTO session_v2 VALUES ('s', '/work', ?)`, at)
	execSQL(t, path, `INSERT INTO session_message VALUES ('m', 's', 'assistant', ?, ?)`, at, fmt.Sprintf(`{"time":{"completed":%d},"tokens":{"output":5,"reasoning":2}}`, at))
	execSQL(t, path, `INSERT INTO event VALUES ('s',1,'session.step.failed.1',?, '{"assistantMessageID":"m","tokens":{"output":5,"reasoning":2}}'), ('s',2,'session.step.started.1',?, '{"assistantMessageID":"m"}'), ('s',3,'session.step.failed.1',?, '{"assistantMessageID":"m"}')`, at, at, at)
	v, ok := (openCodeDBSource{path: path}).read([]string{"/work"}, start)
	if !ok || v.output != 7 || v.thinking != 2 {
		t.Fatalf("retry usage = %+v, ok %v; want output 7 and thinking 2", v, ok)
	}
	v, ok = (openCodeDBSource{path: path}).read([]string{"/work"}, time.Time{})
	if !ok || v.output != 7 || v.thinking != 2 {
		t.Fatalf("unbounded retry usage = %+v, ok %v; want output 7 and thinking 2", v, ok)
	}
	execSQL(t, path, `UPDATE event SET data = '{"assistantMessageID":"m","tokens":{"output":3}}' WHERE seq = 3`)
	for _, since := range []time.Time{start, {}} {
		v, ok = (openCodeDBSource{path: path}).read([]string{"/work"}, since)
		if !ok || v.output != 10 || v.thinking != 2 {
			t.Fatalf("retry usage without cost = %+v, ok %v; want output 10 and thinking 2", v, ok)
		}
	}
	execSQL(t, path, `UPDATE event SET data = '{"assistantMessageID":"m","cost":0,"tokens":{"output":3}}' WHERE seq = 3`)
	execSQL(t, path, `UPDATE session_message SET data = ? WHERE id = 'm'`, fmt.Sprintf(`{"time":{"completed":%d},"tokens":{"output":3}}`, at))
	v, ok = (openCodeDBSource{path: path}).read([]string{"/work"}, start)
	if !ok || v.output != 10 || v.thinking != 2 {
		t.Fatalf("projected retry usage = %+v, ok %v; want output 10 and thinking 2", v, ok)
	}
}

func TestOpenCodeV2RetryDoesNotCountFailureBeforeAttach(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT, fork_session_id TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, seq INTEGER, type TEXT, created INTEGER, data TEXT)`)
	start := time.Now().Truncate(time.Millisecond)
	execSQL(t, path, `INSERT INTO session_v2 VALUES ('s', '/work', NULL, ?)`, start.UnixMilli())
	execSQL(t, path, `INSERT INTO session_message VALUES ('m', 's', 'assistant', ?, '{"tokens":{"output":100}}')`, start.UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 1, 'session.step.failed.1', ?, '{"assistantMessageID":"m","tokens":{"output":100}}')`, start.UnixMilli())
	t.Setenv(OpenCodeDBEnv, path)
	if !EnableOpenCodeDB(true) {
		t.Fatal("SQLite source unavailable")
	}
	t.Cleanup(func() { EnableOpenCodeDB(false) })
	w := Watch("opencode", "/work", start)
	if got := w.Poll(); got.Output != 0 {
		t.Fatalf("counted pre-attach failure: %+v", got)
	}
	execSQL(t, path, `UPDATE session_message SET time_created = ? WHERE id = 'm'`, start.Add(time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 2, 'session.step.started.1', ?, '{"assistantMessageID":"m"}')`, start.Add(time.Second).UnixMilli())
	if got := w.Poll(); got.Output != 0 {
		t.Fatalf("counted previous attempt while retry is running: %+v", got)
	}
	execSQL(t, path, `UPDATE session_message SET time_created = ?, data = ? WHERE id = 'm'`, start.Add(time.Second).UnixMilli(), fmt.Sprintf(`{"time":{"completed":%d},"tokens":{"output":5}}`, start.Add(time.Second).UnixMilli()))
	execSQL(t, path, `INSERT INTO event VALUES ('s', 3, 'session.step.ended.1', ?, '{"assistantMessageID":"m","tokens":{"output":5}}')`, start.Add(time.Second).UnixMilli())
	if got := w.Poll(); got.Output != 5 {
		t.Fatalf("counted pre-attach failure after retry: %+v, want 5", got)
	}
}

func TestOpenCodeV2RetryWithoutCostAtAttachBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, seq INTEGER, type TEXT, created INTEGER, data TEXT)`)
	start := time.Now().Truncate(time.Millisecond)
	at := start.UnixMilli()
	execSQL(t, path, `INSERT INTO session_v2 VALUES ('s', '/work', ?)`, at)
	execSQL(t, path, `INSERT INTO session_message VALUES ('m', 's', 'assistant', ?, ?)`, at, fmt.Sprintf(`{"time":{"completed":%d},"tokens":{"input":1000,"output":100}}`, at))
	execSQL(t, path, `INSERT INTO event VALUES ('s',1,'session.step.failed.1',?, '{"assistantMessageID":"m","cost":0,"tokens":{"input":1000,"output":100}}')`, at)
	t.Setenv(OpenCodeDBEnv, path)
	if !EnableOpenCodeDB(true) {
		t.Fatal("SQLite source unavailable")
	}
	t.Cleanup(func() { EnableOpenCodeDB(false) })
	w := Watch("opencode", "/work", start)
	execSQL(t, path, `INSERT INTO event VALUES ('s',2,'session.step.started.1',?, '{"assistantMessageID":"m"}'), ('s',3,'session.step.failed.1',?, '{"assistantMessageID":"m","tokens":{"output":3,"reasoning":1}}')`, at, at)
	for range 2 {
		if got := w.Poll(); got.Output != 4 || got.Thinking != 1 {
			t.Fatalf("boundary retry = %+v, want output 4 and thinking 1", got)
		}
	}
	execSQL(t, path, `UPDATE session_message SET data = ? WHERE id = 'm'`, fmt.Sprintf(`{"time":{"completed":%d},"tokens":{"input":20,"output":2,"reasoning":1}}`, at))
	execSQL(t, path, `INSERT INTO event VALUES ('s',4,'session.step.failed.1',?, '{"assistantMessageID":"m","cost":0,"tokens":{"input":20,"output":2,"reasoning":1}}')`, at)
	if got := w.Poll(); got.Total != 23 || got.Output != 7 || got.Input != 20 || got.Thinking != 2 {
		t.Fatalf("smaller failed retry = %+v, want context 23, output 7, input 20, thinking 2", got)
	}
}

func TestOpenCodeV2IdenticalRetryContextAtAttach(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, seq INTEGER, type TEXT, created INTEGER, data TEXT)`)
	start := time.Now().Truncate(time.Millisecond)
	at := start.UnixMilli()
	execSQL(t, path, `INSERT INTO session_v2 VALUES ('s', '/work', ?)`, at)
	execSQL(t, path, `INSERT INTO session_message VALUES ('m', 's', 'assistant', ?, ?)`, at, fmt.Sprintf(`{"time":{"completed":%d},"tokens":{"input":100,"output":5}}`, at))
	execSQL(t, path, `INSERT INTO event VALUES ('s', 1, 'session.step.failed.1', ?, '{"assistantMessageID":"m","cost":0,"tokens":{"input":100,"output":5}}')`, at)
	t.Setenv(OpenCodeDBEnv, path)
	if !EnableOpenCodeDB(true) {
		t.Fatal("SQLite source unavailable")
	}
	t.Cleanup(func() { EnableOpenCodeDB(false) })
	w := Watch("opencode", "/work", start)
	if got := w.Poll(); got.Total != 0 {
		t.Fatalf("counted context before attach: %+v", got)
	}
	execSQL(t, path, `INSERT INTO event VALUES ('s', 2, 'session.step.started.1', ?, '{"assistantMessageID":"m"}'), ('s', 3, 'session.step.ended.1', ?, '{"assistantMessageID":"m"}')`, at, at)
	for range 2 {
		if got := w.Poll(); got.Total != 105 || got.Output != 5 || got.Input != 100 {
			t.Fatalf("identical retry usage = %+v, want context 105, output 5 and input 100", got)
		}
	}
}

func TestOpenCodeV2BoundaryMessagesUseIndependentBaselines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, seq INTEGER, type TEXT, created INTEGER, data TEXT)`)
	start := time.Now().Truncate(time.Millisecond)
	at := start.UnixMilli()
	execSQL(t, path, `INSERT INTO session_v2 VALUES ('s', '/work', ?)`, at)
	execSQL(t, path, `INSERT INTO session_message VALUES ('old', 's', 'assistant', ?, '{"tokens":{"output":100}}'), ('retry', 's', 'assistant', ?, '{"tokens":{"input":50,"output":5}}')`, at, at)
	execSQL(t, path, `INSERT INTO event VALUES ('s',1,'session.step.ended.1',?, '{"assistantMessageID":"retry"}')`, at)
	t.Setenv(OpenCodeDBEnv, path)
	if !EnableOpenCodeDB(true) {
		t.Fatal("SQLite source unavailable")
	}
	t.Cleanup(func() { EnableOpenCodeDB(false) })
	w := Watch("opencode", "/work", start)
	execSQL(t, path, `INSERT INTO session_message VALUES ('new', 's', 'assistant', ?, '{"tokens":{"input":20,"output":3}}')`, at)
	execSQL(t, path, `INSERT INTO event VALUES ('s',2,'session.step.started.1',?, '{"assistantMessageID":"retry"}'), ('s',3,'session.step.ended.1',?, '{"assistantMessageID":"retry"}')`, at, at)
	for range 2 {
		if got := w.Poll(); got.Input != 70 || got.Output != 8 || got.Total != 55 {
			t.Fatalf("boundary messages = %+v, want input 70, output 8, context 55", got)
		}
	}
}

func TestOpenCodeV2UntimedRetryUsesAttachBaseline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, seq INTEGER, type TEXT, data TEXT)`)
	start := time.Now().Truncate(time.Millisecond)
	execSQL(t, path, `INSERT INTO session VALUES ('s', '/work', ?)`, start.UnixMilli())
	execSQL(t, path, `INSERT INTO session_message VALUES ('old', 's', 'assistant', ?, '{"tokens":{"output":100}}')`, start.Add(-time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 1, 'session.step.failed.1', '{"assistantMessageID":"old","tokens":{"output":100}}')`)
	t.Setenv(OpenCodeDBEnv, path)
	if !EnableOpenCodeDB(true) {
		t.Fatal("SQLite source unavailable")
	}
	t.Cleanup(func() { EnableOpenCodeDB(false) })
	w := Watch("opencode", "/work", start)
	if got := w.Poll(); got.Output != 0 {
		t.Fatalf("counted pre-attach failure: %+v", got)
	}
	execSQL(t, path, `UPDATE session_message SET time_created = ?, data = '{"tokens":{"output":5}}' WHERE id = 'old'`, start.Add(time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 2, 'session.step.ended.1', '{"assistantMessageID":"old","tokens":{"output":5}}')`)
	execSQL(t, path, `INSERT INTO session_message VALUES ('new', 's', 'assistant', ?, '{"tokens":{"output":2}}')`, start.Add(time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 3, 'session.step.failed.1', '{"assistantMessageID":"new","tokens":{"output":3,"reasoning":1}}')`)
	execSQL(t, path, `INSERT INTO event VALUES ('s', 4, 'session.step.ended.1', '{"assistantMessageID":"new","tokens":{"output":2}}')`)
	if got := w.Poll(); got.Output != 11 || got.Thinking != 1 {
		t.Fatalf("untimed retries = %+v, want output 11 and thinking 1", got)
	}
}

func TestOpenCodeV2BeforeSessionTableRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT, fork_session_id TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, type TEXT, created INTEGER, data TEXT)`)
	start := time.Now().Truncate(time.Millisecond)
	execSQL(t, path, `INSERT INTO session VALUES ('s', '/work', NULL, ?)`, start.UnixMilli())
	execSQL(t, path, `INSERT INTO session VALUES ('fork', '/work', 's', ?)`, start.Add(time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO message VALUES ('legacy', 's', ?, '{"role":"assistant","tokens":{"output":999}}')`, start.Add(time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO session_message VALUES ('copy', 'fork', 'assistant', ?, '{"tokens":{"output":999}}')`, start.UnixMilli())
	execSQL(t, path, `INSERT INTO session_message VALUES ('step', 's', 'assistant', ?, ?)`, start.Add(-time.Second).UnixMilli(), fmt.Sprintf(`{"time":{"completed":%d},"tokens":{"input":12,"output":34,"reasoning":5}}`, start.Add(time.Second).UnixMilli()))
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', ?, '{"source":"title","tokens":{"input":3,"output":7,"reasoning":2}}')`, start.Add(time.Second).UnixMilli())
	v, ok := (openCodeDBSource{path: path}).read([]string{"/work"}, start)
	if !ok || v.input != 15 || v.output != 48 || v.thinking != 7 {
		t.Fatalf("read = %+v, %v; want input 15, output 48, thinking 7", v, ok)
	}
}

func TestOpenCodeV2BeforeEventTimestamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, type TEXT, data TEXT)`)
	start := time.Now().Truncate(time.Millisecond)
	execSQL(t, path, `INSERT INTO session VALUES ('s', '/work', ?)`, start.UnixMilli())
	execSQL(t, path, `INSERT INTO session_message VALUES ('step', 's', 'assistant', ?, '{"tokens":{"output":34}}')`, start.Add(time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', '{"source":"title","tokens":{"output":7}}')`)
	source := openCodeDBSource{path: path}
	if v, ok := source.read([]string{"/work"}, start); !ok || v.output != 34 {
		t.Fatalf("bounded read = %+v, %v; want 34 without untimed events", v, ok)
	}
	if v, ok := source.read([]string{"/work"}, time.Time{}); !ok || v.output != 41 {
		t.Fatalf("unbounded read = %+v, %v; want 41", v, ok)
	}
}

func TestOpenCodeV2RenamedSessionWithoutForkColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, type TEXT, created INTEGER, data TEXT)`)
	start := time.Now().Truncate(time.Millisecond)
	execSQL(t, path, `INSERT INTO session_v2 VALUES ('s', '/work', ?)`, start.UnixMilli())
	execSQL(t, path, `INSERT INTO session_message VALUES ('m', 's', 'assistant', ?, '{"tokens":{"output":10,"reasoning":2}}')`, start.Add(time.Second).UnixMilli())
	v, ok := (openCodeDBSource{path: path}).read([]string{"/work"}, start)
	if !ok || v.output != 12 || v.thinking != 2 {
		t.Fatalf("read = %+v, %v; want output 12, thinking 2", v, ok)
	}
}

func TestOpenCodeV2UntimedEventsCountOnlyGrowth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, type TEXT, data TEXT)`)
	start := time.Now().Truncate(time.Millisecond)
	execSQL(t, path, `INSERT INTO session VALUES ('s', '/work', ?)`, start.UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', '{"source":"title","tokens":{"input":100,"output":200,"reasoning":30}}')`)
	t.Setenv(OpenCodeDBEnv, path)
	if !EnableOpenCodeDB(true) {
		t.Fatal("SQLite source unavailable")
	}
	t.Cleanup(func() { EnableOpenCodeDB(false) })
	w := Watch("opencode", "/work", start)
	if w == nil {
		t.Fatal("OpenCode source unavailable")
	}
	execSQL(t, path, `INSERT INTO session_message VALUES ('step', 's', 'assistant', ?, '{"tokens":{"input":12,"output":34,"reasoning":5}}')`, start.Add(time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', '{"source":"compaction","tokens":{"input":3,"output":7,"reasoning":2}}')`)
	execSQL(t, path, `INSERT INTO session VALUES ('new', '/work', ?)`, start.Add(time.Second).UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('new', 'session.usage.recorded.1', '{"source":"title","tokens":{"input":1,"output":4,"reasoning":1}}')`)
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', 'invalid')`)
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', '{"source":"title","tokens":{"input":1e30,"output":1e30,"reasoning":1e30}}')`)
	execSQL(t, path, `INSERT INTO session VALUES ('other', '/elsewhere', ?)`, start.UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('other', 'session.usage.recorded.1', '{"source":"title","tokens":{"output":999}}')`)
	s := w.Poll()
	if s.Input != 16 || s.Output != 53 || s.Thinking != 8 {
		t.Fatalf("sample = %+v; want input 16, output 53, thinking 8", s)
	}
	if again := w.Poll(); again.Input != 16 || again.Output != 53 || again.Thinking != 8 {
		t.Fatalf("repeat sample = %+v", again)
	}
	execSQL(t, path, `ALTER TABLE event RENAME TO unavailable_event`)
	if failed := w.Poll(); failed.Input != 16 || failed.Output != 53 || failed.Thinking != 8 {
		t.Fatalf("auxiliary read failure changed the last sample: %+v", failed)
	}
	execSQL(t, path, `ALTER TABLE unavailable_event RENAME TO event`)
	if recovered := w.Poll(); recovered.Input != 16 || recovered.Output != 53 || recovered.Thinking != 8 {
		t.Fatalf("recovered sample changed without new usage: %+v", recovered)
	}
	all := Watch("opencode", "/work", time.Time{}).Poll()
	if all.Input != 116 || all.Output != 283 || all.Thinking != 38 {
		t.Fatalf("unbounded sample = %+v; want all history counted once", all)
	}
	execSQL(t, path, `DELETE FROM event WHERE aggregate_id IN ('s', 'new')`)
	if cleared := w.Poll(); cleared.Input != 12 || cleared.Output != 39 || cleared.Thinking != 5 {
		t.Fatalf("successful empty auxiliary read was not applied: %+v", cleared)
	}
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', '{"source":"title","tokens":{"output":7}}')`)
	if next := w.Poll(); next.Output != 46 {
		t.Fatalf("growth after auxiliary history was cleared = %+v, want output 46", next)
	}
}

func TestOpenCodeV2UntimedEventsWaitForBaseline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	t.Setenv(OpenCodeDBEnv, path)
	if !EnableOpenCodeDB(true) {
		t.Fatal("SQLite source unavailable")
	}
	t.Cleanup(func() { EnableOpenCodeDB(false) })
	w := Watch("opencode", "/work", time.Now())
	execSQL(t, path, `CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, type TEXT, data TEXT)`)
	execSQL(t, path, `INSERT INTO session VALUES ('s', '/work', 1)`)
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', '{"source":"title","tokens":{"output":200}}')`)
	if s := w.Poll(); s.Output != 0 {
		t.Fatalf("history credited before a baseline: %+v", s)
	}
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', '{"source":"title","tokens":{"output":7}}')`)
	if s := w.Poll(); s.Output != 7 {
		t.Fatalf("growth after baseline = %+v, want 7", s)
	}
}

func TestOpenCodeV2MigrationPreservesUntimedGrowth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT, fork_session_id TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, type TEXT, data TEXT)`)
	start := time.Now().Truncate(time.Millisecond)
	execSQL(t, path, `INSERT INTO session VALUES ('s', '/work', NULL, ?)`, start.UnixMilli())
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', '{"source":"title","tokens":{"output":100}}')`)
	t.Setenv(OpenCodeDBEnv, path)
	if !EnableOpenCodeDB(true) {
		t.Fatal("SQLite source unavailable")
	}
	t.Cleanup(func() { EnableOpenCodeDB(false) })
	w := Watch("opencode", "/work", start)
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', '{"source":"compaction","tokens":{"output":7,"reasoning":2}}')`)
	if got := w.Poll(); got.Output != 9 || got.Thinking != 2 {
		t.Fatalf("before migration = %+v; want 9 output, 2 thinking", got)
	}
	execSQL(t, path, `ALTER TABLE session RENAME TO session_v2`)
	if got := w.Poll(); got.Output != 9 || got.Thinking != 2 {
		t.Fatalf("after table rename = %+v; want 9 output, 2 thinking", got)
	}
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', '{"source":"title","tokens":{"output":3}}')`)
	if got := w.Poll(); got.Output != 12 || got.Thinking != 2 {
		t.Fatalf("untimed growth after table rename = %+v; want 12 output, 2 thinking", got)
	}
	if all, ok := (openCodeDBSource{path: path}).read([]string{"/work"}, time.Time{}); !ok || all.output != 112 || all.thinking != 2 {
		t.Fatalf("unbounded read after table rename = %+v, %v", all, ok)
	}
	execSQL(t, path, `ALTER TABLE event ADD COLUMN created INTEGER NOT NULL DEFAULT 0`)
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', '{"source":"title","tokens":{"output":4,"reasoning":1}}', ?)`, start.Add(time.Second).UnixMilli())
	for range 2 {
		if got := w.Poll(); got.Output != 17 || got.Thinking != 3 {
			t.Fatalf("after migration = %+v; want 17 output, 3 thinking", got)
		}
	}
}

func TestOpenCodeV2UsageSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `PRAGMA journal_mode=WAL`)
	execSQL(t, path, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT, fork_session_id TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, type TEXT, created INTEGER, data TEXT)`)
	execSQL(t, path, `INSERT INTO session_v2 VALUES ('s', '/work', NULL, 1)`)
	execSQL(t, path, `INSERT INTO session_message VALUES ('m', 's', 'assistant', 1, '{"tokens":{"output":0}}')`)
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', 1, '{"source":"title","tokens":{"output":0}}')`)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA synchronous=NORMAL`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for i := 1; i <= 300; i++ {
			tx, err := db.Begin()
			if err != nil {
				done <- err
				return
			}
			_, err = tx.Exec(`UPDATE session_message SET data = ?`, fmt.Sprintf(`{"tokens":{"output":%d}}`, i))
			if err == nil {
				_, err = tx.Exec(`UPDATE event SET data = ?`, fmt.Sprintf(`{"source":"title","tokens":{"output":%d}}`, i))
			}
			if err != nil {
				tx.Rollback()
				done <- err
				return
			}
			if err = tx.Commit(); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for range 300 {
		v, ok := (openCodeDBSource{path: path}).read([]string{"/work"}, time.Time{})
		if !ok || v.output%2 != 0 {
			<-done
			t.Fatalf("inconsistent snapshot = %+v, %v", v, ok)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeV2WatcherUsesOneSnapshotForUntimedEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `PRAGMA journal_mode=WAL`)
	execSQL(t, path, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT, fork_session_id TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, type TEXT, created INTEGER, data TEXT)`)
	execSQL(t, path, `INSERT INTO session_v2 VALUES ('s', '/work', NULL, 1)`)
	execSQL(t, path, `INSERT INTO session_message VALUES ('m', 's', 'assistant', 2, '{"tokens":{"output":0}}')`)
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', 0, '{"source":"title","tokens":{"output":0}}')`)
	t.Setenv(OpenCodeDBEnv, path)
	if !EnableOpenCodeDB(true) {
		t.Fatal("SQLite source unavailable")
	}
	t.Cleanup(func() { EnableOpenCodeDB(false) })
	w := Watch("opencode", "/work", time.UnixMilli(1))
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	done := make(chan error, 1)
	go func() {
		for i := 1; i <= 300; i++ {
			tx, err := db.Begin()
			if err != nil {
				done <- err
				return
			}
			_, err = tx.Exec(`UPDATE session_message SET data = ?`, fmt.Sprintf(`{"tokens":{"output":%d}}`, i))
			if err == nil {
				_, err = tx.Exec(`UPDATE event SET data = ?`, fmt.Sprintf(`{"source":"title","tokens":{"output":%d}}`, i))
			}
			if err != nil {
				tx.Rollback()
				done <- err
				return
			}
			if err = tx.Commit(); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for range 300 {
		if sample := w.Poll(); sample.Output%2 != 0 {
			<-done
			t.Fatalf("inconsistent watcher sample = %+v", sample)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if sample := w.Poll(); sample.Output != 600 {
		t.Fatalf("final watcher sample = %+v; want 600 output tokens", sample)
	}
}

func TestOpenCodeV2UsageSaturatesCombinedCounters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT, fork_session_id TEXT, time_created INTEGER)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, type TEXT, created INTEGER, data TEXT)`)
	execSQL(t, path, `INSERT INTO session_v2 VALUES ('s', '/work', NULL, 1)`)
	n := int64(maxSaneTokensInt())/2 + 1
	tokens := fmt.Sprintf(`{"output":%d,"input":%d,"reasoning":%d}`, n, n, n)
	execSQL(t, path, `INSERT INTO session_message VALUES ('m', 's', 'assistant', 2, ?)`, `{"tokens":`+tokens+`}`)
	execSQL(t, path, `INSERT INTO event VALUES ('s', 'session.usage.recorded.1', 2, ?)`, `{"source":"title","tokens":`+tokens+`}`)
	v, ok := (openCodeDBSource{path: path}).read([]string{"/work"}, time.Time{})
	if !ok || v.output != maxSaneTokensInt() || v.input != maxSaneTokensInt() || v.thinking != maxSaneTokensInt() {
		t.Fatalf("combined usage = %+v, %v; want saturated counters", v, ok)
	}
}

// Reasoning without billed output is still a reading: Sample.Empty says so,
// and an agent that thinks before it writes must show a rate rather than
// nothing until the first completion token lands.
func TestOpenCodeDBCountsThinkingOnly(t *testing.T) {
	path := opencodeDB(t)
	work := t.TempDir()
	addSession(t, path, "s1", work)
	start := time.Now()
	addMessage(t, path, "m1", "s1", start.Add(time.Second),
		`{"role":"assistant","tokens":{"reasoning":52}}`)
	withOpenCodeDB(t, path)
	w := Watch("opencode", work, start)
	if w == nil {
		t.Fatal("opencode should be readable once the database is enabled")
	}
	w.poll(nil)
	s := w.Sample()
	if s.Empty() {
		t.Fatal("thinking-only message reported as nothing")
	}
	if s.Thinking != 52 {
		t.Fatalf("thinking tokens %d, want 52", s.Thinking)
	}
	if s.Output != 0 || s.Input != 0 {
		t.Fatalf("invented billed tokens: %+v", s)
	}
}

// A SUM past maxSaneTokens is a misread, not a measurement.
func TestOpenCodeDBDropsAbsurdCounts(t *testing.T) {
	path := opencodeDB(t)
	work := t.TempDir()
	addSession(t, path, "s1", work)
	start := time.Now()
	addMessage(t, path, "m1", "s1", start.Add(time.Second),
		`{"role":"assistant","tokens":{"output":1099511627777,"reasoning":1099511627777,"total":1099511627777}}`)
	withOpenCodeDB(t, path)
	w := Watch("opencode", work, start)
	if w == nil {
		t.Fatal("opencode should be readable once the database is enabled")
	}
	w.poll(nil)
	if got := w.Sample(); !got.Empty() {
		t.Fatalf("absurd counts reported as usage: %+v", got)
	}
}

// An absurd counter beside a real one must not fail the statement. SQLite's
// integer sum() raises "integer overflow" where a wrapped sum would answer,
// and the read then failed for as long as the row survived: the agent read as
// permanently idle, and the real message beside it was never counted.
func TestOpenCodeDBSumsPastAbsurdCounter(t *testing.T) {
	path := opencodeDB(t)
	work := t.TempDir()
	addSession(t, path, "s1", work)
	start := time.Now()
	addMessage(t, path, "m1", "s1", start.Add(time.Second),
		`{"role":"assistant","tokens":{"output":1e30}}`)
	addMessage(t, path, "m2", "s1", start.Add(2*time.Second),
		`{"role":"assistant","tokens":{"output":120,"input":40}}`)
	withOpenCodeDB(t, path)
	w := Watch("opencode", work, start)
	if w == nil {
		t.Fatal("opencode should be readable once the database is enabled")
	}
	w.poll(nil)
	if got := w.Sample(); got.Empty() {
		t.Fatal("the read failed, so the real message beside the absurd one was not counted")
	}
}

// A payload that is not well-formed JSON reads as absent. json_extract raises
// on one, and the store belongs to another program that toktop only reads, so
// an unguarded extraction would fail the whole statement and report nothing
// for this agent for as long as the row is there.
func TestOpenCodeDBSkipsMalformedPayloads(t *testing.T) {
	path := opencodeDB(t)
	work := t.TempDir()
	addSession(t, path, "s1", work)
	start := time.Now()
	addMessage(t, path, "m1", "s1", start.Add(time.Second),
		`{"role":"assistant","tokens":{"output":324}}`)
	addMessage(t, path, "m2", "s1", start.Add(2*time.Second), `{"role":"assist`)
	withOpenCodeDB(t, path)
	w := Watch("opencode", work, start)
	if w == nil {
		t.Fatal("opencode should be readable once the database is enabled")
	}
	w.poll(nil)
	if got := w.Sample().Output; got != 324 {
		t.Fatalf("output %d, want the 324 from the readable row next to a malformed one", got)
	}
}

// Every directory value still travels as a bound parameter, so a path is never
// spliced into the statement.
func TestUsageQueryForPlaceholderCount(t *testing.T) {
	orig := foldSessionDirectory.Load()
	t.Cleanup(func() { foldSessionDirectory.Store(orig) })

	foldSessionDirectory.Store(false)
	if n := strings.Count(usageQueryFor(2), "?"); n != 3 {
		t.Errorf("unfolded usageQueryFor(2) has %d placeholders, want 3 (2 dirs + time)", n)
	}
	foldSessionDirectory.Store(true)
	if n := strings.Count(usageQueryFor(2), "?"); n != 5 {
		t.Errorf("folded usageQueryFor(2) has %d placeholders, want 5 (2 dirs twice + time)", n)
	}
}

func TestFoldSessionDirectoryFollowsTheFilesystem(t *testing.T) {
	want := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	if got := foldSessionDirectory.Load(); got != want {
		t.Fatalf("foldSessionDirectory = %v, want %v on %s", got, want, runtime.GOOS)
	}
}

// The folding flag decides both how many placeholders the statement carries
// and how many directory values are bound to it. Building the two from
// separate reads of the flag splices a directory where the timestamp belongs,
// which reads another directory's usage as this one's, so the two must agree
// for every spelling count and either setting.
func TestUsageQueryPlaceholdersMatchBoundDirectories(t *testing.T) {
	dirs := []string{"/work", "/work/Équipe", "/work/sub"}
	for _, fold := range []bool{false, true} {
		for n := 1; n <= len(dirs); n++ {
			query := usageQuery(n, fold, true)
			// The trailing bound value is the since timestamp.
			want := len(dirArgs(dirs[:n], fold)) + 1
			if got := strings.Count(query, "?"); got != want {
				t.Errorf("usageQuery(%d, fold=%v) has %d placeholders, want %d",
					n, fold, got, want)
			}
			// A zero since means no lower bound, so the statement carries
			// no timestamp placeholder to bind an argument to.
			unbounded := usageQuery(n, fold, false)
			if got, want := strings.Count(unbounded, "?"), len(dirArgs(dirs[:n], fold)); got != want {
				t.Errorf("unbounded usageQuery(%d, fold=%v) has %d placeholders, want %d",
					n, fold, got, want)
			}
		}
	}
}

// A session recorded under a folded spelling of the watched directory matches
// under the production GOOS default and with folding forced on, so a
// regression that only folds in tests cannot hide. Linux compares bytes and
// skips the default case.
func TestOpenCodeDBMatchesFoldedDirectorySpellings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T)
	}{
		{"folding forced on", func(t *testing.T) {
			orig := foldSessionDirectory.Load()
			foldSessionDirectory.Store(true)
			t.Cleanup(func() { foldSessionDirectory.Store(orig) })
		}},
		{"production GOOS default", func(t *testing.T) {
			if !foldSessionDirectory.Load() {
				t.Skip("this OS compares session directories by bytes")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)

			path := opencodeDB(t)
			work := t.TempDir()
			stored := strings.ToUpper(filepath.ToSlash(work))
			if stored == work {
				t.Skip("temp dir has no case or slash to fold")
			}
			addSession(t, path, "s-mine", stored)
			start := time.Now()
			addMessage(t, path, "m1", "s-mine", start.Add(time.Second),
				`{"role":"assistant","tokens":{"output":42}}`)
			withOpenCodeDB(t, path)

			w := Watch("opencode", work, start)
			if w == nil {
				t.Fatal("the source is registered, so a watcher is expected")
			}
			w.poll(nil)
			if got := w.Sample().Output; got != 42 {
				t.Fatalf("output tokens %d, want 42: folded directory spelling was not matched", got)
			}
		})
	}
}

// A non-ASCII case difference must survive folding too: on an
// case-insensitive filesystem an agent can record /work/Équipe while the
// watcher resolves /work/équipe. Go's strings.ToLower folds É, SQLite's
// lower() does not, so the stored spelling is folded before it is bound.
func TestOpenCodeDBFoldsNonASCIICaseDifferences(t *testing.T) {
	orig := foldSessionDirectory.Load()
	foldSessionDirectory.Store(true)
	t.Cleanup(func() { foldSessionDirectory.Store(orig) })

	path := opencodeDB(t)
	base := t.TempDir()
	watched := filepath.ToSlash(filepath.Join(base, "équipe"))
	stored := filepath.ToSlash(filepath.Join(base, "Équipe"))
	if !strings.EqualFold(watched, stored) || watched == stored {
		t.Skipf("no foldable non-ASCII pair (%q vs %q)", watched, stored)
	}
	addSession(t, path, "s-mine", stored)
	start := time.Now()
	addMessage(t, path, "m1", "s-mine", start.Add(time.Second),
		`{"role":"assistant","tokens":{"output":42}}`)
	withOpenCodeDB(t, path)

	w := Watch("opencode", watched, start)
	if w == nil {
		t.Fatal("the source is registered, so a watcher is expected")
	}
	w.poll(nil)
	if got := w.Sample().Output; got != 42 {
		t.Fatalf("output tokens %d, want 42: non-ASCII case spelling was not matched", got)
	}
}

func TestOpenCodeDBWithoutAStoreReportsNothing(t *testing.T) {
	withOpenCodeDB(t, filepath.Join(t.TempDir(), "absent.db"))
	w := Watch("opencode", t.TempDir(), time.Now())
	if w == nil {
		t.Fatal("the source is registered, so a watcher is expected")
	}
	w.poll(nil)
	if got := w.Sample(); !got.Empty() {
		t.Fatalf("invented usage from a missing database: %+v", got)
	}
}

func TestReadOnlyDSNEscapesURISyntax(t *testing.T) {
	q := fmt.Sprintf("?mode=ro&_query_only=1&_busy_timeout=%d&_defensive=1&_dqs=0&_pragma=trusted_schema=OFF", dbBusyTimeout.Milliseconds())
	for _, tc := range []struct{ path, want string }{
		{"/home/user/.local/share/opencode/opencode.db",
			"file:/home/user/.local/share/opencode/opencode.db" + q},
		{"/home/mar{c}o#witz/db", "file:/home/mar{c}o%23witz/db" + q},
		{"/tmp/what?now/db", "file:/tmp/what%3Fnow/db" + q},
		{"/tmp/100%20done/db", "file:/tmp/100%2520done/db" + q},
		{`C:\Users\mw\.local\share\opencode.db`, `file:C:\Users\mw\.local\share\opencode.db` + q},
	} {
		if got := readOnlyDSN(tc.path); got != tc.want {
			t.Errorf("readOnlyDSN(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// A live session store is opened so it cannot be written: query_only rejects
// INSERT, the pool is a single connection, and a planted schema cannot run
// extra SQL through views, triggers, or double-quoted string literals.
func TestOpenReadOnlyRejectsWrites(t *testing.T) {
	path := opencodeDB(t)
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var q int
	if err := db.QueryRow("PRAGMA query_only").Scan(&q); err != nil || q != 1 {
		t.Fatalf("PRAGMA query_only = %d (%v), want 1", q, err)
	}
	if _, err := db.Exec(`INSERT INTO session (id, directory) VALUES ('x', '/tmp')`); err == nil {
		t.Fatal("query_only allowed a write")
	}
	var busy int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil || busy != int(dbBusyTimeout.Milliseconds()) {
		t.Fatalf("PRAGMA busy_timeout = %d (%v), want %d", busy, err, dbBusyTimeout.Milliseconds())
	}
	var trusted int
	if err := db.QueryRow("PRAGMA trusted_schema").Scan(&trusted); err != nil || trusted != 0 {
		t.Fatalf("PRAGMA trusted_schema = %d (%v), want 0", trusted, err)
	}
	var quoted string
	if err := db.QueryRow(`SELECT "not_a_column"`).Scan(&quoted); err == nil {
		t.Fatal("dqs treated a double-quoted identifier as a string literal")
	}
}

// A home directory may contain characters that carry URI syntax in a SQLite
// file: DSN; those paths must still open and read as themselves.
func TestOpenCodeDBReadsPathWithURISyntaxCharacters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "50%.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	createOpenCodeSchema(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	addSession(t, path, "s", dir)
	start := time.Now()
	addMessage(t, path, "m1", "s", start.Add(time.Second),
		`{"role":"assistant","tokens":{"output":42}}`)
	withOpenCodeDB(t, path)

	w := Watch("opencode", dir, start)
	if w == nil {
		t.Fatal("the source is registered, so a watcher is expected")
	}
	w.poll(nil)
	if got := w.Sample().Output; got != 42 {
		t.Fatalf("output tokens %d, want 42: the URI-syntax path was misread", got)
	}
}

// Reasoning without billed output is still a reading: Sample.Empty treats
// thinking-only samples as present, and file adapters already count them.
func TestOpenCodeDBThinkingOnlyIsPresent(t *testing.T) {
	path := opencodeDB(t)
	dir := t.TempDir()
	addSession(t, path, "s", dir)
	start := time.Now()
	addMessage(t, path, "m1", "s", start.Add(time.Second),
		`{"role":"assistant","tokens":{"reasoning":12}}`)
	withOpenCodeDB(t, path)

	w := Watch("opencode", dir, start)
	if w == nil {
		t.Fatal("opencode should be readable once the database is enabled")
	}
	w.poll(nil)
	s := w.Sample()
	if s.Thinking != 12 || s.Empty() {
		t.Fatalf("thinking-only sample dropped: %+v", s)
	}
}

// Tokens live on assistant messages. A user prompt that carries the same
// JSON shape must not be summed in, or a watcher invents output.
func TestOpenCodeDBIgnoresNonAssistantTokens(t *testing.T) {
	path := opencodeDB(t)
	dir := t.TempDir()
	addSession(t, path, "s", dir)
	start := time.Now()
	addMessage(t, path, "m-user", "s", start.Add(time.Second),
		`{"role":"user","tokens":{"output":9999,"reasoning":50,"total":9999}}`)
	addMessage(t, path, "m-as", "s", start.Add(2*time.Second),
		`{"role":"assistant","tokens":{"output":12,"reasoning":3,"total":100}}`)
	withOpenCodeDB(t, path)

	w := Watch("opencode", dir, start)
	w.poll(nil)
	s := w.Sample()
	if s.Output != 12 || s.Thinking != 3 || s.Total != 100 {
		t.Fatalf("read %+v, want only the assistant message", s)
	}
}

// json_extract of a JSON string is TEXT, and SQLite ranks TEXT above INTEGER,
// so MAX without CAST would pick "9" over 100.
func TestOpenCodeDBMaxTotalIsNumeric(t *testing.T) {
	path := opencodeDB(t)
	dir := t.TempDir()
	addSession(t, path, "s", dir)
	start := time.Now()
	addMessage(t, path, "m-str", "s", start.Add(time.Second),
		`{"role":"assistant","tokens":{"total":"9"}}`)
	addMessage(t, path, "m-num", "s", start.Add(2*time.Second),
		`{"role":"assistant","tokens":{"total":100}}`)
	withOpenCodeDB(t, path)

	w := Watch("opencode", dir, start)
	if w == nil {
		t.Fatal("the source is registered, so a watcher is expected")
	}
	w.poll(nil)
	if got := w.Sample().Total; got != 100 {
		t.Fatalf("total %d, want 100: MAX compared token totals as text", got)
	}
}

// A counter larger than any real usage is corruption, not a measurement.
func TestOpenCodeDBRejectsAbsurdCounts(t *testing.T) {
	path := opencodeDB(t)
	dir := t.TempDir()
	addSession(t, path, "s", dir)
	start := time.Now()
	addMessage(t, path, "m1", "s", start.Add(time.Second),
		fmt.Sprintf(`{"role":"assistant","tokens":{"output":%d,"total":%d}}`, maxSaneTokens+1, maxSaneTokens+1))
	withOpenCodeDB(t, path)

	w := Watch("opencode", dir, start)
	w.poll(nil)
	if got := w.Sample(); !got.Empty() {
		t.Fatalf("invented usage from an absurd counter: %+v", got)
	}
}

func TestOpenCodeDBNegativeCounters(t *testing.T) {
	for _, valid := range []bool{false, true} {
		t.Run(fmt.Sprintf("valid_message_%t", valid), func(t *testing.T) {
			path := opencodeDB(t)
			dir := t.TempDir()
			start := time.Now()
			addSession(t, path, "s", dir)
			addMessage(t, path, "negative", "s", start.Add(time.Second),
				`{"role":"assistant","tokens":{"output":-10,"reasoning":-10,"input":-10}}`)
			want := 0
			if valid {
				addMessage(t, path, "valid", "s", start.Add(time.Second),
					`{"role":"assistant","tokens":{"output":42,"reasoning":42,"input":42}}`)
				want = 42
			}
			withOpenCodeDB(t, path)
			w := Watch("opencode", dir, start)
			got := w.Poll()
			if got.Output != want || got.Thinking != want || got.Input != want {
				t.Fatalf("usage = %+v, want each counter %d", got, want)
			}
		})
	}
}

// tokens.total is the context the model read, so it cannot be smaller than
// the output the same reading reports. counter already refuses a negative
// one; the floor against the output is what the JSONL adapters get from
// foldCounters and this reader, whose total is a MAX in SQL, did not have.
// Without it a store holding `{"output":100,"total":5}` publishes a context
// window of 5 beside an output of 100, and every consumer of the sample
// reads the pair as a measurement.
func TestOpenCodeDBTotalIsFlooredAtOutput(t *testing.T) {
	for _, c := range []struct {
		name    string
		payload string
		want    int
	}{
		{"negative", `{"role":"assistant","tokens":{"output":100,"total":-5}}`, 100},
		{"below output", `{"role":"assistant","tokens":{"output":100,"total":5}}`, 100},
		{"zero beside output", `{"role":"assistant","tokens":{"output":100,"total":0}}`, 100},
		{"absent beside output", `{"role":"assistant","tokens":{"output":100}}`, 100},
		{"real context", `{"role":"assistant","tokens":{"output":100,"total":900}}`, 900},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := opencodeDB(t)
			dir := t.TempDir()
			start := time.Now()
			addSession(t, path, "s", dir)
			addMessage(t, path, "m", "s", start.Add(time.Second), c.payload)
			withOpenCodeDB(t, path)

			w := Watch("opencode", dir, start)
			got := w.Poll()
			if got.Total != c.want {
				t.Fatalf("total %d, want %d from %s", got.Total, c.want, c.payload)
			}
			if got.Total < 0 {
				t.Fatalf("total %d is negative", got.Total)
			}
			if got.Output > 0 && got.Total < got.Output {
				t.Fatalf("total %d below output %d: %+v", got.Total, got.Output, got)
			}
		})
	}
}

// The floor is on the reading, not on any row: a message carrying no total
// at all does not zero a context another message reported, and a message
// carrying a small one does not lower it. The statement picks the largest
// total before the floor runs, so this is the whole guarantee.
func TestOpenCodeDBTotalFloorDoesNotOverrideTheLargestTotal(t *testing.T) {
	path := opencodeDB(t)
	dir := t.TempDir()
	start := time.Now()
	addSession(t, path, "s", dir)
	addMessage(t, path, "m-big-out", "s", start.Add(time.Second),
		`{"role":"assistant","tokens":{"output":9000}}`)
	addMessage(t, path, "m-real-ctx", "s", start.Add(2*time.Second),
		`{"role":"assistant","tokens":{"output":1,"total":50000}}`)
	withOpenCodeDB(t, path)

	w := Watch("opencode", dir, start)
	got := w.Poll()
	if got.Total != 50000 {
		t.Fatalf("total %d, want the 50000 the store reported", got.Total)
	}
	if got.Output != 9001 {
		t.Fatalf("output %d, want 9001", got.Output)
	}
}

func TestEnableOpenCodeDBReportsAvailability(t *testing.T) {
	if !EnableOpenCodeDB(false) {
		t.Fatal("a build with -tags sqlite can read the database")
	}
	if Supported("opencode") {
		t.Fatal("disabled means unreadable")
	}
	if !EnableOpenCodeDB(true) {
		t.Fatal("enabling should report the same availability")
	}
	t.Cleanup(func() { EnableOpenCodeDB(false) })
	if !Supported("opencode") {
		t.Fatal("enabled means readable")
	}
}

// An agent records the directory it was started in, which on macOS is a
// symlink to the one filepath.EvalSymlinks reports. Both spellings must count.
func TestOpenCodeDBMatchesUnresolvedDirectories(t *testing.T) {
	path := opencodeDB(t)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	addSession(t, path, "s", link) // the session names the path as given
	withOpenCodeDB(t, path)

	start := time.Now()
	w := Watch("opencode", link, start)
	addMessage(t, path, "m1", "s", start.Add(time.Second),
		`{"role":"assistant","tokens":{"output":42}}`)
	w.poll(nil)
	if got := w.Sample().Output; got != 42 {
		t.Fatalf("output tokens %d, want 42: the unresolved spelling was missed", got)
	}
}

func explainQueryPlan(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestOpenCodeV2RetryQueryUsesEventTypeIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, seq INTEGER, type TEXT, created INTEGER, data TEXT)`)
	execSQL(t, path, `CREATE INDEX event_aggregate_type_seq_idx ON event (aggregate_id, type, seq)`)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	query := `SELECT COUNT(*) FROM session_v2 session CROSS JOIN event m
		WHERE m.aggregate_id = session.id AND session.directory = ? AND m.created > ? AND ` + openCodeEventPredicate(true)
	plan := explainQueryPlan(t, db, query, "/work", 1)
	if !strings.Contains(plan, "event_aggregate_type_seq_idx (aggregate_id=? AND type=?)") {
		t.Fatalf("event lookup does not restrict the event type:\n%s", plan)
	}
	baseline := `SELECT COUNT(*) FROM session_v2 session CROSS JOIN event m
		WHERE m.aggregate_id = session.id AND session.directory = ? AND (m.created = 0 OR m.created = ?)
		AND m.type IN ('session.usage.recorded.1', 'session.step.failed.1') AND (` + openCodeEventPredicate(false) + ` OR m.type = 'session.step.failed.1')`
	plan = explainQueryPlan(t, db, baseline, "/work", 1)
	if !strings.Contains(plan, "event_aggregate_type_seq_idx (aggregate_id=? AND type=?)") {
		t.Fatalf("event baseline scans unrelated types:\n%s", plan)
	}
}

func TestOpenCodeV2MessageRetryChecksUseEventIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, seq INTEGER, type TEXT, data TEXT)`)
	execSQL(t, path, `CREATE INDEX event_aggregate_type_seq_idx ON event (aggregate_id, type, seq)`)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	query := `SELECT COUNT(*) FROM session_v2 session CROSS JOIN session_message m WHERE m.session_id = session.id AND ` + openCodeMessagePredicate()
	plan := explainQueryPlan(t, db, query)
	if strings.Count(plan, "event_aggregate_type_seq_idx") < 3 || strings.Contains(plan, "SCAN failed") || strings.Contains(plan, "SCAN started") || strings.Contains(plan, "SCAN later") {
		t.Fatalf("retry checks scan the event table:\n%s", plan)
	}
}

func TestOpenCodeV2BoundarySequenceUsesEventTypeIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execSQL(t, path, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT)`)
	execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, data TEXT)`)
	execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, seq INTEGER, type TEXT, data TEXT)`)
	execSQL(t, path, `CREATE INDEX event_aggregate_type_seq_idx ON event (aggregate_id, type, seq)`)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	query := `SELECT m.id, (SELECT MAX(e.seq) FROM event e WHERE e.aggregate_id = session.id
		AND e.type IN ('session.step.ended.1', 'session.step.failed.1')
		AND ` + strings.ReplaceAll(jsonToken("$.assistantMessageID"), "m.data", "e.data") + ` = m.id)
		FROM session_v2 session CROSS JOIN session_message m WHERE m.session_id = session.id`
	plan := explainQueryPlan(t, db, query)
	if !strings.Contains(plan, "event_aggregate_type_seq_idx (aggregate_id=? AND type=?)") {
		t.Fatalf("boundary sequence scans unrelated event types:\n%s", plan)
	}
}

// The usage query must walk session first, and must not scan message. The
// store is filled out because a plan is a cost estimate: on a handful of rows
// SQLite indexes a scan away, so a shape that only becomes expensive at scale
// reads as fine until the table is big enough to care. opencode's own message
// index leads with session_id, so a session-outer plan is the one that stays
// inside it; a message-outer plan reads every message on the box on a poll
// that runs four times a second.
func TestUsageQueryStartsAtSession(t *testing.T) {
	path := opencodeDB(t)
	start := time.Unix(1_700_000_000, 0)
	// One watched session among many, so a plan that reaches for the whole
	// message table has rows it could visibly have avoided.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for i := range 500 {
		dir := "/other"
		if i == 250 {
			dir = "/work"
		}
		if _, err := tx.Exec(`INSERT INTO session (id, directory) VALUES (?, ?)`, fmt.Sprintf("s-%d", i), dir); err != nil {
			t.Fatal(err)
		}
		for j := range 20 {
			if _, err := tx.Exec(
				`INSERT INTO message (id, session_id, time_created, data) VALUES (?, ?, ?, ?)`,
				fmt.Sprintf("m-%d-%d", i, j), fmt.Sprintf("s-%d", i), start.Add(time.Duration(j)*time.Second).UnixMilli(),
				`{"role":"assistant","tokens":{"output":1}}`); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("ANALYZE"); err != nil {
		t.Fatal(err)
	}

	orig := foldSessionDirectory.Load()
	foldSessionDirectory.Store(false)
	t.Cleanup(func() { foldSessionDirectory.Store(orig) })

	plan := explainQueryPlan(t, db, usageQueryFor(1), "/work", start.UnixMilli())
	first, _, _ := strings.Cut(plan, "\n")
	if !strings.Contains(strings.ToLower(first), "session") {
		t.Fatalf("outer loop is not session:\n%s", plan)
	}
	// A plan can name the index and still scan, and a scan there reads every
	// message in the store on every poll. Nothing in the query as written
	// produces that: SQLite reorders even a plain INNER JOIN into this shape,
	// so the check is a guard on the statement rather than a reproducer, and
	// it fires if a future edit drops the join term the index is chosen on.
	for _, step := range strings.Split(plan, "\n") {
		if strings.HasPrefix(step, "SCAN m") {
			t.Fatalf("message table is scanned, so the whole store is read per poll:\n%s", plan)
		}
	}
}

func TestOpenCodeDBPathResolvesRelativeXDGDataHomePerAgent(t *testing.T) {
	t.Setenv("OPENCODE_DB", "opencode.db")
	t.Setenv("HOME", t.TempDir())
	abs := t.TempDir()
	t.Setenv("XDG_DATA_HOME", abs)
	if got, want := openCodeDBPath(), filepath.Join(abs, "opencode", "opencode.db"); got != want {
		t.Fatalf("absolute XDG_DATA_HOME: got %q, want %q", got, want)
	}

	t.Setenv("XDG_DATA_HOME", "relative/data")
	got := openCodeDBPath()
	if want := filepath.Join("relative", "data", "opencode", "opencode.db"); got != want {
		t.Fatalf("relative XDG_DATA_HOME = %q, want %q", got, want)
	}
}

func TestOpenCodeRelativeDataHomeTracksEachAgentDirectory(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "relative/data")
	t.Setenv("OPENCODE_DB", "custom.db")
	if !EnableOpenCodeDB(true) {
		t.Fatal("SQLite source unavailable")
	}
	t.Cleanup(func() { EnableOpenCodeDB(false) })
	start := time.Now().Add(-time.Second)
	for i, want := range []int{17, 29} {
		dir := t.TempDir()
		path := filepath.Join(dir, "relative", "data", "opencode", "custom.db")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		execSQL(t, path, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT, time_created INTEGER)`)
		execSQL(t, path, `CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`)
		execSQL(t, path, `CREATE TABLE event (aggregate_id TEXT, type TEXT, created INTEGER, data TEXT)`)
		execSQL(t, path, `INSERT INTO session_v2 VALUES ('s', ?, ?)`, dir, start.UnixMilli())
		execSQL(t, path, `INSERT INTO session_message VALUES ('m', 's', 'assistant', ?, ?)`, time.Now().UnixMilli(), fmt.Sprintf(`{"tokens":{"output":%d}}`, want))
		if got := Watch("opencode", dir, start).Poll(); got.Output != want {
			t.Errorf("agent %d: output = %d, want %d", i, got.Output, want)
		}
	}
}

func TestOpenCodeDBPathOverride(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	for _, tt := range []struct{ name, want string }{
		{"custom.db", filepath.Join(data, "opencode", "custom.db")},
		{filepath.Join(data, "absolute.db"), filepath.Join(data, "absolute.db")},
		{":memory:", ""},
	} {
		t.Setenv("OPENCODE_DB", tt.name)
		if got := openCodeDBPath(); got != tt.want {
			t.Errorf("OPENCODE_DB=%q: got %q, want %q", tt.name, got, tt.want)
		}
	}
	t.Setenv("XDG_DATA_HOME", "relative")
	abs := filepath.Join(data, "absolute.db")
	t.Setenv(OpenCodeDBEnv, abs)
	if got := openCodeDBPath(); got != abs {
		t.Fatalf("absolute database with relative data home = %q, want %q", got, abs)
	}
}

func TestOpenCodeDBPathOverrideWithoutUsableHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("XDG_DATA_HOME", "")
	abs := filepath.Join(t.TempDir(), "custom.db")
	t.Setenv(OpenCodeDBEnv, abs)
	if got := openCodeDBPath(); got != abs {
		t.Fatalf("absolute database without home = %q, want %q", got, abs)
	}
	t.Setenv(OpenCodeDBEnv, "custom.db")
	if got := openCodeDBPath(); got != "" {
		t.Fatalf("relative database without a data root = %q, want no store", got)
	}
	t.Setenv("XDG_DATA_HOME", "relative")
	if got, want := openCodeDBPath(), filepath.Join("relative", "opencode", "custom.db"); got != want {
		t.Fatalf("relative database without home = %q, want %q", got, want)
	}
}

// The fold matches directories, and a directory is not a case mapping. Full
// folding renders U+00DF as "ss" and U+0130 as "i" plus a combining dot, so a
// stored "/work/straße" would match a watched "/work/strasse": two
// directories a case-insensitive volume keeps apart, with one checkout's
// tokens billed to the other. Simple folding keeps them apart and still
// matches the case differences the volume equates.
func TestFoldDirKeepsDirectoriesFullFoldingMerges(t *testing.T) {
	merged := [][2]string{
		{"/work/straße", "/work/strasse"},
		{"/work/i", "/work/İ"},
	}
	for _, pair := range merged {
		if got := foldDir(pair[1]); got == foldDir(pair[0]) {
			t.Errorf("foldDir folded %q onto %q (%q): distinct directories must not match", pair[1], pair[0], got)
		}
	}
	equated := [][2]string{
		{"/work/équipe", "/work/Équipe"},
		{"/work/straße", "/work/Straße"},
		{"cafe\u0301/x", "café/x"}, // NFD and NFC are one directory on macOS
	}
	for _, pair := range equated {
		if foldDir(pair[1]) != foldDir(pair[0]) {
			t.Errorf("foldDir(%q) = %q, want it to equal foldDir(%q) = %q", pair[1], foldDir(pair[1]), pair[0], foldDir(pair[0]))
		}
	}
}

// A zero since is the unattached reading: the whole store counts, including
// messages written before the process started. Binding the zero time's
// UnixMilli instead would ask for a bound of year 1, which every real message
// precedes, so the statement has to drop the lower bound instead of spelling
// it.
func TestOpenCodeDBZeroSinceReadsTheWholeStore(t *testing.T) {
	path := opencodeDB(t)
	work := t.TempDir()
	addSession(t, path, "s-mine", work)
	// Written long before any attach could name it.
	addMessage(t, path, "m0", "s-mine", time.Unix(1_000_000, 0).UTC(),
		`{"role":"assistant","tokens":{"output":9000,"reasoning":100,"total":50000}}`)
	withOpenCodeDB(t, path)

	w := Watch("opencode", work, time.Time{})
	if w == nil {
		t.Fatal("opencode should be readable once the database is enabled")
	}
	w.poll(nil)
	if got := w.Sample().Output; got != 9000 {
		t.Fatalf("output = %d, want the 9000 written before the process existed", got)
	}
}
