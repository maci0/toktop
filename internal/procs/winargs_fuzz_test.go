// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package procs

import (
	"slices"
	"strings"
	"testing"
)

// quoteWindowsArg is the inverse of splitWindowsArgs: it writes one argument
// the way a program would have to write it on a command line for the split to
// hand that argument back. A backslash run only doubles when a quote follows
// it or when it ends the argument, because those are the two places a lone
// backslash would otherwise pair up against the quote.
func quoteWindowsArg(arg string) string {
	if arg == "" {
		return `""`
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(arg); {
		if arg[i] != '\\' {
			// A quote inside the argument needs a backslash of its own, or the
			// split reads it as a delimiter.
			if arg[i] == '"' {
				b.WriteString(`\"`)
			} else {
				b.WriteByte(arg[i])
			}
			i++
			continue
		}
		run := i
		for i < len(arg) && arg[i] == '\\' {
			i++
		}
		n := i - run
		switch {
		case i == len(arg):
			// The closing quote pairs up with half the run, so the run doubles
			// and the quote ends the argument.
			b.WriteString(strings.Repeat(`\`, 2*n))
		case arg[i] == '"':
			// The quote is part of the argument, so it needs an odd run in
			// front of it to escape it.
			b.WriteString(strings.Repeat(`\`, 2*n+1))
		default:
			b.WriteString(strings.Repeat(`\`, n))
		}
		if i < len(arg) && arg[i] == '"' {
			b.WriteByte('"')
			i++
		}
	}
	b.WriteByte('"')
	return b.String()
}

// FuzzSplitWindowsArgs drives the CommandLineToArgvW split over arbitrary
// command lines. The input is a raw Win32_Process.CommandLine, so a process
// name the user does not control picks the backslash runs, the quote nesting
// and the length: the parser is the one hand-rolled tokenizer in the tree with
// no length cap, and an escaped quote that fails to pair up swallows the rest
// of the line into one argument.
//
// Two things have to hold. Splitting is deterministic, and a quoted form of
// the result splits back to the same arguments, so no run of backslashes, no
// quote and no separator is read as something other than what it is. The
// result is also never longer than the line: the split pairs backslashes up
// and never writes one the line did not have, so a long run cannot grow into
// a longer argument than the input that produced it.
func FuzzSplitWindowsArgs(f *testing.F) {
	for _, seed := range []string{
		"",
		" ",
		"\t\t",
		`C:\venv\Scripts\python.exe -m vllm`,
		`"C:\Program Files\Python\python.exe" -m vllm`,
		`"C:\models\\" serve`,
		`"C:\models\" serve`,
		`a b\"c d`,
		`server --name "" --port 8000`,
		`a   b\tc`,
		`C:\dir\\`,
		`"""`,
		`""""""`,
		`\`,
		`\\`,
		`\\\`,
		`a\\\"b`,
		`"`,
		`" " "`,
		`"\`,
		"\"\t\\\"",
		strings.Repeat(`\`, 257),
		strings.Repeat(`\`, 4096) + `"`,
		`"` + strings.Repeat(`\`, 4095),
		strings.Repeat(`"\`, 512),
		"héllo wörld",
		"\x00\x01\x7f",
		strings.Repeat("a b", 1000),
		strings.Repeat(`"`, 200),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, cmd string) {
		got := splitWindowsArgs(cmd)

		again := splitWindowsArgs(cmd)
		if !slices.Equal(got, again) {
			t.Fatalf("splitWindowsArgs(%q) is not deterministic: %q then %q", cmd, got, again)
		}

		total := 0
		for _, a := range got {
			total += len(a)
		}
		if total > len(cmd) {
			t.Fatalf("splitWindowsArgs(%q) returned %d bytes of argument, more than the %d the line holds: %q",
				cmd, total, len(cmd), got)
		}
		if len(got) > len(cmd)+1 {
			t.Fatalf("splitWindowsArgs(%q) returned %d arguments from a line of %d bytes: %q", cmd, len(got), len(cmd), got)
		}

		quoted := make([]string, len(got))
		for i, a := range got {
			quoted[i] = quoteWindowsArg(a)
		}
		if round := splitWindowsArgs(strings.Join(quoted, " ")); !slices.Equal(round, got) {
			t.Fatalf("splitWindowsArgs(%q) = %q, which re-quoted as %q splits back to %q",
				cmd, got, strings.Join(quoted, " "), round)
		}
	})
}
