// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build sqlite

package agentusage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
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
func crushDBPaths(dirs []string) []string {
	seen := make(map[string]bool, len(dirs))
	var paths []string
	for _, dir := range dirs {
		path := crushDBPath(dir)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
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
func crushDBIn(root string) string {
	r, err := os.OpenRoot(root)
	if err != nil {
		return ""
	}
	defer r.Close()
	f, err := r.Open(crushDBRel)
	if err != nil {
		return ""
	}
	fi, err := f.Stat()
	f.Close()
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	resolved := root
	if p, err := filepath.EvalSymlinks(root); err == nil {
		resolved = p
	}
	return filepath.Join(resolved, filepath.FromSlash(crushDBRel))
}

const crushSessionsQuery = `
	SELECT id, completion_tokens, prompt_tokens
	FROM sessions`

const crushSessionsSinceQuery = crushSessionsQuery + `
	WHERE updated_at >= ? OR (updated_at <= ? AND updated_at >= ?)`

// sessions returns each database's current per-session completion and prompt
// tokens. Watch snapshots this at attach so a continued session contributes
// only what it adds afterwards, matching the file adapters. A missing or
// unreadable store is not an error: most trees have never run crush.
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

func readCrushSessions(path string, since time.Time) (map[string]sessionCounts, bool) {
	db, err := openReadOnly(path)
	if err != nil {
		return nil, false
	}
	defer db.Close()

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
		return nil, false
	}
	defer rows.Close()

	out := map[string]sessionCounts{}
	for rows.Next() {
		var id string
		var n, in sql.NullInt64
		if err := rows.Scan(&id, &n, &in); err != nil {
			return nil, false
		}
		c := sessionCounts{output: int64(counter64(n.Int64)), input: int64(counter64(in.Int64))}
		if c.output > 0 || c.input > 0 {
			out[id] = c
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false
	}
	return out, true
}
