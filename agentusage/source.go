// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"sync"
	"time"
)

// usageSource reads usage for one working directory from somewhere other than
// an appendable transcript file. opencode keeps sessions in SQLite rather than
// JSONL, so it registers as a source instead of a file adapter.
//
// The contract is the same as everywhere else here: report what the agent
// recorded for this directory since this moment, or report nothing.
//
// dirs holds the spellings the process's directory can appear under, resolved
// and unresolved, because an agent records whichever it was started with and
// on macOS every temporary path is a symlink to another.
type usageSource interface {
	read(dirs []string, since time.Time) (values, bool)
}

// sessionCounts is one session's cumulative counters, snapshotted at attach
// so a continued session contributes only growth after that.
type sessionCounts struct {
	output int64
	input  int64
}

// sessionSource reports per-session cumulative counters. Watch snapshots
// them at attach so a continued session contributes only what it adds,
// matching the file adapters. A snapshot that fails is retried on the next
// poll rather than treated as "no sessions". The outer key is the database
// path, so two projects cannot collide on a session id.
//
// A zero since returns every session (the attach baseline). A non-zero
// since returns sessions written at or after that instant, which is enough
// to compute growth: untouched sessions contribute a zero delta.
type sessionSource interface {
	sessions(dirs []string, since time.Time) (map[string]map[string]sessionCounts, bool)
}

// tokenSource is a non-file usage reader. One struct, two shapes: usage
// (opencode) sums this attach's tokens via read; session (crush) snapshots
// per-session counters via sessions so a continued session contributes only
// growth. Exactly one shape is set per tool, decided by read/session below:
// usageSource/sessionSource stay separate interfaces because the two shapes
// have different methods and callers, and merging them would force every
// source to implement both. Kept, not cut: the audit's merge saves no lines
// once both method sets still exist.
type tokenSource struct {
	usage   usageSource
	session sessionSource
}

func (s tokenSource) present() bool {
	return s.usage != nil || s.session != nil
}

var (
	sourcesMu sync.RWMutex
	sources   = map[string]tokenSource{}
)

// builtinSource returns a source compiled into this build, or false. It is a
// build-tagged function rather than a registry row so no source is installed
// at module load: a built-in cannot be left behind by a runtime switch meant
// for another agent, and it needs no owner to dispose it. The registry holds
// only what a runtime switch put there (opencode).
func sourceFor(tool string) (tokenSource, bool) {
	sourcesMu.RLock()
	s, registered := sources[tool]
	sourcesMu.RUnlock()
	if registered {
		return s, s.present()
	}
	s, ok := builtinSource(tool)
	return s, ok && s.present()
}

// EnableOpenCodeDB turns reading of opencode's SQLite session store on or off.
//
// It is gated twice on purpose. The build tag `sqlite` decides whether the
// database driver is linked in at all, since it is a large dependency for one
// agent, and this switch decides whether a program that has it actually opens
// the operator's session database. Neither gate implies the other.
//
// Call it before Watch or Supported for "opencode": a watcher built while
// the store is off cannot be turned on later. It reports whether this build
// can read it: false means the binary was compiled without `-tags sqlite`,
// and nothing was enabled.
func EnableOpenCodeDB(on bool) bool { return setOpenCodeDB(on) }
