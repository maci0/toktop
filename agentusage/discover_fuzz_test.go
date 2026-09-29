// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"
)

// FuzzAgentCommandLine drives the process classification every platform's
// discovery ends with: splitASCIISpace, agentName and dshHosts, fed the two
// spellings the platforms hand over (a NUL-separated /proc cmdline already
// split into words, and the single line ps prints on macOS).
//
// Both inputs belong to the processes on the host, not to toktop: any program
// the operator runs chooses its own comm and argv, and a misread name is not a
// cosmetic listing error. The name decides which transcript store is opened
// and which project the tokens in it are billed to, and the all-dirs flag
// decides whether one dsh process attributes every project's sessions or only
// its own. So a returned name has to be a known agent and a canonical spelling
// of it, the comm has to win over the command line, a name may only be read
// off a whole path component of the first two words, and the two spellings of
// one command line have to classify the same.
func FuzzAgentCommandLine(f *testing.F) {
	for _, seed := range []struct {
		comm    string
		cmdline string
	}{
		{"claude", "claude\x00--dangerously-skip-permissions"},
		{"node", "/usr/local/bin/node\x00/opt/agents/claude-code/cli.js"},
		{"node", "/usr/local/bin/node\x00/opt/agents/kimi-code"},
		{"kimi-code", "kimi-code"},
		{"", ""},
		{"", "\x00"},
		{"", " \x00 \x00claude\x00"},
		{"  claude  ", "claude"},
		{"claude-code", ""},
		{"bash", "-c\x00claude --print"},
		{"sh", "/bin/sh\x00-c\x00claude\x00extra\x00claude"},
		{"dsh", "dsh\x00web"},
		{"dsh", "dsh\x00run\x00web"},
		{"dsh", "dsh\x00webhook"},
		{"Web Content", "/Applications/My Agents/claude"},
		{"café", "café"},
		{"café", "café"},
		{"claude", "/a/b/claude/\x00x"},
		{"claude", "/a/b/claude\x00"},
		{"claude", "/a/b/claude.exe\x00x"},
		{"claude", "\x00claude\x00x"},
		{"\x00", "\x00"},
		{"claude ", " claude "},
		{"node", "/x/node\x00/x/node_modules/@anthropic-ai/claude-code/cli.js"},
		{"gemini", "gemini\x00--yolo"},
		{"ollama", "ollama\x00serve"},
		{"node", "/usr/bin/node\x00/home/me/My Agents/claude-code"},
		{"", "web"},
		{"dsh", "dsh\x00web\x00"},
		{"dsh", "dsh\x00web\x00\x00"},
	} {
		f.Add(seed.comm, seed.cmdline)
	}

	f.Fuzz(func(t *testing.T, comm, cmdline string) {
		known := knownNames()
		argv := splitCmdline(cmdline)
		what := fmt.Sprintf("comm=%q cmdline=%q", comm, cmdline)

		tool := agentName(comm, argv, known)
		assertKnownAgent(t, what, tool, known)
		if again := agentName(comm, argv, known); again != tool {
			t.Fatalf("%s: agentName = %q then %q", what, tool, again)
		}
		// The comm is the kernel's own name for the process, so it is the
		// strongest evidence available and outranks the command line.
		fromComm := resolveAgent(canonicalTool(comm), known)
		if fromComm != "" && fromComm != tool {
			t.Fatalf("%s: agentName = %q, but the comm alone names %q", what, tool, fromComm)
		}
		// Only the first two words are read, and only as whole path
		// components, so a shell that merely mentions an agent further along
		// its command line is not one. A name that came back has to sit on one
		// of those two words; an agent named only further along is expected to
		// go unread, which is the whole point of the limit.
		if tool != "" && fromComm == "" && !namesPathComponent(argv[:min(2, len(argv))], tool, known) {
			t.Fatalf("%s: agentName = %q, which is no word's path component", what, tool)
		}

		// The macOS spelling of the same command line: ps prints it space
		// joined, and the walk splits it back into words before classifying.
		// Quoting is lost in the process, so the two spellings are allowed to
		// disagree on which agent they find; each has to answer a known name.
		psLine := strings.Join(argv, " ")
		words := splitASCIISpace(psLine)
		assertASCIISplit(t, psLine, words)
		assertKnownAgent(t, what+" ps", agentName(comm, words, known), known)

		// All-dirs is a dsh-only claim about a command line holding the exact
		// word "web", not one it merely contains: a subcommand spelled in
		// another case, or as a prefix of a longer word, is a different
		// argument and the sessions it starts belong to one project.
		if dshHosts(argv) {
			raised := slices.Clone(argv)
			for i, a := range raised {
				if a == "web" {
					raised[i] = "WEB"
				}
			}
			if dshHosts(raised) {
				t.Fatalf("%s: dshHosts(%q) claims all-dirs for a word that is not exactly web", what, raised)
			}
		}
	})
}

// splitCmdline reads a /proc/PID/cmdline body the way the Linux walk does: the
// kernel separates the arguments with NUL bytes and terminates the last one
// with another, and an empty body is no arguments at all rather than one empty
// argument.
func splitCmdline(cmdline string) []string {
	trimmed := strings.TrimRight(cmdline, "\x00")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\x00")
}

// assertKnownAgent fails unless tool is the empty answer or a known agent named
// in the canonical spelling SpecFor and sourceFor look it up under. A name that
// came back with surrounding whitespace or in a decomposed spelling resolves to
// no store at all, so the watcher would attach to nothing.
func assertKnownAgent(t *testing.T, what, tool string, known map[string]bool) {
	t.Helper()
	if tool == "" {
		return
	}
	if !known[tool] {
		t.Fatalf("%s: %q is not a known agent", what, tool)
	}
	if canonical := canonicalTool(tool); canonical != tool {
		t.Fatalf("%s: %q is not canonical, the lookup key is %q", what, tool, canonical)
	}
	if norm.NFC.String(tool) != tool {
		t.Fatalf("%s: %q is not NFC", what, tool)
	}
	// kimi-code is the binary's name; the tool it registers under is kimi, and
	// returning the binary's own name would resolve to no adapter.
	if tool == "kimi-code" {
		t.Fatalf("%s: returned kimi-code, which is not its own tool", what)
	}
}

// namesPathComponent reports whether tool is the agent one of argv's words
// names, which is the only way a command line is allowed to name an agent: the
// agent has to be a whole component of the path, so a shell that merely spells
// it inside a longer word, or a directory on the way to it, is not one.
func namesPathComponent(argv []string, tool string, known map[string]bool) bool {
	return agentInWords(argv, known) == tool
}

// agentInWords returns the agent any of argv's words names as a whole path
// component, or "" when none does.
func agentInWords(argv []string, known map[string]bool) string {
	for _, a := range argv {
		for _, comp := range strings.Split(filepath.ToSlash(a), "/") {
			if t := resolveAgent(canonicalTool(comp), known); t != "" {
				return t
			}
		}
	}
	return ""
}

// assertASCIISplit pins the split ps output goes through. Only the ASCII space
// separates, and the empty words the padding produces are dropped, so the words
// are exactly the pieces between runs of spaces. A Unicode space is an ordinary
// character here on purpose: an agent launched from a path holding one arrives
// as a single word, and a split that broke there would hide the agent entirely
// and report the host's directory as the process's.
func assertASCIISplit(t *testing.T, s string, words []string) {
	t.Helper()
	for i, w := range words {
		if w == "" {
			t.Fatalf("splitASCIISpace(%q) produced an empty word at %d: %q", s, i, words)
		}
		if strings.Contains(w, " ") {
			t.Fatalf("splitASCIISpace(%q) left a space inside word %d: %q", s, i, w)
		}
	}
	if want := strings.FieldsFunc(s, func(r rune) bool { return r == ' ' }); !slices.Equal(words, want) {
		t.Fatalf("splitASCIISpace(%q) = %q, want %q", s, words, want)
	}
}
