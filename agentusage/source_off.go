// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build !sqlite

package agentusage

// builtinSource and setOpenCodeDB report that this build has no SQLite
// driver linked in, so no database-backed agent can be read however loudly
// it is asked for. One file, not two: both stubs share the !sqlite gate.
func builtinSource(string) (tokenSource, bool) { return tokenSource{}, false }

// setOpenCodeDB reports that this build has no SQLite driver linked in, so
// opencode's session database cannot be read however loudly it is asked for.
func setOpenCodeDB(bool) bool { return false }
