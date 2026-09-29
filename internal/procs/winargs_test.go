// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package procs

import (
	"slices"
	"testing"
)

// A Windows command line is split the way CommandLineToArgvW splits it, so the
// arguments here are the ones the program on the machine received. The cases
// that matter are the ones a light quote-aware split gets wrong: a quoted
// directory whose path ends in a backslash (the shape PowerShell hands back in
// Win32_Process.CommandLine), and a doubled backslash that really is one path
// separator.
func TestSplitWindowsArgsMatchesCommandLineToArgvW(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want []string
	}{
		{"plain", `C:\venv\Scripts\python.exe -m vllm`, []string{`C:\venv\Scripts\python.exe`, "-m", "vllm"}},
		{"quoted model path", `"C:\Program Files\Python\python.exe" -m vllm`,
			[]string{`C:\Program Files\Python\python.exe`, "-m", "vllm"}},
		{"quoted path ending in a separator", `"C:\models\\" serve`,
			[]string{`C:\models\`, "serve"}},
		// The escaped quote does not close the argument, so the rest of the
		// line belongs to it: that is the line the program itself sees.
		{"an unpaired backslash escapes the quote", `"C:\models\" serve`,
			[]string{`C:\models" serve`}},
		{"odd backslash run escapes the quote", `a b\"c d`, []string{"a", `b"c`, "d"}},
		{"empty quoted argument", `server --name "" --port 8000`,
			[]string{"server", "--name", "", "--port", "8000"}},
		{"repeated spaces separate once", "a   b\tc", []string{"a", "b", "c"}},
		{"trailing backslashes are literal", `C:\dir\\`, []string{`C:\dir\\`}},
		{"empty line has no arguments", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitWindowsArgs(c.cmd)
			if !slices.Equal(got, c.want) {
				t.Errorf("splitWindowsArgs(%q) = %q, want %q", c.cmd, got, c.want)
			}
		})
	}
}
