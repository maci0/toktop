// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package procs

import "strings"

// splitWindowsArgs splits a Windows command line into arguments the way
// CommandLineToArgvW does, because that is the parser the programs on this
// machine already split themselves with: Win32_Process reports the raw line,
// and the engine matchers compare what came out of it against a program name
// and a model path.
//
// It lives apart from the CIM query that feeds it so the rule is testable on
// every platform, not only on the one that can produce a command line to split.

// splitWindowsArgs returns the arguments of cmd, the executable included.
// A quoted run keeps the spaces inside it, backslash runs stay literal, and a
// backslash run into a quote pairs up: two backslashes and the quote is a
// delimiter, an odd one escapes the quote and the rest stands. A quoted
// directory therefore ends in "C:\models\" without closing the argument on its
// last backslash, and an empty "" is the empty argument the program received.
func splitWindowsArgs(cmd string) []string {
	var (
		args    []string
		cur     strings.Builder
		started bool
		slashes int
		inQ     bool
	)
	flush := func() {
		if started {
			args = append(args, cur.String())
			cur.Reset()
			started = false
		}
	}
	// Byte by byte, not rune by rune: only the four ASCII delimiters below are
	// special, so ranging over the string would rewrite every byte a line
	// carries that is not valid UTF-8 into U+FFFD and hand the matchers a
	// program name or model path that is no longer the one on the machine.
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case c == '\\':
			slashes++
		case c == '"':
			cur.WriteString(strings.Repeat(`\`, slashes/2))
			if slashes%2 == 1 {
				cur.WriteByte('"')
			} else {
				inQ = !inQ
			}
			slashes = 0
			started = true
		case (c == ' ' || c == '\t') && slashes == 0 && !inQ:
			flush()
		default:
			cur.WriteString(strings.Repeat(`\`, slashes))
			slashes = 0
			cur.WriteByte(c)
			started = true
		}
	}
	// A line ending in backslashes keeps them: the run is only paired up
	// against a quote, and there is none here.
	cur.WriteString(strings.Repeat(`\`, slashes))
	flush()
	return args
}
