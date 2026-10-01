// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build sqlite

package agentusage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// crush keeps its sessions in SQLite like opencode, but inside the project it
// is working on rather than under $HOME: `.crush/crush.db`, at the project
// root crush resolves (the git root, the way its own config discovery does).
//
//	sessions.completion_tokens  what the model generated, which is output here
//	sessions.prompt_tokens      billed prompt, which is input here
//	sessions.updated_at         when the row last changed, which bounds the since filter
//
// That last column carries two units. The schema comments call it
// milliseconds, and crush writes milliseconds from Go, but the table's own
// update trigger writes `strftime('%s','now')`, which is seconds: a row can
// hold either depending on who touched it last. The since predicate compares
// the column as stored (milliseconds against Unix ms, seconds against Unix
// seconds) rather than wrapping it, so an index on updated_at can be used.
//
// The only JSONL crush writes is `.crush/logs/crush.log`, and it carries no
// counters, so there is nothing for the file adapters to tail.
//
// The database being inside the project tree makes the directory the query
// bound on its own: there is no cross-project store to filter, so a session
// written after attach is this watcher's. It is registered without an
// opt-in switch for the same reason, unlike opencode's operator-wide store.
type crushDBSource struct{}

// builtinSource returns the sources this build links in. crush is one because
// its database lives inside the project being watched, so there is nothing for
// the operator to opt into; opencode is not, because its machine-wide store
// needs the EnableOpenCodeDB gate. A function rather than an init-time
// registry write, so nothing is installed at module load.
func builtinSource(tool string) (tokenSource, bool) {
	if tool == "crush" {
		return tokenSource{session: crushDBSource{}}, true
	}
	return tokenSource{}, false
}

const (
	// crushMaxWalkUp bounds the search for the project root, so a watcher
	// started outside any project cannot walk to /.
	crushMaxWalkUp = 16
)

// crushDBPaths is the unique set of crush databases for dirs. Two spellings
// of one directory share one file; the walk that finds it is the same for
// a usage read and a session snapshot.
//
// The dedup folds each candidate once, not once per comparison: the set is
// walked for every path, and folding a path means scanning and copying it
// (NFC on macOS, Clean plus two case folds on Windows), so comparing the
// folded forms made the lookup quadratic in work per byte.
func crushDBPaths(dirs []string) []string {
	var paths, folded []string
	for _, dir := range dirs {
		path := crushDBPath(dir)
		if path == "" {
			continue
		}
		key := foldSpelling(path)
		if slices.ContainsFunc(folded, func(f string) bool { return spellingEqual(f, key) }) {
			continue
		}
		paths, folded = append(paths, path), append(folded, key)
	}
	return paths
}

// crushDBPath finds the database crush would use for a directory, walking up
// to the project root as crush does. It returns "" when there is none, which
// is the common case: most machines have never run crush. The path is
// returned with directory-path symlinks resolved, so the spellings one
// directory is watched under (on macOS always more than one) name it
// identically. A crush.db (or .crush directory) that is a symlink out of
// the project is refused: the store is writable by the agent, the same
// class of planted link the JSONL adapters already reject.
func crushDBPath(dir string) string {
	cur, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for range crushMaxWalkUp {
		if path := crushDBIn(cur); path != "" {
			return path
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
	return ""
}

const (
	// crushMillisCutoff is the smallest Unix-ms value that cannot be a Unix
	// second. Seconds of 1e11 are year 5138; milliseconds of 1e11 are 1973.
	// Crush's schema comment says milliseconds, but its update trigger writes
	// strftime('%s','now') (seconds), and Go writers use milliseconds.
	crushMillisCutoff int64 = 100_000_000_000
)

const crushDBRel = ".crush/crush.db"

// crushDBIn returns the crush database under root when OpenRoot can open it
// as a regular file. That refuses a crush.db or .crush directory whose
// symlink target leaves the project, while still allowing the project path
// itself to be a symlink (macOS /var).
//
// A store that is not there is the answer for most projects, so the opens
// below stay silent. The closes do not: this runs on the poll path, and a
// descriptor the kernel would not hand back is one per project per poll for
// the life of the dashboard, which shows up as an EMFILE refusal on every other
// store long before anything names this one.
//
// The Lstat prefilter is what makes the common answer cheap. crushDBPath walks
// up to crushMaxWalkUp levels per directory per poll, and on a machine that
// never ran crush every one of those levels used to open a root descriptor,
// open the store, stat it and close both before reporting that there is
// nothing there: two descriptors and four syscalls per level, on the poll path,
// for a store that is not there. One Lstat settles it instead, and it is not a
// weaker test of what the walk below accepts: a missing .crush, and a
// .crush/crush.db that is a directory, both leave nothing to open, while a
// .crush that is itself a symlink falls through to the OpenRoot check, which
// is what refuses a link whose target leaves the project.
func crushDBIn(root string) string {
	store := filepath.Join(root, filepath.FromSlash(crushDBRel))
	crushDir := filepath.Join(root, ".crush")
	if di, err := os.Lstat(crushDir); err != nil || !di.IsDir() {
		return ""
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return ""
	}
	defer func() {
		if cerr := r.Close(); cerr != nil {
			auditStoreLeak("crush", store, cerr)
		}
	}()
	f, err := r.Open(crushDBRel)
	if err != nil {
		return ""
	}
	fi, err := f.Stat()
	if cerr := f.Close(); cerr != nil {
		auditStoreLeak("crush", store, cerr)
	}
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	resolved := root
	if p, err := filepath.EvalSymlinks(root); err == nil {
		resolved = p
	}
	return filepath.Join(resolved, filepath.FromSlash(crushDBRel))
}

// The two counter columns are cast in the statement rather than left as the
// table declares them. SQLite gives a column the storage class of whatever was
// written into it, so a column crush declares INTEGER can hold a fraction or a
// string, and scanning either into an int64 is a conversion error that fails
// the statement and with it every reading of the store for as long as the row
// stands. CAST reads such a value as the zero it cannot be, and leaves a real
// counter, a NULL and a fraction's whole part alone. The id is not cast: it is
// read as text, and every storage class converts to text.
const crushSessionsQuery = `
	SELECT id, CAST(completion_tokens AS INTEGER), CAST(prompt_tokens AS INTEGER)
	FROM sessions`

const crushSessionsSinceQuery = crushSessionsQuery + `
	WHERE updated_at >= ? OR (updated_at <= ? AND updated_at >= ?)`

// sessions returns each database's current per-session completion and prompt
// tokens. Watch snapshots this at attach so a continued session contributes
// only what it adds afterwards, matching the file adapters. A tree that never
// ran crush contributes nothing: crushDBPaths only yields stores that exist.
//
// Any store that fails to read fails the whole call, and the result is all or
// nothing. Skipping it instead would leave the attach baseline without that
// store's counts, and every pre-attach session in it would be credited to this
// attach as growth the first time it does read. A transient SQLite lock
// therefore costs a poll, not a wrong number. A store that does not exist is
// not a failure at all; one that exists and will not read is audited, because
// reporting nothing for it forever is what an idle agent looks like.
//
// A zero since reads every session (the attach baseline). After that, only
// rows touched since attach: idle history is not re-scanned on every poll.
func (crushDBSource) sessions(dirs []string, since time.Time) (map[string]map[string]sessionCounts, bool) {
	out := map[string]map[string]sessionCounts{}
	for _, path := range crushDBPaths(dirs) {
		sess, ok := readCrushSessions(path, since)
		if !ok {
			return nil, false
		}
		out[path] = sess
	}
	return out, true
}

func readCrushSessions(path string, since time.Time) (_ map[string]sessionCounts, ok bool) {
	h, err := openStore(path)
	if err != nil {
		if !storeAbsent(path) {
			auditStoreRead("crush", path, err)
		}
		return nil, false
	}
	// The handle is held for this read, so a reader that is on it while
	// another read drops it, or the cache evicts it, still has a live
	// connection to finish on. Registered first so it runs last: the drop
	// below has to land before the release, or a handle the table has already
	// given up would sit closed-on-release with a reader still to come.
	defer releaseStore(h)
	db := h.db
	// A read that fails drops the shared handle rather than leaving it for the
	// next poll: a handle whose last read went wrong is replaced on the next
	// open, so a corrupt page or a database left mid-recovery costs one poll
	// instead of every poll after it. Registered before rows.Close so the rows
	// are closed first.
	defer func() {
		if !ok {
			closeStore(path)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), dbQueryTimeout)
	defer cancel()

	query := crushSessionsQuery
	var args []any
	if !since.IsZero() {
		query = crushSessionsSinceQuery
		ms := since.UnixMilli()
		args = append(args, ms, crushMillisCutoff, since.Unix())
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		if !storeAbsent(path) {
			auditStoreRead("crush", path, err)
		}
		return nil, false
	}
	defer rows.Close()

	out := map[string]sessionCounts{}
	for rows.Next() {
		// Every column is read as nullable, and a row missing any of them is
		// skipped rather than failing the statement. This is another program's
		// store, opened read-only and unconstrained, so one row toktop cannot
		// repair must not cost every reading of the agent for as long as it
		// survives. The id is the case that bites: a PRIMARY KEY column in a
		// rowid table may hold NULL unless it is declared NOT NULL, crush does
		// not declare its session id so, and scanning an absent id into a
		// string raises and takes the whole statement with it. The two counter
		// columns are nullable for the same reason, and a session with no id
		// cannot be named into the baseline map, so it is dropped and the rest
		// of the store still reads.
		var id sql.NullString
		var n, in sql.NullInt64
		if err := rows.Scan(&id, &n, &in); err != nil {
			auditStoreRead("crush", path, err)
			return nil, false
		}
		if !id.Valid {
			continue
		}
		c := sessionCounts{output: counter(n.Int64), input: counter(in.Int64)}
		if c.output > 0 || c.input > 0 {
			out[id.String] = c
		}
	}
	if err := rows.Err(); err != nil {
		auditStoreRead("crush", path, err)
		return nil, false
	}
	noteStoreReadOK("crush", path)
	return out, true
}
