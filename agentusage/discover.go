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
	// AllDirs is set for a process that writes sessions for every project,
	// not only its own working directory. dsh web is one: the server's cwd
	// is the harness tree, and the sessions it is filling belong to the
	// projects it was asked to work in.
	AllDirs bool
}

// Watch starts reading usage for this process, using its agent name and
// working directory. A dsh process with AllDirs reads every session store,
// because that is the work the server is doing.
func (p Process) Watch(since time.Time) *Watcher {
	return openWatch(p.Tool, p.Dir, since, p.AllDirs && canonicalTool(p.Tool) == "dsh")
}

// dshHosts reports whether this command line is the dsh server. It writes
// every project's sessions; a watch of only the server's cwd sees none of
// them.
func dshHosts(argv []string) bool {
	for _, a := range argv {
		if a == "web" {
			return true
		}
	}
	return false
}

// Process discovery is implemented per GOOS: discover_linux.go walks /proc,
// discover_darwin.go asks ps(1) and lsof(8), and platforms where a process's
// working directory cannot be read without native calls (unsupported.go)
// report nothing at all. None of those shells out to a helper binary to
// enumerate processes: only ps(1) and lsof(8), and only on macOS, where
// there is no procfs to read.

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

// splitASCIISpace splits on the ASCII space ps used to join a command into one
// line, dropping the empty words the padding produces. strings.Fields would
// split on every rune Unicode calls a space, and an argument may legally hold
// one: a macOS agent launched from "/Users/me/My Agents/claude-code" arrives
// as three words, the first two of which are the only ones agentName reads,
// so a Unicode space in that path would hide the agent entirely.
func splitASCIISpace(s string) []string {
	var out []string
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, s[start:])
	}
	return out
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
// that is the key SpecFor and sourceFor look up.
func agentName(comm string, argv []string, known map[string]bool) string {
	if t := resolveAgent(canonicalTool(comm), known); t != "" {
		return t
	}
	for i, a := range argv {
		if i > 1 || a == "" {
			break
		}
		if t := resolveAgent(canonicalTool(filepath.Base(a)), known); t != "" {
			return t
		}
	}
	return ""
}

// resolveAgent maps a process title onto the agent whose transcripts we read.
// Kimi Code's binary and argv0 are kimi-code; the adapter and the store are
// registered as kimi. kimi-code is not its own tool.
func resolveAgent(name string, known map[string]bool) string {
	if name == "kimi-code" {
		name = "kimi"
	}
	if known[name] {
		return name
	}
	return ""
}
