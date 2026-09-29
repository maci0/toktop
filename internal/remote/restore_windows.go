// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build windows

package remote

import "strings"

// restoreCommand renders the command an operator runs to put a recovered copy
// of the store back. Windows has no `cp`, and the store lives under %AppData%
// where a path carries a drive letter, spaces, and backslashes, so the hint is
// the PowerShell cmdlet every supported Windows release ships.
func restoreCommand(copy, path string) string {
	return "Copy-Item -LiteralPath " + powershellWord(copy) +
		" -Destination " + powershellWord(path) + " -Force"
}

// powershellWord renders one path as a single PowerShell argument. -LiteralPath
// stops PowerShell from reading the path as a wildcard, which a directory named
// "a[b]" otherwise is, and single quotes cover every byte but the single quote
// itself, which is doubled inside them.
func powershellWord(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
