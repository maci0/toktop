// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"golang.org/x/text/unicode/norm"
)

// knownAgents are the CLIs this package recognizes by name.
//
// Recognition and readability are separate questions: an agent here is one
// whose process can be identified, which is what makes it appear in Discover.
// Whether its tokens can be read is decided by the adapters in registry.go, and
// most of this list keeps no transcript worth reading.
var knownAgents = []string{
	"agy", "claude", "clanker", "codex", "copilot", "crush", "cursor-agent",
	"dsh", "feynman", "gemini", "grok", "kimi", "omp", "opencode", "pi",
	"prime-agent", "qwen",
}

// Agents lists every agent name this package knows, built in and defined.
func Agents() []string {
	out := slices.Clone(knownAgents)
	defsMu.RLock()
	for name := range defs {
		out = append(out, name)
	}
	defsMu.RUnlock()
	adaptersMu.RLock()
	for name := range adapters {
		out = append(out, name)
	}
	adaptersMu.RUnlock()

	slices.Sort(out)
	return slices.Compact(out)
}

// Spec describes where a defined agent keeps its transcripts, so live usage
// works for agents this package was not compiled to know about (pi and the
// CLIs built on it, in-house wrappers). The records are parsed generically:
// any JSONL whose objects carry recognizable token counters works, and one
// whose objects do not simply reports nothing.
type Spec struct {
	// Roots are directories to search, with ~ expanded. Blank entries are
	// ignored, and a spec with none left is not usable.
	//
	// {dir} in a root stands for the agent process's working directory, which
	// is what an agent keeping its transcripts inside the project it works in
	// needs (clanker keeps one under state/):
	//
	//	{"roots": ["{dir}/state"]}
	//
	// Roots name directories; the suffix chooses the files inside them.
	Roots []string `json:"roots"`
	// Suffix filters transcript files (default ".jsonl").
	Suffix string `json:"suffix,omitempty"`
	// Suffixes matches several extensions, for an agent that writes more than
	// one (compressed by default, plain when compression is off). It replaces
	// Suffix when set.
	Suffixes []string `json:"suffixes,omitempty"`
	// Cumulative says the counters already include everything before them, so
	// the first value seen becomes a baseline. Default is per message.
	Cumulative bool `json:"cumulative,omitempty"`

	// HeaderCwd says the working directory appears once in a session header
	// rather than on every record, so ownership is decided from the head of
	// the file. Without it, a transcript whose usage lines carry no cwd is
	// attributed by location alone.
	HeaderCwd bool `json:"header_cwd,omitempty"`
}

// canonicalTool is the map key for an agent name. Surrounding whitespace is
// not part of the name, and would otherwise make Watch miss a spec registered
// as "claude " while Discover reports "claude". The name is composed to NFC
// so "café" spelled NFD (e + combining acute, typical of a macOS-typed
// definitions file) and NFC (precomposed JSON) are one agent.
func canonicalTool(tool string) string {
	return norm.NFC.String(strings.TrimSpace(tool))
}

// specRoots is the usable transcript directories in spec. Blank entries are
// not roots, matching RegisterSpec.
func specRoots(spec Spec) []string {
	out := make([]string, 0, len(spec.Roots))
	for _, r := range spec.Roots {
		if strings.TrimSpace(r) != "" {
			out = append(out, r)
		}
	}
	return out
}

var (
	defsMu sync.RWMutex
	// The pi family keeps ordinary JSONL transcripts, so they need locations
	// rather than adapters. They ship here so both this tool and gauntlet read
	// them without a definitions file.
	defs = map[string]Spec{
		"pi":          {Roots: []string{"~/.pi/agent/sessions"}},
		"prime-agent": {Roots: []string{"~/.prime/agent/sessions"}},
		"feynman":     {Roots: []string{"~/.feynman/sessions"}},
	}
)

// SpecFor reports the transcript location registered for an agent, whether it
// was compiled in (the pi family) or loaded by [LoadDefinitions], and whether
// there is one at all. It is how a program that wrote a definitions file finds
// out what that file registered, since [LoadDefinitions] says nothing about
// entries it skipped and a skipped entry is otherwise indistinguishable from
// one that was never in the file.
//
// The name is canonicalized like everywhere else, so " pi " and "pi" name the
// same agent. Roots come back as written: ~ and {dir} are expanded per process
// when a watcher walks them, which is why a spec is the right thing to show a
// person and the resolved paths are not.
//
// An agent this package was compiled to read (claude, codex, dsh, …) is
// described by a built-in adapter rather than a definition, so SpecFor reports
// false for it; [Supported] is the question that covers every agent.
func SpecFor(tool string) (Spec, bool) { return definedSpec(tool) }

// definedSpec returns an agent's transcript location, whether compiled in
// (the pi family) or loaded at runtime by LoadDefinitions.
func definedSpec(tool string) (Spec, bool) {
	tool = canonicalTool(tool)
	defsMu.RLock()
	defer defsMu.RUnlock()
	s, ok := defs[tool]
	if ok {
		s.Roots = slices.Clone(s.Roots)
		s.Suffixes = slices.Clone(s.Suffixes)
	}
	return s, ok
}

// definitionFile mirrors the agent definitions gauntlet keeps in
// ~/.gauntlet/agents.json. Only the transcript location matters here: the rest
// of that file describes how to launch an agent, which is not this package's
// business.
type definitionFile map[string]*struct {
	Usage *struct {
		Roots      []string `json:"roots"`
		Suffix     string   `json:"suffix,omitempty"`
		Suffixes   []string `json:"suffixes,omitempty"`
		Cumulative bool     `json:"cumulative,omitempty"`
		HeaderCwd  bool     `json:"header_cwd,omitempty"`
	} `json:"usage,omitempty"`
}

// ErrInvalidDefinitions is returned by LoadDefinitions when the file exists
// but has invalid JSON or colliding agent names after normalization. JSON
// errors are wrapped, so errors.As still recovers the parse position.
var ErrInvalidDefinitions = errors.New("malformed agent definitions")

// errCollidingDefinitions marks an agents.json holding two names that NFC
// reduces to one canonical key, an overlap the per-name checks in
// LoadDefinitions cannot see.
var errCollidingDefinitions = errors.New("agent names collide after NFC normalization")

// LoadDefinitions reads agent definitions from a JSON file, teaching this
// package about agents it was not compiled to know, including where they keep
// their transcripts:
//
//	{"myagent": {"usage": {"roots": ["~/.myagent/sessions"]}}}
//
// A missing file is not an error, since most machines have none. A malformed
// or unreadable one is: running with a half-loaded agent set is worse than
// refusing. The error names the file; errors.Is matches ErrInvalidDefinitions
// for invalid JSON or colliding agent names after normalization.
//
// Names are canonicalized before registration, so two spellings that NFC
// reduces to one key (NFD "café" beside precomposed "café") would silently
// overwrite each other in defs. That overlap is refused instead: the file is
// ambiguous about which spec the surviving key should hold, and silently
// keeping whichever entry iterates last makes the loaded agent set depend on
// map order. The registry is left untouched.
func LoadDefinitions(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("agent definitions %s: %w", path, err)
	}
	data = bytes.TrimPrefix(data, utf8BOM)
	var file definitionFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrInvalidDefinitions, path, err)
	}
	if file == nil {
		return fmt.Errorf("%w: %s: expected an object, not null", ErrInvalidDefinitions, path)
	}
	type pendingSpec struct {
		name string
		spec Spec
	}
	var pending []pendingSpec
	seen := make(map[string]string, len(file))
	for name, def := range file {
		if def == nil {
			return fmt.Errorf("%w: %s: agent %q must be an object, not null", ErrInvalidDefinitions, path, name)
		}
		canonical := canonicalTool(name)
		if canonical == "" || def.Usage == nil {
			continue // a launch-only definition says nothing about tokens
		}
		if prev, dup := seen[canonical]; dup {
			return fmt.Errorf("%w: %s: %w: %q and %q both reduce to %q",
				ErrInvalidDefinitions, path, errCollidingDefinitions, prev, name, canonical)
		}
		seen[canonical] = name
		spec := Spec{
			Roots:      slices.Clone(def.Usage.Roots),
			Suffix:     def.Usage.Suffix,
			Suffixes:   slices.Clone(def.Usage.Suffixes),
			Cumulative: def.Usage.Cumulative,
			HeaderCwd:  def.Usage.HeaderCwd,
		}
		if len(specRoots(spec)) == 0 {
			continue
		}
		pending = append(pending, pendingSpec{name: canonical, spec: spec})
	}
	defsMu.Lock()
	defer defsMu.Unlock()
	for _, p := range pending {
		defs[p.name] = p.spec
	}
	return nil
}

// DefinitionsPath is where agent definitions live by default. It follows
// gauntlet's location so one file serves both tools.
func DefinitionsPath() string {
	if h := os.Getenv("GAUNTLET_HOME"); h != "" {
		return filepath.Join(h, "agents.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gauntlet", "agents.json")
}
