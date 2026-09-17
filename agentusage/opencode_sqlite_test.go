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
// session_id index the usage query is written against.
func createOpenCodeSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE TABLE session (id text PRIMARY KEY, directory text NOT NULL)`,
		`CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL,
			time_created integer NOT NULL, data text NOT NULL,
			FOREIGN KEY (session_id) REFERENCES session(id))`,
		`CREATE INDEX message_session_idx ON message (session_id)`,
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

// A missing database is an ordinary state (no opencode on this machine), not
// an error, and must never invent a number.
func TestUsageQueryForPlaceholderCount(t *testing.T) {
	orig := foldSessionDirectory
	t.Cleanup(func() { foldSessionDirectory = orig })

	foldSessionDirectory = false
	if n := strings.Count(usageQueryFor(2), "?"); n != 3 {
		t.Errorf("unfolded usageQueryFor(2) has %d placeholders, want 3 (2 dirs + time)", n)
	}
	foldSessionDirectory = true
	if n := strings.Count(usageQueryFor(2), "?"); n != 5 {
		t.Errorf("folded usageQueryFor(2) has %d placeholders, want 5 (2 dirs twice + time)", n)
	}
}

func TestFoldSessionDirectoryFollowsTheFilesystem(t *testing.T) {
	want := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	if foldSessionDirectory != want {
		t.Fatalf("foldSessionDirectory = %v, want %v on %s", foldSessionDirectory, want, runtime.GOOS)
	}
}

func TestOpenCodeDBMatchesFoldedDirectorySpellings(t *testing.T) {
	orig := foldSessionDirectory
	foldSessionDirectory = true
	t.Cleanup(func() { foldSessionDirectory = orig })

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
}

// Same matching as TestOpenCodeDBMatchesFoldedDirectorySpellings, but with
// the production GOOS default so a regression that only folds in tests cannot
// hide. Linux compares bytes and skips.
func TestOpenCodeDBFoldsDirectoriesOnThisOS(t *testing.T) {
	if !foldSessionDirectory {
		t.Skip("this OS compares session directories by bytes")
	}
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

// The usage query must walk session first. Starting at message would scan
// the whole table; opencode indexes session_id, not time_created.
func TestUsageQueryStartsAtSession(t *testing.T) {
	path := opencodeDB(t)
	addSession(t, path, "s-mine", "/work")
	start := time.Unix(1_700_000_000, 0)
	addMessage(t, path, "m-mine", "s-mine", start.Add(time.Second),
		`{"role":"assistant","tokens":{"output":1}}`)

	orig := foldSessionDirectory
	foldSessionDirectory = false
	t.Cleanup(func() { foldSessionDirectory = orig })

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	plan := explainQueryPlan(t, db, usageQueryFor(1), "/work", start.UnixMilli())
	first, _, _ := strings.Cut(plan, "\n")
	if !strings.Contains(strings.ToLower(first), "session") {
		t.Fatalf("outer loop is not session:\n%s", plan)
	}
}
