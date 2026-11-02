// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build sqlite

package agentusage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
// dashboard re-establishes it once rather than per reading. It is
// machine-wide, so the sessions matching one directory are the few rows; the
// session table itself is not, and it is scanned on every poll because
// opencode indexes project_id, workspace_id and parent_id but not directory,
// and this reader cannot add an index to a store it opens mode=ro. The scan
// is linear in the sessions on the box: 50k sessions measured 8ms a read
// against 88µs with a directory index, at a poll that runs every 250ms.
type openCodeDBSource struct{ path string }

func setOpenCodeDB(on bool) bool {
	if !on {
		registerSource("opencode", tokenSource{})
		return true
	}
	path := openCodeDBPath()
	registerSource("opencode", tokenSource{usage: openCodeDBSource{path: path}, auxiliary: openCodeUntimedSource{path: path}})
	return true
}

// openCodeUntimedSource snapshots events without usable timestamps and
// records at the attach millisecond, where timestamps cannot order writes.
type openCodeUntimedSource struct {
	path  string
	since time.Time
}

func (o openCodeUntimedSource) withSince(since time.Time) sessionSource {
	o.since = since
	return o
}

func openCodeRetryPredicate() string {
	id := jsonToken("$.assistantMessageID")
	return `m.type = 'session.step.failed.1' AND ` + id + ` IS NOT NULL AND (EXISTS (
		SELECT 1 FROM event later WHERE later.aggregate_id = m.aggregate_id AND later.seq > m.seq
		AND later.type IN ('session.step.started.1', 'session.step.failed.1', 'session.step.ended.1') AND ` +
		strings.ReplaceAll(id, "m.data", "later.data") + ` = ` + id + `) OR (` + jsonToken("$.tokens") + ` IS NOT NULL AND ` + jsonToken("$.cost") + ` IS NULL))`
}

func openCodeEventPredicate(sequenced bool) string {
	usage := "m.type = 'session.usage.recorded.1' AND " + jsonToken("$.source") + " IN ('title', 'compaction')"
	if !sequenced {
		return usage
	}
	return "(m.type IN ('session.usage.recorded.1', 'session.step.failed.1') AND ((" + usage + ") OR (" + openCodeRetryPredicate() + ")))"
}

func openCodeV2BoundCounters(query string) string {
	for _, path := range []string{"$.tokens.output", "$.tokens.reasoning", "$.tokens.input"} {
		value := "CAST(" + jsonToken(path) + " AS REAL)"
		query = strings.ReplaceAll(query, "MAX("+value+", 0)", fmt.Sprintf("CASE WHEN %s BETWEEN 0 AND %d THEN %s ELSE 0 END", value, maxSaneTokens, value))
	}
	return query
}

func openCodeMessagePredicate() string {
	// A final failure without both tokens and cost leaves old projected tokens.
	id := jsonToken("$.assistantMessageID")
	failedID := strings.ReplaceAll(id, "m.data", "failed.data")
	startedID := strings.ReplaceAll(id, "m.data", "started.data")
	laterID := strings.ReplaceAll(id, "m.data", "later.data")
	return `(` + jsonToken("$.time.completed") + ` IS NOT NULL OR NOT EXISTS (
		SELECT 1 FROM event failed JOIN event started ON started.aggregate_id = failed.aggregate_id
		AND started.seq > failed.seq AND started.type = 'session.step.started.1'
		WHERE failed.aggregate_id = session.id AND failed.type = 'session.step.failed.1'
		AND ` + failedID + ` = m.id AND ` + startedID + ` = m.id)) AND NOT EXISTS (
		SELECT 1 FROM event failed WHERE failed.aggregate_id = session.id
		AND failed.type = 'session.step.failed.1' AND ` + failedID + ` = m.id
		AND (` + strings.ReplaceAll(jsonToken("$.tokens"), "m.data", "failed.data") + ` IS NULL OR ` + strings.ReplaceAll(jsonToken("$.cost"), "m.data", "failed.data") + ` IS NULL)
		AND NOT EXISTS (SELECT 1 FROM event later WHERE later.aggregate_id = failed.aggregate_id
		AND later.seq > failed.seq AND later.type IN ('session.step.started.1', 'session.step.failed.1', 'session.step.ended.1')
		AND ` + laterID + ` = m.id))`
}

func (o openCodeUntimedSource) sessions(dirs []string, since time.Time) (_ map[string]map[string]sessionCounts, ok bool) {
	o.path = openCodeSourcePath(o.path, dirs)
	if o.path == "" || len(dirs) == 0 {
		return nil, false
	}
	h, err := openStore(o.path)
	if err != nil {
		if !storeAbsent(o.path) {
			auditStoreRead("opencode auxiliary", o.path, err)
		}
		return nil, false
	}
	defer releaseStore(h)
	defer func() {
		if !ok {
			closeStore(o.path)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), dbQueryTimeout)
	defer cancel()
	tx, err := h.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		auditStoreRead("opencode auxiliary", o.path, err)
		return nil, false
	}
	defer tx.Rollback()
	return o.sessionsTx(ctx, tx, dirs, since.IsZero())
}

func (o openCodeUntimedSource) sessionsTx(ctx context.Context, tx *sql.Tx, dirs []string, baseline bool) (map[string]map[string]sessionCounts, bool) {
	var applicable, split, timed, sequenced, eventID bool
	if err := tx.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'session_message')
		OR EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'session_v2'),
		EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'session_v2'),
		EXISTS(SELECT 1 FROM pragma_table_info('event') WHERE name = 'created'),
		EXISTS(SELECT 1 FROM pragma_table_info('event') WHERE name = 'seq'),
		EXISTS(SELECT 1 FROM pragma_table_info('event') WHERE name = 'id')`).Scan(&applicable, &split, &timed, &sequenced, &eventID); err != nil {
		auditStoreRead("opencode auxiliary", o.path, err)
		return nil, false
	}
	counts := map[string]sessionCounts{}
	retries := map[string]sessionCounts{}
	contexts := map[string]sessionCounts{}
	boundaryMessages := map[string]sessionCounts{}
	if applicable {
		fold := foldSessionDirectory.Load()
		sessionTable, untimed := "session", "1"
		if split {
			sessionTable = "session_v2"
		}
		if timed {
			untimed = "m.created = 0"
			if !o.since.IsZero() {
				untimed = fmt.Sprintf("(m.created = 0 OR m.created = %d)", o.since.UnixMilli())
			}
		}
		eventKind := openCodeEventPredicate(false)
		if eventID {
			eventKind = "(" + eventKind + ") AND m.id IS NOT NULL"
		}
		query := fmt.Sprintf(`SELECT session.id,
			COALESCE(CAST(SUM(MAX(CAST(%s AS REAL), 0)) AS INTEGER), 0),
			COALESCE(CAST(SUM(MAX(CAST(%s AS REAL), 0)) AS INTEGER), 0),
			COALESCE(CAST(SUM(MAX(CAST(%s AS REAL), 0)) AS INTEGER), 0)
			FROM %s AS session LEFT JOIN event m ON m.aggregate_id = session.id
			AND %s AND %s
			WHERE %s
			GROUP BY session.id`, jsonToken("$.tokens.output"), jsonToken("$.tokens.input"), jsonToken("$.tokens.reasoning"),
			sessionTable, eventKind, untimed, directoryPred(len(dirs), fold))
		query = openCodeV2BoundCounters(query)
		rows, err := tx.QueryContext(ctx, query, dirArgs(dirs, fold)...)
		if err != nil {
			auditStoreRead("opencode auxiliary", o.path, err)
			return nil, false
		}
		defer rows.Close()
		for rows.Next() {
			var id sql.NullString
			var output, input, thinking sql.NullInt64
			if err := rows.Scan(&id, &output, &input, &thinking); err != nil {
				auditStoreRead("opencode auxiliary", o.path, err)
				return nil, false
			}
			if id.Valid {
				counts[id.String] = sessionCounts{output: int64(satAdd(counter64(output.Int64), counter64(thinking.Int64))), input: int64(counter64(input.Int64)), thinking: int64(counter64(thinking.Int64))}
			}
		}
		if err := rows.Err(); err != nil {
			auditStoreRead("opencode auxiliary", o.path, err)
			return nil, false
		}
		var parts []string
		for _, path := range []string{"$.tokens.input", "$.tokens.output", "$.tokens.reasoning", "$.tokens.cache.read", "$.tokens.cache.write"} {
			parts = append(parts, "MAX(COALESCE(CAST("+jsonToken(path)+" AS REAL), 0), 0)")
		}
		kind := eventKind
		if sequenced {
			kind = openCodeEventPredicate(true)
			if baseline {
				kind = "m.type IN ('session.usage.recorded.1', 'session.step.failed.1') AND (" + eventKind + " OR m.type = 'session.step.failed.1')"
			}
			if eventID {
				kind = "(" + kind + ") AND m.id IS NOT NULL"
			}
		}
		identity := "CAST(m.rowid AS TEXT)"
		if eventID {
			identity = "m.id"
		}
		query = fmt.Sprintf(`SELECT %s, CAST((%s) AS INTEGER)
			FROM %s AS session CROSS JOIN event m
			WHERE m.aggregate_id = session.id AND %s AND %s AND %s`,
			identity, strings.Join(parts, " + "), sessionTable, directoryPred(len(dirs), fold), untimed, kind)
		rows, err = tx.QueryContext(ctx, query, dirArgs(dirs, fold)...)
		if err != nil {
			auditStoreRead("opencode auxiliary", o.path, err)
			return nil, false
		}
		defer rows.Close()
		for rows.Next() {
			var id sql.NullString
			var context int64
			if err := rows.Scan(&id, &context); err != nil {
				auditStoreRead("opencode auxiliary", o.path, err)
				return nil, false
			}
			if id.Valid {
				contexts["event\x00"+id.String] = sessionCounts{context: int64(counter64(context))}
			}
		}
		if err := rows.Err(); err != nil {
			auditStoreRead("opencode auxiliary", o.path, err)
			return nil, false
		}
		// A failure present at attach can become superseded only after attach.
		if sequenced && !o.since.IsZero() {
			kind := "m.type = 'session.step.failed.1'"
			if !baseline {
				kind = openCodeRetryPredicate()
			}
			if eventID {
				kind = "(" + kind + ") AND m.id IS NOT NULL"
			}
			query := fmt.Sprintf(`SELECT session.id, m.seq,
				COALESCE(CAST(MAX(CAST(%s AS REAL), 0) AS INTEGER), 0),
				COALESCE(CAST(MAX(CAST(%s AS REAL), 0) AS INTEGER), 0),
				COALESCE(CAST(MAX(CAST(%s AS REAL), 0) AS INTEGER), 0)
				FROM %s AS session CROSS JOIN event m
				WHERE m.aggregate_id = session.id AND %s AND %s AND %s`,
				jsonToken("$.tokens.output"), jsonToken("$.tokens.input"), jsonToken("$.tokens.reasoning"),
				sessionTable, directoryPred(len(dirs), fold), untimed, kind)
			query = openCodeV2BoundCounters(query)
			rows, err := tx.QueryContext(ctx, query, dirArgs(dirs, fold)...)
			if err != nil {
				auditStoreRead("opencode auxiliary", o.path, err)
				return nil, false
			}
			defer rows.Close()
			for rows.Next() {
				var id sql.NullString
				var seq, output, input, thinking int64
				if err := rows.Scan(&id, &seq, &output, &input, &thinking); err != nil {
					auditStoreRead("opencode auxiliary", o.path, err)
					return nil, false
				}
				if id.Valid {
					retries[fmt.Sprintf("%s\x00%d", id.String, seq)] = sessionCounts{output: int64(satAdd(counter64(output), counter64(thinking))), input: int64(counter64(input)), thinking: int64(counter64(thinking))}
				}
			}
			if err := rows.Err(); err != nil {
				auditStoreRead("opencode auxiliary", o.path, err)
				return nil, false
			}
		}
		if !o.since.IsZero() {
			var hasFork bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name = 'fork_session_id')`, sessionTable).Scan(&hasFork); err != nil {
				auditStoreRead("opencode auxiliary", o.path, err)
				return nil, false
			}
			fork := ""
			if hasFork {
				fork = "AND (session.fork_session_id IS NULL OR m.time_created >= session.time_created)"
			}
			contextFork := fork
			if sequenced {
				fork += " AND " + openCodeMessagePredicate()
				if !baseline {
					contextFork = fork
				}
			}
			ended := "0"
			if sequenced {
				// Retries can complete with exactly the same tokens as the previous attempt.
				ended = `COALESCE((SELECT MAX(e.seq) FROM event e WHERE e.aggregate_id = session.id AND e.type IN ('session.step.ended.1', 'session.step.failed.1') AND ` + strings.ReplaceAll(jsonToken("$.assistantMessageID"), "m.data", "e.data") + ` = m.id), 0)`
			}
			query = fmt.Sprintf(`SELECT m.id, CAST((%s) AS INTEGER), COALESCE(%s, ''), %s,
				COALESCE(CAST(MAX(CAST(%s AS REAL), 0) AS INTEGER), 0),
				COALESCE(CAST(MAX(CAST(%s AS REAL), 0) AS INTEGER), 0),
				COALESCE(CAST(MAX(CAST(%s AS REAL), 0) AS INTEGER), 0), %s
				FROM %s AS session CROSS JOIN session_message m
				WHERE m.session_id = session.id AND m.id IS NOT NULL AND m.type = 'assistant' AND %s
				AND COALESCE(CAST(%s AS INTEGER), m.time_created) = %d %s`,
				strings.Join(parts, " + "), jsonToken("$.tokens"), ended, jsonToken("$.tokens.output"), jsonToken("$.tokens.input"), jsonToken("$.tokens.reasoning"),
				"CASE WHEN 1=1 "+fork+" THEN 1 ELSE 0 END", sessionTable, directoryPred(len(dirs), fold), jsonToken("$.time.completed"), o.since.UnixMilli(), contextFork)
			query = openCodeV2BoundCounters(query)
			messages, err := tx.QueryContext(ctx, query, dirArgs(dirs, fold)...)
			if err != nil {
				auditStoreRead("opencode auxiliary", o.path, err)
				return nil, false
			}
			defer messages.Close()
			for messages.Next() {
				var id sql.NullString
				var context, sequence, output, input, thinking int64
				var included bool
				var data []byte
				if err := messages.Scan(&id, &context, &data, &sequence, &output, &input, &thinking, &included); err != nil {
					auditStoreRead("opencode auxiliary", o.path, err)
					return nil, false
				}
				if !id.Valid {
					continue
				}
				contexts["message\x00"+id.String] = sessionCounts{context: int64(counter64(context)), content: sha256.Sum256(fmt.Appendf(data, "\x00%d", sequence))}
				if included {
					boundaryMessages[id.String] = sessionCounts{output: int64(satAdd(counter64(output), counter64(thinking))), input: int64(counter64(input)), thinking: int64(counter64(thinking)), sequence: sequence}
				}
			}
			if err := messages.Err(); err != nil {
				auditStoreRead("opencode auxiliary", o.path, err)
				return nil, false
			}
		}
	}
	noteStoreReadOK("opencode auxiliary", o.path)
	return map[string]map[string]sessionCounts{o.path: counts, o.path + "\x00retry": retries, o.path + "\x00context": contexts, o.path + "\x00message": boundaryMessages}, true
}

// openCodeDBPath locates the session database. A relative data root stays
// relative until the watched agent's working directory is known.
func openCodeDBPath() string {
	data := os.Getenv(XDGDataHomeEnv)
	if data == "" {
		if dir := HomeDir(); dir != "" {
			data = filepath.Join(dir, ".local", "share")
		} else {
			data = ""
		}
	}
	name, set := os.LookupEnv(OpenCodeDBEnv)
	if !set {
		name = "opencode.db"
	}
	if name == ":memory:" {
		return ""
	}
	if filepath.IsAbs(name) {
		return filepath.Clean(name)
	}
	if data == "" {
		return ""
	}
	return filepath.Join(data, "opencode", name)
}

func openCodeSourcePath(path string, dirs []string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	if len(dirs) == 0 || !filepath.IsAbs(dirs[0]) {
		return ""
	}
	return filepath.Join(dirs[0], path)
}

// usageQuery sums what this directory's sessions spent after a point in time.
// Cached reads are deliberately absent: they are cache hits, not billed
// prompt. tokens.input, when present, is billed prompt and is summed.
// Only assistant messages carry tokens; other roles are prompts. The JSON
// paths and the directory list are spliced in by usageQueryFor, since SQL has
// no placeholder for either. MAX(CAST(…), 0) floors a negative stored counter
// at zero so one malformed row cannot subtract from sessions that read fine.
//
// The context total is floored after the read, in floorTotal, not in the
// statement. It has to be the largest of two numbers — the stored maximum and
// the output this same reading summed — and the first is an aggregate over
// rows while the second is another aggregate over the same rows, which SQL
// will not nest: a scalar MAX of the two is a per-row fold that silently
// reports whichever row it saw last. counter already refuses a negative total,
// so that half of the rule was never missing here; the half that was is the
// floor against the output. Every JSONL adapter gets the same floor from
// foldCounters, and this reader's total is a MAX in SQL, so a store holding a
// total below the output beside it published a context smaller than the turn
// that produced it: a reading no row supports.
//
// floorTotal lives beside foldCounters in sample.go rather than here, so the
// untagged adapters and this tagged reader apply one rule instead of two
// copies that could drift.
//
// Sessions are the outer loop, then messages through the message index on
// session_id. CROSS JOIN stops SQLite from reversing that into a scan of
// message: opencode's own index leads with session_id and carries
// time_created beside it, so the join and the since bound are both served by
// it, and the message table is kept out of the read entirely. It indexes
// session_id but not directory, which leaves the session table itself scanned
// (see the type's comment for what that costs). CAST keeps MAX numeric:
// json_extract of a JSON string is TEXT, and SQLite ranks TEXT above INTEGER,
// so MAX('9', 100) would be '9'.
//
// The since bound is a spliced predicate, not a placeholder, because a
// zero since means no lower bound at all and a bound still needs a value to
// compare against. Splice it a lower bound that no message can precede and
// the statement reads as unbounded, which is what the caller meant, but the
// spelling is then a fact about how far back timestamps reach rather than
// about the caller's intent, and it moves the day the arithmetic that
// produced it is wrong for. A watcher attaches with since = time.Now(), so a
// zero one is the unattached reading of a tree that ran before this process
// did; it means the whole store.
const usageQueryFormat = `
	SELECT
		COALESCE(CAST(SUM(MAX(CAST(%[1]s AS REAL), 0)) AS INTEGER), 0),
		COALESCE(CAST(SUM(MAX(CAST(%[2]s AS REAL), 0)) AS INTEGER), 0),
		COALESCE(MAX(CAST(%[3]s AS INTEGER)), 0),
		COALESCE(CAST(SUM(MAX(CAST(%[4]s AS REAL), 0)) AS INTEGER), 0)
	FROM session
	CROSS JOIN message m
	WHERE m.session_id = session.id
	  AND %[5]s AND %[6]s
	  AND %[7]s = 'assistant'`

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

// usageQueryFor builds the bounded query for n directory spellings: the one
// that filters on a since instant. Only the number of placeholders varies:
// every value still travels as a bound parameter.
func usageQueryFor(n int) string {
	return usageQuery(n, foldSessionDirectory.Load(), true)
}

// usageQuery is usageQueryFor with the directory folding and the presence of a
// since bound fixed by the caller. Both decide how many placeholders the
// statement carries and how many arguments are bound to them, so one read of
// them has to govern both: a statement built folded against a flat argument
// list, or a bounded statement bound to an unbounded argument list, binds a
// directory where the timestamp belongs and reads another directory's usage as
// this one's.
//
// Memoized on (n, fold, bounded): those are a directory-spelling count and two
// bools, so the statement is one of a handful for the life of the process,
// while the un-memoized form spent five Sprintf calls building it on every
// poll of every opencode watcher. The cache is bounded by n, which is the
// number of spellings dirSpellings produced (one or two off macOS, up to ten
// on it), and a count above the bound is built and discarded rather than
// retained.
func usageQuery(n int, fold, bounded bool) string {
	if n < 0 || n > maxCachedQuerySpellings {
		return buildUsageQuery(n, fold, bounded)
	}
	key := queryKey{n: n, fold: fold, bounded: bounded}
	queryCacheMu.Lock()
	defer queryCacheMu.Unlock()
	if q, ok := queryCache[key]; ok {
		return q
	}
	q := buildUsageQuery(n, fold, bounded)
	queryCache[key] = q
	return q
}

// maxCachedQuerySpellings bounds the memo: dirSpellings yields the resolved
// spelling and the caller's own, and on macOS the other normalization and case
// forms of each, so a mixed-form accented path behind a symlinked working
// directory reaches ten. A watcher over such a path rebuilds the statement
// every poll, which costs one Sprintf per spelling and keeps no state.
const maxCachedQuerySpellings = 8

type queryKey struct {
	n       int
	fold    bool
	bounded bool
}

var (
	queryCacheMu sync.Mutex
	queryCache   = map[queryKey]string{}
)

func buildUsageQuery(n int, fold, bounded bool) string {
	return fmt.Sprintf(usageQueryFormat,
		jsonToken("$.tokens.output"),
		jsonToken("$.tokens.reasoning"),
		jsonToken("$.tokens.total"),
		jsonToken("$.tokens.input"),
		directoryPred(n, fold),
		sincePred(bounded),
		jsonToken("$.role"))
}

// sincePred is the lower bound on message.time_created, or no bound at all
// when the caller passed a zero since. The two forms carry different numbers
// of placeholders, so the caller binding the arguments is the one that has to
// ask for the form.
func sincePred(bounded bool) string {
	if bounded {
		return "m.time_created > ?"
	}
	return "1"
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

func (o openCodeDBSource) read(dirs []string, since time.Time) (values, bool) {
	v, _, ok := o.readSnapshot(dirs, since)
	return v, ok
}

func (o openCodeDBSource) readSnapshot(dirs []string, since time.Time) (v values, snapshot map[string]map[string]sessionCounts, ok bool) {
	o.path = openCodeSourcePath(o.path, dirs)
	if o.path == "" || len(dirs) == 0 {
		return values{}, nil, false
	}
	// mode=ro leaves the database alone; a missing one is an answer ("nothing
	// to report"), not an error worth surfacing, since most machines running
	// this have no opencode at all. A store that is present and unreadable is
	// a different thing: it reports nothing forever, which the dashboard
	// renders as an idle agent, so it is audited.
	h, err := openStore(o.path)
	if err != nil {
		if !storeAbsent(o.path) {
			auditStoreRead("opencode", o.path, err)
		}
		return values{}, nil, false
	}
	// The handle is held for this read, so a reader that is on it while
	// another read drops it, or the cache evicts it, still has a live
	// connection to finish on. Registered first so it runs last: the drop
	// below has to land before the release.
	defer releaseStore(h)
	db := h.db
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
	// A zero since is the unattached reading: the whole store counts, and the
	// statement drops its lower bound rather than binding an instant no
	// message can precede.
	bounded := !since.IsZero()
	args := dirArgs(dirs, fold)
	if bounded {
		args = append(args, since.UnixMilli())
	}

	ctx, cancel := context.WithTimeout(context.Background(), dbQueryTimeout)
	defer cancel()
	// Schema, message usage and auxiliary events must come from one snapshot.
	// OpenCode can write or migrate the store between any two statements.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		auditStoreRead("opencode", o.path, err)
		return values{}, nil, false
	}
	defer tx.Rollback()

	var out, thinking, total, input sql.NullInt64
	var v2, split bool
	if err := tx.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'session_message'),
		EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'session_v2')`).Scan(&v2, &split); err != nil {
		auditStoreRead("opencode", o.path, err)
		return values{}, nil, false
	}
	v2 = v2 || split
	hasFork, eventTime, eventSeq, eventID := false, false, false, false
	if v2 {
		sessionTable := "session"
		if split {
			sessionTable = "session_v2"
		}
		if err := tx.QueryRowContext(ctx, `SELECT
			EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name = 'fork_session_id'),
			EXISTS(SELECT 1 FROM pragma_table_info('event') WHERE name = 'created'),
			EXISTS(SELECT 1 FROM pragma_table_info('event') WHERE name = 'seq'),
			EXISTS(SELECT 1 FROM pragma_table_info('event') WHERE name = 'id')`, sessionTable).Scan(&hasFork, &eventTime, &eventSeq, &eventID); err != nil {
			auditStoreRead("opencode", o.path, err)
			return values{}, nil, false
		}
	}
	query := usageQuery(len(dirs), fold, bounded)
	if v2 {
		if split {
			query = strings.Replace(query, "FROM session\n", "FROM session_v2 AS session\n", 1)
		}
		query = strings.Replace(query, "CROSS JOIN message m", "CROSS JOIN session_message m", 1)
		query = strings.Replace(query, jsonToken("$.role"), "m.type", 1)
		query = strings.Replace(query, "AND m.type = 'assistant'", "AND m.id IS NOT NULL AND m.type = 'assistant'", 1)
		if bounded {
			// OpenCode 2 publishes a step's usage at completion, which can be
			// later than the watch attaching to its already-created message.
			query = strings.Replace(query, sincePred(true), "COALESCE(CAST("+jsonToken("$.time.completed")+" AS INTEGER), m.time_created) > ?", 1)
		}
		var tokens []string
		for _, path := range []string{"$.tokens.input", "$.tokens.output", "$.tokens.reasoning", "$.tokens.cache.read", "$.tokens.cache.write"} {
			tokens = append(tokens, "MAX(COALESCE(CAST("+jsonToken(path)+" AS REAL), 0), 0)")
		}
		contextTotal := strings.Join(tokens, " + ")
		query = strings.Replace(query, jsonToken("$.tokens.total"), fmt.Sprintf("CASE WHEN (%s) <= %d THEN (%s) END", contextTotal, maxSaneTokens, contextTotal), 1)
		if hasFork {
			query = strings.Replace(query, "AND m.type = 'assistant'", "AND m.type = 'assistant' AND (session.fork_session_id IS NULL OR m.time_created >= session.time_created)", 1)
		}
		if eventSeq {
			query = strings.Replace(query, "AND m.type = 'assistant'", "AND m.type = 'assistant' AND "+openCodeMessagePredicate(), 1)
		}
		query = openCodeV2BoundCounters(query)
	}
	row := tx.QueryRowContext(ctx, query, args...)
	if err := row.Scan(&out, &thinking, &total, &input); err != nil {
		if !errors.Is(err, sql.ErrNoRows) && !storeAbsent(o.path) {
			auditStoreRead("opencode", o.path, err)
		}
		return values{}, nil, false
	}
	// Untimed events use the watcher's per-session baseline for bounded reads.
	if v2 && (eventTime || !bounded) {
		// Title and compaction usage is stored as events, not assistant messages.
		sessionTable := "session"
		if split {
			sessionTable = "session_v2"
		}
		eventKind := openCodeEventPredicate(eventSeq)
		if eventID {
			eventKind = "(" + eventKind + ") AND m.id IS NOT NULL"
		}
		var eventTokens []string
		for _, path := range []string{"$.tokens.input", "$.tokens.output", "$.tokens.reasoning", "$.tokens.cache.read", "$.tokens.cache.write"} {
			eventTokens = append(eventTokens, "MAX(COALESCE(CAST("+jsonToken(path)+" AS REAL), 0), 0)")
		}
		eventTotal := strings.Join(eventTokens, " + ")
		query = fmt.Sprintf(`SELECT
			COALESCE(CAST(SUM(MAX(CAST(%s AS REAL), 0)) AS INTEGER), 0),
			COALESCE(CAST(SUM(MAX(CAST(%s AS REAL), 0)) AS INTEGER), 0),
			COALESCE(CAST(SUM(MAX(CAST(%s AS REAL), 0)) AS INTEGER), 0),
			COALESCE(CAST(MAX(CASE WHEN (%s) <= %d THEN (%s) END) AS INTEGER), 0)
			FROM %s AS session CROSS JOIN event m
			WHERE m.aggregate_id = session.id AND %s AND %s
			AND %s`,
			jsonToken("$.tokens.output"), jsonToken("$.tokens.reasoning"), jsonToken("$.tokens.input"), eventTotal, maxSaneTokens, eventTotal, sessionTable,
			directoryPred(len(dirs), fold), strings.Replace(sincePred(bounded), "m.time_created", "m.created", 1), eventKind)
		query = openCodeV2BoundCounters(query)
		var extraOut, extraThinking, extraInput, extraTotal sql.NullInt64
		if err := tx.QueryRowContext(ctx, query, args...).Scan(&extraOut, &extraThinking, &extraInput, &extraTotal); err != nil {
			auditStoreRead("opencode", o.path, err)
			return values{}, nil, false
		}
		out.Int64 = int64(satAdd(counter64(out.Int64), counter64(extraOut.Int64)))
		thinking.Int64 = int64(satAdd(counter64(thinking.Int64), counter64(extraThinking.Int64)))
		input.Int64 = int64(satAdd(counter64(input.Int64), counter64(extraInput.Int64)))
		total.Int64 = int64(max(counter64(total.Int64), counter64(extraTotal.Int64)))
	}
	outCount := counter64(out.Int64)
	if v2 {
		// OpenCode 2 stores visible output and reasoning as separate counters.
		outCount = satAdd(outCount, counter64(thinking.Int64))
	}
	v = values{
		output:   outCount,
		thinking: counter64(thinking.Int64),
		total:    floorTotal(counter64(total.Int64), outCount),
		input:    counter64(input.Int64),
	}
	if bounded {
		snapshot, ok = (openCodeUntimedSource{path: o.path, since: since}).sessionsTx(ctx, tx, dirs, false)
		if !ok {
			return values{}, nil, false
		}
	}
	noteStoreReadOK("opencode", o.path)
	// The read itself succeeded whatever the totals came to, so the handle
	// stays open even when the store holds nothing for this window.
	return v, snapshot, true
}
