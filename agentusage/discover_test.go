// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import "testing"

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
