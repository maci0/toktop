// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"math/rand/v2"
	"slices"
	"testing"
)

// Discover documents one order on every platform, so the assertion belongs
// where it covers all of them rather than beside the one reader that used to
// be the only one sorting: a reader that hands matches back in its source's
// order (ps on macOS) otherwise breaks the promise silently, and the package
// only gets tested on the platform that already sorted.
func TestDiscoverIsOrderedByPID(t *testing.T) {
	got := Discover()
	for i := 1; i < len(got); i++ {
		if got[i-1].PID > got[i].PID {
			t.Fatalf("Discover returned pid %d before %d: %v", got[i-1].PID, got[i].PID, got)
		}
	}
}

// The same guarantee, on a listing no platform can supply: the sort is what
// turns an unordered result into a documented one, so it is checked directly
// rather than only where a machine happens to have agents running.
func TestSortByPIDOrdersAnyListing(t *testing.T) {
	procs := make([]Process, 0, 64)
	for i := range cap(procs) {
		procs = append(procs, Process{PID: i + 1})
	}
	rand.Shuffle(len(procs), func(i, j int) { procs[i], procs[j] = procs[j], procs[i] })
	sortByPID(procs)
	pids := make([]int, len(procs))
	for i, p := range procs {
		pids[i] = p.PID
	}
	if !slices.IsSorted(pids) {
		t.Fatalf("sortByPID left the listing unordered: %v", pids)
	}
}

func TestAgentName(t *testing.T) {
	known := map[string]bool{"claude": true, "codex": true, "node": false}

	cases := []struct {
		name string
		comm string
		argv []string
		want string
	}{
		{"kernel name wins", "claude", []string{"/opt/node", "/x/claude"}, "claude"},
		{"argv0 basename", "", []string{"/usr/local/bin/codex", "--full-auto"}, "codex"},
		{"node script in argv1", "", []string{"node", "/home/u/.local/bin/codex", "--full-auto"}, "codex"},
		{"extension is not stripped", "", []string{"node", "/home/u/.claude/local/claude.js"}, ""},
		{"later mention is not the agent", "", []string{"node", "server.js", "--agent=claude"}, ""},
		{"empty argv", "bash", nil, ""},
		{"unknown everything", "vim", []string{"vim", "notes.txt"}, ""},
		{"comm whitespace trimmed", " claude ", []string{}, "claude"},
	}
	for _, tc := range cases {
		if got := agentName(tc.comm, tc.argv, known); got != tc.want {
			t.Errorf("%s: agentName(%q, %q) = %q, want %q", tc.name, tc.comm, tc.argv, got, tc.want)
		}
	}
}

// An agent whose name carries an accent is registered in whatever form its
// definition was written in, but the process name comes from the file system
// and a macOS one hands back the decomposed spelling. Byte equality missed it
// and the running agent never appeared.
func TestAgentNameNormalization(t *testing.T) {
	const precomposed = "caf\u00e9" // café, one code point
	const decomposed = "cafe\u0301" // e + combining acute
	known := map[string]bool{precomposed: true}

	for _, form := range []string{precomposed, decomposed} {
		if got := agentName(form, nil, known); got != precomposed {
			t.Errorf("agentName(comm=%q) = %q, want %q", form, got, precomposed)
		}
		if got := agentName("", []string{"/usr/local/bin/" + form}, known); got != precomposed {
			t.Errorf("agentName(argv0=%q) = %q, want %q", form, got, precomposed)
		}
	}
	// A name that is neither spelling of a known agent is still unknown.
	if got := agentName("cafe\u0301x", nil, known); got != "" {
		t.Errorf("agentName matched an unregistered name: %q", got)
	}
}

func TestAgentNameKimiCode(t *testing.T) {
	if got := agentName("kimi-code", []string{"kimi-code"}, knownNames()); got != "kimi" {
		t.Fatalf("agentName(kimi-code) = %q, want kimi", got)
	}
	if got := agentName("", []string{"/usr/local/bin/kimi-code"}, knownNames()); got != "kimi" {
		t.Fatalf("agentName(argv0 kimi-code) = %q, want kimi", got)
	}
	if Supported("kimi-code") {
		t.Fatal("kimi-code must not be its own tool")
	}
}

func TestKnownNamesUsesCanonicalForm(t *testing.T) {
	known := knownNames()
	for _, name := range Agents() {
		c := canonicalTool(name)
		if !known[c] {
			t.Errorf("knownNames is missing %q (canonical %q)", name, c)
		}
	}
}
