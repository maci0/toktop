// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build sqlite

package agentusage

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// The two agent session stores toktop reads are SQLite databases another
// program writes, opened read-only and unconstrained. Nothing about their
// contents is checked on the way in: SQLite gives a column the storage class
// of whatever was written into it, so a column declared INTEGER can hold a
// string, a float or a blob, and the JSON a message row carries can be any
// bytes at all. A row that fails the statement costs every reading of the
// agent for as long as the row stands, and a row that decodes into an absurd
// count is summed straight into displayed throughput. These two harnesses
// fuzz the stores themselves rather than a fixture's shape, because that is
// where the untrusted input actually is.
//
// go test -fuzz accepts one []byte per argument, so a whole store travels as
// a single blob: rows are newline-separated and a row's four column values are
// NUL-separated. A field holding either separator is truncated at it, which is
// a value SQLite stores as text without it being a distinct case: a NUL is the
// boundary the Go driver cuts a TEXT value at anyway. The field bytes are
// bound as written rather than as the column's declared type, which is what
// puts a fuzzed string into a column the reader CASTs.

// fuzzRowsCap bounds how many rows one iteration writes. The readers scan
// every row, so the interesting behaviour is per-row and a handful of rows
// reach all of it; more only costs insert commits.
const fuzzRowsCap = 8

// fuzzStorePathCap bounds one fuzzed directory spelling. A path longer than a
// filesystem allows only reaches the store's own limits rather than the
// reader's.
const fuzzStorePathCap = 512

// fuzzPayloadCap bounds one fuzzed message payload. json_valid and
// json_extract both walk what they are given, and a megabyte of braces
// exercises the JSON scanner rather than the reader.
const fuzzPayloadCap = 1 << 14

// fuzzStoreCap bounds one fuzzed crush store. Every row is inserted and then
// scanned, so cost is linear in the blob and a store past the cap buys nothing
// the rows it already carries did not.
const fuzzStoreCap = 1 << 14

// encodeRow packs a row's four column values into the NUL-separated form the
// harness carries, truncating each field at the two separators.
func encodeRow(id, out, in, at string) string {
	fields := [4]string{id, out, in, at}
	for i, f := range fields {
		if cut := strings.IndexAny(f, "\x00\n"); cut >= 0 {
			fields[i] = f[:cut]
		}
	}
	return strings.Join(fields[:], "\x00")
}

// decodeRows unpacks what encodeRow packed. A row of the wrong arity still
// reaches the reader with its missing fields empty, which is the shape a
// hand-edited store holds anyway.
func decodeRows(store []byte) (rows [][4]string) {
	for _, row := range strings.Split(string(store), "\n") {
		if row == "" {
			continue
		}
		fields := strings.Split(row, "\x00")
		var r [4]string
		for i := range r {
			if i < len(fields) {
				r[i] = fields[i]
			}
		}
		rows = append(rows, r)
		if len(rows) == fuzzRowsCap {
			break
		}
	}
	return rows
}

// crushSeedRows are the row shapes a crush store is actually seen holding:
// real counters under both timestamp units crush writes, the storage classes
// a hand-edited or half-migrated file carries, and the values that must not
// be mistaken for a measurement.
func crushSeedRows() []string {
	return []string{
		encodeRow("a", "1200", "400", "1700000000000"),
		encodeRow("a", "1200", "400", "1700000000000"),
		encodeRow("b", "7", "9", "1700000000"),
		encodeRow("a", "-1", "-1", "1700000000000"),
		encodeRow("", "5", "5", "1700000000"),
		encodeRow("d", "9223372036854775807", "9223372036854775807", "1700000000000"),
		encodeRow("e", "1.5", "1.9", "1700000000"),
		encodeRow("f", "many", "1e308", "1700000000000"),
		encodeRow("g", "100", "100", "soon"),
		encodeRow("h", "0", "0", "0"),
		encodeRow("i", "100", "100", "99999999999999999999999"),
		encodeRow("j", "  1200  ", "\t400\n", "1700000000000"),
	}
}

// FuzzReadCrushSessions drives crush's session reader with a store whose rows
// are fuzzed. crush keeps its database inside the project the agent is
// working on, so the directory bounds the query on its own and every row in
// the file is this watcher's. The reader's CASTs and nullable scans are the
// only thing standing between a hostile or merely broken row and a failed
// statement, so a reading that succeeds must name only real sessions, count
// only sane non-negative totals, and answer an unchanged store identically
// twice.
func FuzzReadCrushSessions(f *testing.F) {
	for _, row := range crushSeedRows() {
		f.Add([]byte(row))
	}
	f.Add([]byte(strings.Join(crushSeedRows()[:4], "\n")))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, store []byte) {
		if len(store) > fuzzStoreCap {
			t.Skip()
		}
		path := filepath.Join(t.TempDir(), crushDBRel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		buildCrushStore(t, path, store)
		defer forgetStoreRead("crush", path)

		// The same store read three ways: since an instant, since before
		// everything, and since nothing at all, which is the attach baseline
		// that reads the whole store. Only the reader's predicate differs, and
		// updated_at is the column where it bites: crush writes that column
		// in seconds from its own trigger and in milliseconds from Go, so the
		// predicate compares the column as stored, and a row whose updated_at
		// holds something other than a number is where that comparison stops
		// being an integer comparison.
		attach := time.UnixMilli(1700000000000)
		for _, since := range []time.Time{attach, attach.Add(-time.Hour), {}} {
			got, ok := readCrushSessions(path, since)
			if !ok {
				continue
			}
			for id, c := range got {
				if id == "" {
					t.Fatalf("a row without an id reached the reading: %+v", got)
				}
				if c.output < 0 || c.input < 0 {
					t.Fatalf("session %q has a negative counter: %+v", id, c)
				}
				// One counter is held to the same ceiling a transcript line
				// is, so a value a row cannot really carry is read as absent
				// rather than summed into the displayed rate.
				if c.output > maxSaneTokens || c.input > maxSaneTokens {
					t.Fatalf("session %q past the sane ceiling: %+v", id, c)
				}
				// A session is kept only for a count above zero, so a
				// zeroed pair in the reading is a row the reader meant to
				// drop and did not.
				if c.output == 0 && c.input == 0 {
					t.Fatalf("session %q kept a zeroed reading: %+v", id, c)
				}
			}
			// The file is unchanged between the two reads, so the second is
			// the first repeated: a reader whose answer moves without the
			// store moving is reporting something the store did not say.
			if again, ok2 := readCrushSessions(path, since); ok2 && !maps.Equal(got, again) {
				t.Fatalf("read is not deterministic: %+v then %+v", got, again)
			}
		}
	})
}

// buildCrushStore writes a crush-shaped sessions table holding rows, binding
// each value as written. The table is declared the way crush declares it: id
// is a PRIMARY KEY with no NOT NULL, which in a rowid table is a column a row
// may hold NULL in, and the counters are nullable for the same reason. That is
// the row that scans an absent id into a string and takes the whole statement
// with it.
func buildCrushStore(t *testing.T, path string, store []byte) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		prompt_tokens INTEGER DEFAULT 0,
		completion_tokens INTEGER DEFAULT 0,
		updated_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	// One transaction for the whole store. In autocommit each INSERT is its
	// own durable commit, which at fuzz iteration speed is the harness's
	// dominant cost and says nothing about the reader.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, r := range decodeRows(store) {
		_, err := tx.Exec(
			`INSERT INTO sessions (id, completion_tokens, prompt_tokens, updated_at)
			 VALUES (?, ?, ?, ?)`,
			nilIfEmpty(r[0]), r[1], r[2], r[3])
		if err != nil {
			// A row the store itself refuses (a duplicate primary key) is
			// not a finding about the reader, which never saw it.
			t.Logf("insert %+v: %v", r, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// nilIfEmpty binds an empty fuzzed id as SQL NULL rather than as the empty
// string: the absent id is the case the reader is written for, and an empty
// string is the one the primary key can hold beside it.
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// FuzzOpenCodeDBUsage drives opencode's usage query against a store whose
// session directory and message payload are fuzzed. The query sums
// JSON-extracted counters over rows keyed by session directory, and
// json_extract raises on a payload that is not well-formed JSON, so the CASE
// guard around each extraction is the only thing between one truncated row and
// a store that reads as nothing for as long as the row stands. A decoded
// reading must therefore carry only sane non-negative totals, and one role
// other than assistant must contribute nothing: other roles are prompts, and
// billing one as output is a reading the rows do not support.
func FuzzOpenCodeDBUsage(f *testing.F) {
	for _, seed := range [][2]string{
		{"/work", `{"role":"assistant","tokens":{"output":324,"reasoning":52,"total":376,"input":900}}`},
		{"/work", `{"role":"user","tokens":{"output":999999}}`},
		{"/work", `{"role":"assist`},
		{"/work", ""},
		{"/work", "null"},
		{"/work", "[]"},
		{"/work", `{"role":"assistant","tokens":{"output":-5}}`},
		{"/work", `{"role":"assistant","tokens":{"output":1e30}}`},
		{"/work", `{"role":"assistant","tokens":{"output":1.5,"reasoning":-1.5}}`},
		{"/work", `{"role":"assistant","tokens":"many"}`},
		{"/work", `{"role":"assistant","tokens":{"output":"7"}}`},
		{"/work", `{"role":"assistant","tokens":{"output":null}}`},
		{"/work", `{"role":["assistant"],"tokens":{"output":9}}`},
		// A total below the output beside it, and a negative one: the
		// context cannot be smaller than the turn that produced it, and
		// it cannot be negative. Both are reachable from a single row,
		// and the assertion below only checks a store the corpus
		// actually builds, so the corpus has to name these shapes or
		// the invariant is never exercised.
		{"/work", `{"role":"assistant","tokens":{"output":100,"total":5}}`},
		{"/work", `{"role":"assistant","tokens":{"output":100,"total":-5}}`},
		{"/work", `{"role":"assistant","tokens":{"output":100,"total":0}}`},
		{"/work", `{"role":"assistant","tokens":{"output":100,"total":null}}`},
		{"/work", `{"role":"assistant","tokens":{"reasoning":50,"total":1}}`},
		{"/work", `{"role":"assistant","tokens":{"output":1e30,"total":-1e30}}`},
		{"/work", `{"role":"assistant","tokens":{"output":"100","total":"5"}}`},
		{"", `{"role":"assistant","tokens":{"output":5}}`},
		{"/work\x00\xff", `{"role":"assistant","tokens":{"output":5}}`},
		{"/work' OR 1=1 --", `{"role":"assistant","tokens":{"output":5}}`},
		{"/work/sub\\dir", `{"role":"assistant","tokens":{"output":5}}`},
		{"/work/Équipe", `{"role":"assistant","tokens":{"output":5}}`},
		{"/WORK", `{"role":"assistant","tokens":{"output":5}}`},
		{strings.Repeat("[", 200) + strings.Repeat("]", 200), `{"role":"assistant","tokens":{"output":5}}`},
		{"/work", `{"role":"assistant","tokens":{"output":` + strings.Repeat("[", 200) + strings.Repeat("]", 200) + `}}`},
	} {
		f.Add([]byte(seed[0]), []byte(seed[1]))
	}

	orig := foldSessionDirectory.Load()
	f.Cleanup(func() { foldSessionDirectory.Store(orig) })

	f.Fuzz(func(t *testing.T, dir, payload []byte) {
		if len(dir) > fuzzStorePathCap || len(payload) > fuzzPayloadCap {
			t.Skip()
		}
		path := filepath.Join(t.TempDir(), "opencode.db")
		buildOpenCodeStore(t, path, string(dir), string(payload))
		defer forgetStoreRead("opencode", path)

		foldSessionDirectory.Store(false)
		unfolded, unfoldedOK := openCodeUsage(t, path, string(dir))
		foldSessionDirectory.Store(true)
		folded, foldedOK := openCodeUsage(t, path, string(dir))

		assertUsage := func(which string, v values, ok bool) {
			t.Helper()
			if !ok {
				return
			}
			for _, n := range []int{v.output, v.thinking, v.total, v.input} {
				if n < 0 {
					t.Fatalf("%s: negative counter from directory %q payload %q: %+v",
						which, dir, payload, v)
				}
				if n > maxSaneTokensInt() {
					t.Fatalf("%s: counter %d past the sane ceiling from directory %q payload %q: %+v",
						which, n, dir, payload, v)
				}
			}
			// total is the context the model read, so it is at least the
			// output the turn produced. A decoded store saying otherwise is
			// reporting a reading its rows do not support.
			if v.total > 0 && v.total < v.output {
				t.Fatalf("%s: total %d below output %d from directory %q payload %q: %+v",
					which, v.total, v.output, dir, payload, v)
			}
		}
		assertUsage("unfolded", unfolded, unfoldedOK)
		assertUsage("folded", folded, foldedOK)

		// A payload that is not well-formed JSON reads as absent rather than
		// raising: json_extract raises on one, and the CASE guard is what
		// keeps a truncated row from blinding the agent. The role predicate
		// sits beside it, so a well-formed payload that names no assistant
		// row is absent for the same reason.
		if counted := unfoldedOK && unfolded.output != 0; counted && !countsAssistant(payload) {
			t.Fatalf("payload %q is not an assistant usage record but reported output %d",
				payload, unfolded.output)
		}

		// Folding is the same stored directory compared under the volume's
		// own spelling rules, so it can only widen the match: a session the
		// plain predicate found is still found under the fold.
		if unfoldedOK && unfolded.output > 0 {
			if !foldedOK || folded.output < unfolded.output {
				t.Fatalf("folding lost a session the plain predicate read: %+v then %+v",
					unfolded, folded)
			}
		}

		// The statement and its argument list are built from separate reads of
		// the folding flag, so both forms have to agree for every spelling
		// count and either setting. A disagreement binds a directory where the
		// timestamp belongs and reads another directory's usage as this one's.
		assertPlaceholdersAgree(t)
	})
}

// openCodeUsage reads the store at path as the opencode source would, for one
// directory spelling and no lower bound.
func openCodeUsage(t *testing.T, path, dir string) (values, bool) {
	t.Helper()
	src := openCodeDBSource{path: path}
	return src.read([]string{dir}, time.Time{})
}

// countsAssistant reports whether the payload is a well-formed assistant
// message carrying an output count, which is the only shape the query is
// written to sum.
func countsAssistant(payload []byte) bool {
	var msg struct {
		Role   string `json:"role"`
		Tokens struct {
			Output json.Number `json:"output"`
		} `json:"tokens"`
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&msg); err != nil {
		return false
	}
	if msg.Role != "assistant" {
		return false
	}
	// The extraction casts to REAL, so any JSON number reaches the sum; the
	// guard against an absurd one is counter64, asserted above.
	_, err := msg.Tokens.Output.Float64()
	return err == nil
}

// assertPlaceholdersAgree checks the whole (n, fold, bounded) space the
// statement and its arguments span. dirSpellings yields one spelling on Linux
// and up to ten on macOS, so the memo covers a handful and the rest is built
// fresh on every poll: a form past the memo's bound that paired its
// placeholders wrongly would only show up there.
func assertPlaceholdersAgree(t *testing.T) {
	t.Helper()
	dirs := make([]string, maxCachedQuerySpellings+4)
	for i := range dirs {
		dirs[i] = "/work/" + strings.Repeat("d", i)
	}
	for _, fold := range []bool{false, true} {
		for n := range len(dirs) + 1 {
			args := dirArgs(dirs[:n], fold)
			for _, bounded := range []bool{true, false} {
				query := usageQuery(n, fold, bounded)
				want := len(args)
				if bounded {
					want++ // the since timestamp
				}
				if got := strings.Count(query, "?"); got != want {
					t.Fatalf("usageQuery(%d, fold=%v, bounded=%v) has %d placeholders, want %d",
						n, fold, bounded, got, want)
				}
			}
		}
	}
}

// buildOpenCodeStore writes an opencode-shaped store holding one session
// under dir and one message with payload.
func buildOpenCodeStore(t *testing.T, path, dir, payload string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createOpenCodeSchema(t, db)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	// session.directory is declared NOT NULL by opencode, so unlike crush's
	// id this one cannot be absent: an empty string is the only shape the
	// store holds for "no directory recorded".
	if _, err := tx.Exec(`INSERT INTO session (id, directory) VALUES (?, ?)`,
		"s1", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(
		`INSERT INTO message (id, session_id, time_created, data) VALUES (?, ?, ?, ?)`,
		"m1", "s1", time.Now().UnixMilli(), payload); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
