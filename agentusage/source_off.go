// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build !sqlite

package agentusage

// builtinSource reports that this build has no compiled-in source. Without the
// SQLite driver there is no database-backed agent to read, and a file-adapter
// agent is not a source.
func builtinSource(string) (tokenSource, bool) { return tokenSource{}, false }
