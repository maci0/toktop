// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build sqlite

package agentusage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"modernc.org/sqlite"
)

func init() {
	sqlite.MustRegisterCollationUtf8("toktop_directory", func(left, right string) int {
		return strings.Compare(strings.ToLower(left), strings.ToLower(right))
	})
}

// opencode keeps its sessions in SQLite rather than the JSONL every other
// agent here writes: ~/.local/share/opencode/opencode.db, with one row per
// message and the usage in a JSON column.
//
//	session.directory  the working directory, which is the attribution key
//	message.data       {"role":"assistant","tokens":{"output":324,"reasoning":52,…}}
//	message.time_created  milliseconds, which bounds the since filter
//
// The database is opened read-only for each reading and closed again, so a
// long-lived dashboard never holds a handle on a database the agent is
// writing. Sessions for this directory are few; their messages are found
// through the session_id index rather than a scan of the message table.
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
// opencode itself does.
func openCodeDBPath() string {
	if data := os.Getenv("XDG_DATA_HOME"); data != "" {
		return filepath.Join(data, "opencode", "opencode.db")
	}
	dir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, ".local", "share", "opencode", "opencode.db")
}

// usageQuery sums what this directory's sessions spent after a point in time.
// Cached reads are deliberately absent: they are cache hits, not billed
// prompt. tokens.input, when present, is billed prompt and is summed.
// Only assistant messages carry tokens; other roles are prompts. The directory
// list is spliced in by usageQueryFor, since SQL has no placeholder for a set.
// MAX(CAST(…), 0) floors a negative stored counter at zero so one malformed
// row cannot subtract from sessions that read fine.
//
// Sessions are the outer loop, then messages via message_session_idx
// (session_id). CROSS JOIN stops SQLite from reversing that into a scan of
// message: opencode indexes session_id, not (session_id, time_created) and
// not directory. CAST keeps MAX numeric: json_extract of a JSON string is
// TEXT, and SQLite ranks TEXT above INTEGER, so MAX('9', 100) would be '9'.
const usageQuery = `
	SELECT
		COALESCE(SUM(MAX(CAST(json_extract(m.data, '$.tokens.output') AS INTEGER), 0)), 0),
		COALESCE(SUM(MAX(CAST(json_extract(m.data, '$.tokens.reasoning') AS INTEGER), 0)), 0),
		COALESCE(MAX(CAST(json_extract(m.data, '$.tokens.total') AS INTEGER)), 0),
		COALESCE(SUM(MAX(CAST(json_extract(m.data, '$.tokens.input') AS INTEGER), 0)), 0)
	FROM session
	CROSS JOIN message m
	WHERE m.session_id = session.id
	  AND %s AND m.time_created > ?
	  AND json_extract(m.data, '$.role') = 'assistant'`

// foldSessionDirectory compares session.directory case-insensitively and with
// either path separator. NTFS and the default APFS configuration look names
// up that way; an agent records whichever spelling its runtime produced.
// Linux compares bytes. Tests may flip it.
var foldSessionDirectory = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

// usageQueryFor builds the query for n directory spellings. Only the number of
// placeholders varies: every value still travels as a bound parameter.
func usageQueryFor(n int) string {
	return fmt.Sprintf(usageQuery, directoryPred(n))
}

func directoryPred(n int) string {
	ph := strings.TrimSuffix(strings.Repeat("?,", n), ",")
	pred := "session.directory IN (" + ph + ")"
	if foldSessionDirectory {
		pred = "(" + pred + " OR replace(session.directory, char(92), '/') COLLATE toktop_directory IN (" + ph + "))"
	}
	return pred
}

func (o openCodeDBSource) read(dirs []string, since time.Time) (values, bool) {
	if o.path == "" || len(dirs) == 0 {
		return values{}, false
	}
	// mode=ro leaves the database alone; a missing or unreadable one is an
	// answer ("nothing to report"), not an error worth surfacing, since most
	// machines running this have no opencode at all.
	db, err := openReadOnly(o.path)
	if err != nil {
		return values{}, false
	}
	defer db.Close()

	args := make([]any, 0, len(dirs)*2+1)
	for _, d := range dirs {
		args = append(args, d)
	}
	if foldSessionDirectory {
		for _, d := range dirs {
			args = append(args, strings.ToLower(filepath.ToSlash(d)))
		}
	}
	args = append(args, since.UnixMilli())

	ctx, cancel := context.WithTimeout(context.Background(), dbQueryTimeout)
	defer cancel()

	var out, thinking, total, input sql.NullInt64
	row := db.QueryRowContext(ctx, usageQueryFor(len(dirs)), args...)
	if err := row.Scan(&out, &thinking, &total, &input); err != nil {
		return values{}, false
	}
	v := values{
		output:   counter64(out.Int64),
		thinking: counter64(thinking.Int64),
		total:    counter64(total.Int64),
		input:    counter64(input.Int64),
	}
	return v, v.present()
}
