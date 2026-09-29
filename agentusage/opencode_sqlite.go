// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build sqlite

package agentusage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/maci0/toktop/internal/core"
	"golang.org/x/text/unicode/norm"
	"modernc.org/sqlite"
)

// foldDir brings a stored session directory and a watched one to the one
// spelling the database matches on: composed, then simple case folded.
//
// core.FoldCase, the fold strings.EqualFold compares by, not x/text
// cases.Fold. Full folding is a case mapping, and it over-folds: it renders
// U+00DF as "ss" and U+0130 as "i" plus a combining dot, so the stored
// "/work/straße" and the watched "/work/strasse", two directories a
// case-insensitive volume keeps apart, match, and one checkout's tokens are
// billed to the other. Simple folding keeps them apart, and it is the fold
// the volume performs: two runes are fold-equivalent exactly when they share
// a simple folding orbit.
func foldDir(s string) string {
	return core.FoldCase(norm.NFC.String(s))
}

func init() {
	sqlite.MustRegisterCollationUtf8("toktop_directory", func(left, right string) int {
		return strings.Compare(foldDir(left), foldDir(right))
	})
}

// opencode keeps its sessions in SQLite, as crush does, rather than in the
// JSONL the file adapters tail: ~/.local/share/opencode/opencode.db, with one
// row per message and the usage in a JSON column.
//
//	session.directory  the working directory, which is the attribution key
//	message.data       {"role":"assistant","tokens":{"output":324,"reasoning":52,…}}
//	message.time_created  milliseconds, which bounds the since filter
//
// The database is opened read-only and the handle is kept across polls (see
// sqlite.go), dropped when a read fails or the file is replaced, so the
// dashboard re-establishes it once rather than per reading. Sessions for this
// directory are few; their messages are found through the session_id index
// rather than a scan of the message table.
type openCodeDBSource struct{ path string }

func setOpenCodeDB(on bool) bool {
	if !on {
		registerSource("opencode", tokenSource{})
		return true
	}
	registerSource("opencode", tokenSource{usage: openCodeDBSource{path: openCodeDBPath()}})
	return true
}

// openCodeDBPath locates the session database, honoring XDG_DATA_HOME the way
// opencode itself does. A relative value is ignored: the XDG base-directory
// spec calls it invalid, and joining onto it would read a database under
// whatever directory the run started in, which reads as an agent producing
// no tokens.
func openCodeDBPath() string {
	if data := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(data) {
		return filepath.Join(data, "opencode", "opencode.db")
	}
	dir := HomeDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, ".local", "share", "opencode", "opencode.db")
}

// usageQuery sums what this directory's sessions spent after a point in time.
// Cached reads are deliberately absent: they are cache hits, not billed
// prompt. tokens.input, when present, is billed prompt and is summed.
// Only assistant messages carry tokens; other roles are prompts. The JSON
// paths and the directory list are spliced in by usageQueryFor, since SQL has
// no placeholder for either. MAX(CAST(…), 0) floors a negative stored counter
// at zero so one malformed row cannot subtract from sessions that read fine.
//
// Sessions are the outer loop, then messages via message_session_idx
// (session_id). CROSS JOIN stops SQLite from reversing that into a scan of
// message: opencode indexes session_id, not (session_id, time_created) and
// not directory. CAST keeps MAX numeric: json_extract of a JSON string is
// TEXT, and SQLite ranks TEXT above INTEGER, so MAX('9', 100) would be '9'.
const usageQueryFormat = `
	SELECT
		COALESCE(CAST(SUM(MAX(CAST(%[1]s AS REAL), 0)) AS INTEGER), 0),
		COALESCE(CAST(SUM(MAX(CAST(%[2]s AS REAL), 0)) AS INTEGER), 0),
		COALESCE(MAX(CAST(%[3]s AS INTEGER)), 0),
		COALESCE(CAST(SUM(MAX(CAST(%[4]s AS REAL), 0)) AS INTEGER), 0)
	FROM session
	CROSS JOIN message m
	WHERE m.session_id = session.id
	  AND %[5]s AND m.time_created > ?
	  AND %[6]s = 'assistant'`

// jsonToken builds the extraction of one JSON path from a message payload,
// guarded so a row that is not well-formed JSON reads as absent instead of
// raising. json_extract raises on malformed input, and one such row fails the
// whole statement: this is another program's store, which toktop opens
// read-only and cannot constrain, so a single truncated or non-JSON payload
// would blind every reading of this agent for as long as the row survived.
// The CASE guards the extract rather than adding a json_valid term beside it,
// because the aggregate is a SELECT list where no predicate orders the
// evaluation. A NULL payload keeps reading as NULL, as json_extract(NULL) did.
//
// The three sums accumulate in REAL and cast back on the way out. SQLite's
// integer sum() raises "integer overflow" rather than wrapping, and a store
// this program cannot constrain can hold a row no agent wrote: CAST of a
// 1e30 JSON number to INTEGER is MaxInt64, so one such row beside any other
// overflowed the sum and failed the statement. Every read of the agent then
// failed the same way, for as long as the row survived. A real sum cannot
// overflow, and the cast back clamps a value past int64 to MaxInt64, which
// counter64 already refuses, the same answer one absurd row gets on its own.
func jsonToken(path string) string {
	return fmt.Sprintf("CASE WHEN json_valid(m.data) THEN json_extract(m.data, '%s') END", path)
}

// foldSessionDirectory compares session.directory case-insensitively and with
// either path separator. NTFS and the default APFS configuration look names
// up that way; an agent records whichever spelling its runtime produced.
// Linux compares bytes. Tests may flip it.
//
// Atomic because every opencode watcher's goroutine consults it on every read
// while a caller (or a test) flips it underneath them.
var foldSessionDirectory atomic.Bool

func init() {
	foldSessionDirectory.Store(runtime.GOOS == "windows" || runtime.GOOS == "darwin")
}

// usageQueryFor builds the query for n directory spellings. Only the number of
// placeholders varies: every value still travels as a bound parameter.
func usageQueryFor(n int) string {
	return usageQuery(n, foldSessionDirectory.Load())
}

// usageQuery is usageQueryFor with the directory folding fixed by the caller.
// The flag decides how many placeholders the statement carries and how many
// arguments are bound to them, so one read of it has to govern both: a
// statement built folded against a flat argument list, or the reverse, binds a
// directory where the timestamp belongs and reads another directory's usage as
// this one's.
func usageQuery(n int, fold bool) string {
	return fmt.Sprintf(usageQueryFormat,
		jsonToken("$.tokens.output"),
		jsonToken("$.tokens.reasoning"),
		jsonToken("$.tokens.total"),
		jsonToken("$.tokens.input"),
		directoryPred(n, fold),
		jsonToken("$.role"))
}

func directoryPred(n int, fold bool) string {
	ph := strings.TrimSuffix(strings.Repeat("?,", n), ",")
	pred := "session.directory IN (" + ph + ")"
	if fold {
		pred = "(" + pred + " OR replace(session.directory, char(92), '/') COLLATE toktop_directory IN (" + ph + "))"
	}
	return pred
}

// dirArgs binds the directory spellings, in the order directoryPred lists
// them: the plain forms first, then the folded set the second branch compares
// against, and only when that branch is in the statement.
func dirArgs(dirs []string, fold bool) []any {
	args := make([]any, 0, 2*len(dirs))
	for _, d := range dirs {
		args = append(args, d)
	}
	if fold {
		for _, d := range dirs {
			args = append(args, foldDir(filepath.ToSlash(d)))
		}
	}
	return args
}

func (o openCodeDBSource) read(dirs []string, since time.Time) (v values, ok bool) {
	if o.path == "" || len(dirs) == 0 {
		return values{}, false
	}
	// mode=ro leaves the database alone; a missing one is an answer ("nothing
	// to report"), not an error worth surfacing, since most machines running
	// this have no opencode at all. A store that is present and unreadable is
	// a different thing: it reports nothing forever, which the dashboard
	// renders as an idle agent, so it is audited.
	db, err := openStore(o.path)
	if err != nil {
		if !storeAbsent(o.path) {
			auditStoreRead("opencode", o.path, err)
		}
		return values{}, false
	}
	// A read that failed drops the shared handle, so the next poll opens a
	// fresh one instead of reusing a handle that just went wrong. Only a failed
	// read drops it: a store that reads and holds nothing in this window is an
	// idle agent, not a broken handle, and dropping it would reopen the
	// database on every poll of every idle session. Registered before the
	// cancel below so the context is released first.
	defer func() {
		if !ok {
			closeStore(o.path)
		}
	}()

	// One read of the folding flag decides the statement and its arguments
	// together, so the placeholder count and the bound values cannot disagree.
	fold := foldSessionDirectory.Load()
	args := append(dirArgs(dirs, fold), since.UnixMilli())

	ctx, cancel := context.WithTimeout(context.Background(), dbQueryTimeout)
	defer cancel()

	var out, thinking, total, input sql.NullInt64
	row := db.QueryRowContext(ctx, usageQuery(len(dirs), fold), args...)
	if err := row.Scan(&out, &thinking, &total, &input); err != nil {
		if !errors.Is(err, sql.ErrNoRows) && !storeAbsent(o.path) {
			auditStoreRead("opencode", o.path, err)
		}
		return values{}, false
	}
	v = values{
		output:   counter64(out.Int64),
		thinking: counter64(thinking.Int64),
		total:    counter64(total.Int64),
		input:    counter64(input.Int64),
	}
	noteStoreReadOK("opencode", o.path)
	// The read itself succeeded whatever the totals came to, so the handle
	// stays open even when the store holds nothing for this window.
	return v, true
}
