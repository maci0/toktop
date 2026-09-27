// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"path/filepath"
	"time"
)

// Process is one running agent CLI.
type Process struct {
	// PID is the OS process identifier.
	PID int
	// Tool is the agent name (claude, codex, pi, …).
	Tool string
	// Dir is the process's working directory, which is what attributes a
	// transcript to it.
	Dir string
	// Started is when the process began, as far as the OS reports it; the
	// zero time when the platform does not report it.
	Started time.Time
}

// Watch starts reading usage for this process, using its agent name and
// working directory. Equivalent to [Watch](p.Tool, p.Dir, since).
func (p Process) Watch(since time.Time) *Watcher {
	return Watch(p.Tool, p.Dir, since)
}

// Process discovery is implemented per GOOS: discover_linux.go walks /proc,
// discover_darwin.go asks ps(1) and lsof(8), and platforms where a process's
// working directory cannot be read without native calls (discover_windows.go,
// discover_other.go) report nothing at all. None of those implementations
// shell out to pgrep-style helpers to find the processes themselves: spawning
// a process to count processes is how a monitor ends up measuring itself.

// knownNames is the set of agent names a discovered process is matched
// against, keyed the way agentName looks them up: canonical (NFC, trimmed).
// Building it in one place keeps every platform's discovery walking the same
// convention as the registry it reads from.
func knownNames() map[string]bool {
	known := make(map[string]bool, len(knownAgents))
	for _, a := range Agents() {
		known[canonicalTool(a)] = true
	}
	return known
}

// agentName names the known agent a process is running, or "" when it is not
// one.
//
// The executable name alone is not enough: agents ship as node and bun scripts,
// so the process is called "node" and the agent name is in the command line.
// Both the kernel's short name (comm / ucomm) and the first two command-line
// words are checked, and only whole path components count, so a shell that
// merely mentions an agent in a later argument is not mistaken for one.
//
// Candidates are canonicalized the way definitions are, so the process name
// and the registered name are compared in one form. A binary named with a
// decomposed accent ("cafe" + U+0301, what a macOS file system stores) is
// otherwise missed by an agent registered precomposed in agents.json, and
// the running agent never shows up. The canonical spelling is returned, since
// that is the key definedSpec and sourceFor look up.
func agentName(comm string, argv []string, known map[string]bool) string {
	if t := canonicalTool(comm); known[t] {
		return t
	}
	for i, a := range argv {
		if i > 1 || a == "" {
			break
		}
		if t := canonicalTool(filepath.Base(a)); known[t] {
			return t
		}
	}
	return ""
}
