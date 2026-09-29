// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build !windows

package remote

import "strings"

// restoreCommand renders the command an operator runs to put a recovered copy
// of the store back. It is a POSIX `cp`, spelled for the shell that ships
// there.
func restoreCommand(copy, path string) string {
	return "cp " + shellWord(copy) + " " + shellWord(path)
}

// shellWord renders one path as a single POSIX shell word. Both paths here
// come from $XDG_CONFIG_HOME or $HOME, so a home directory named
// "/home/a b" reached the operator as a command that copied toktop.old to
// /home/a, and a directory whose name carries a quote or a $() reached it as
// something to paste. The hint is a command the operator is meant to run, so
// it has to survive the shell it is printed into: single quotes cover every
// byte but the single quote itself, which closes and reopens the word.
func shellWord(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
