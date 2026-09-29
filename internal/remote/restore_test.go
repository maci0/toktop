// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package remote

import (
	"runtime"
	"strings"
	"testing"
)

// A store Windows cannot repair is a store the operator loses the pins in, so
// the command the error carries has to be one their shell runs. `cp` is not a
// command on Windows, and the POSIX single-quote escaping the hint used is not
// syntax PowerShell reads, so the two platforms cannot share one spelling.
func TestRestoreCommandRunsOnTheHostShell(t *testing.T) {
	cmd := restoreCommand("/home/a b/known_hosts.old", "/home/a b/known_hosts")
	switch runtime.GOOS {
	case "windows":
		if !strings.HasPrefix(cmd, "Copy-Item -LiteralPath ") {
			t.Errorf("windows has no cp; got %q", cmd)
		}
		if !strings.Contains(cmd, " -Force") {
			t.Errorf("the restore must overwrite a store that does not parse; got %q", cmd)
		}
	default:
		if !strings.HasPrefix(cmd, "cp ") {
			t.Errorf("a posix host spells the restore as cp; got %q", cmd)
		}
	}
	if strings.Count(cmd, "/home/a b/known_hosts") != 2 {
		t.Errorf("both paths belong in the command, quoted whole: %q", cmd)
	}
}

// A path carrying a quote is what separates quoting the paths from merely
// printing them: unquoted it closes the word and the tail of the command runs
// as something else.
func TestRestoreCommandQuotesAQuoteInThePath(t *testing.T) {
	dir := `/home/o'brien`
	cmd := restoreCommand(dir+"/known_hosts.old", dir+"/known_hosts")
	// Both paths are named, and the embedded quote is escaped rather than left
	// to close the word: POSIX closes and reopens around it, PowerShell
	// doubles it. A path cannot be matched whole once escaped, because
	// escaping splits it across the word boundary by design, so each is
	// checked on the escaped tail it ends in.
	esc := `'\''`
	if runtime.GOOS == "windows" {
		esc = "''"
	}
	for _, want := range []string{esc + "brien/known_hosts.old'", esc + "brien/known_hosts'"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("escaped path %q belongs in the command: %q", want, cmd)
		}
	}
}
