// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build sqlite

package agentwatch

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/maci0/toktop/agentusage"
)

func TestOpenCodeV2DirectoryChangeReportsBothProjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	t.Setenv("OPENCODE_DB", path)
	if !agentusage.EnableOpenCodeDB(true) {
		t.Fatal("SQLite source unavailable")
	}
	t.Cleanup(func() { agentusage.EnableOpenCodeDB(false) })
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, query := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT, fork_session_id TEXT, time_created INTEGER)`,
		`CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`,
		`CREATE TABLE event (aggregate_id TEXT, type TEXT, created INTEGER, data TEXT)`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	first, second := t.TempDir(), t.TempDir()
	process := agentusage.Process{PID: 101, Tool: "opencode", Dir: first, Started: time.Unix(100, 0)}
	rec := &idRecorder{}
	w := New(rec, nil)
	instant := time.Now()
	w.SetNow(func() time.Time { return instant })
	w.readEvery = time.Hour
	w.listAgents = func() []agentusage.Process { return []agentusage.Process{process} }
	defer w.stopAll()
	w.discover(t.Context())
	write := func(id, dir, data string) {
		t.Helper()
		stamp := time.Now().Add(time.Second).UnixMilli()
		if _, err := db.Exec(`INSERT INTO session_v2 VALUES (?, ?, NULL, ?)`, id, dir, stamp); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO session_message VALUES (?, ?, 'assistant', ?, ?)`, id, id, stamp, data); err != nil {
			t.Fatal(err)
		}
	}
	write("a", first, `{"tokens":{"output":20}}`)
	tr := trackerFor(t, w, process.PID)
	if tr == nil || tr.watch == nil {
		t.Fatal("first project is not watched")
	}
	w.report(tr, tr.watch.Poll())
	process.Dir = second
	w.discover(t.Context())
	write("b", second, `{"tokens":{"output":40}}`)
	tr = trackerFor(t, w, process.PID)
	if tr == nil || tr.watch == nil {
		t.Fatal("second project is not watched")
	}
	w.report(tr, tr.watch.Poll())
	var total int64
	for _, event := range rec.all() {
		total += event.OutputTokens
	}
	if total != 60 {
		t.Fatalf("reported %d output tokens, want 20 + 40", total)
	}
}

func TestOpenCodeV2ClientAndServerShareWatcher(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	t.Setenv("OPENCODE_DB", path)
	if !agentusage.EnableOpenCodeDB(true) {
		t.Fatal("SQLite source unavailable")
	}
	t.Cleanup(func() { agentusage.EnableOpenCodeDB(false) })
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, query := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT, fork_session_id TEXT, time_created INTEGER)`,
		`CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT)`,
		`CREATE TABLE event (aggregate_id TEXT, type TEXT, created INTEGER, data TEXT)`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	processes := []agentusage.Process{{PID: 101, Tool: "opencode", Dir: dir}, {PID: 102, Tool: "opencode", Dir: dir}}
	rec := &idRecorder{}
	w := New(rec, nil)
	w.readEvery = time.Hour
	w.listAgents = func() []agentusage.Process { return processes }
	defer w.stopAll()
	w.discover(t.Context())
	client, server := trackerFor(t, w, 101), trackerFor(t, w, 102)
	if client == nil || client.watch == nil || server == nil || server.watch != nil {
		t.Fatal("client and server did not share one watcher")
	}
	first := time.Now().Add(100 * time.Millisecond)
	if _, err := db.Exec(`INSERT INTO session_v2 VALUES ('s', ?, NULL, ?)`, dir, first.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO session_message VALUES ('a', 's', 'assistant', ?, '{"tokens":{"output":20}}')`, first.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	w.report(client, client.watch.Poll())
	time.Sleep(time.Until(first.Add(5 * time.Millisecond)))
	processes = processes[1:]
	w.discover(t.Context())
	server = trackerFor(t, w, 102)
	if server == nil || server.watch == nil {
		t.Fatal("server did not take over after client exited")
	}
	if _, err := db.Exec(`INSERT INTO session_message VALUES ('b', 's', 'assistant', ?, '{"tokens":{"output":40}}')`, time.Now().Add(time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	w.report(server, server.watch.Poll())
	var total int64
	for _, event := range rec.all() {
		total += event.OutputTokens
	}
	if total != 60 {
		t.Fatalf("reported %d output tokens, want 20 + 40", total)
	}
}
