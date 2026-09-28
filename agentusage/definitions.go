// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/text/unicode/norm"

	"github.com/maci0/toktop/internal/core"
)

// knownAgents are the CLIs this package recognizes by name.
//
// Recognition and readability are separate questions: an agent here is one
// whose process can be identified, which is what makes it appear in Discover.
// Whether its tokens can be read is decided by the adapters in registry.go
// and the definitions below.
var knownAgents = []string{
	"agy", "claude", "clanker", "codex", "copilot", "crush", "cursor-agent",
	"dsh", "feynman", "gemini", "grok", "kimi", "omp", "opencode", "pi",
	"prime-agent", "qwen",
}

// Agents lists every agent name this package knows: the recognized CLIs in
// [knownAgents] (most of which keep no transcript worth reading), the ones
// [LoadDefinitions] registered, and the ones [RegisterSpec] added. Sorted and
// deduplicated, so an agent several of those name appears once. Use
// [Supported] to tell which of them can be read.
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
	// Suffix filters transcript files (default [DefaultSuffix]). Surrounding
	// whitespace is trimmed, and a value left blank by that falls back to the
	// default rather than matching nothing.
	Suffix string `json:"suffix,omitempty"`
	// Suffixes matches several extensions, for an agent that writes more than
	// one (compressed by default, plain when compression is off). It replaces
	// Suffix when set, and blank entries among them are ignored.
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
	defs = builtinDefs()
	// defsGen counts changes to defs: a load that registered an entry, or
	// ResetDefinitions. A watcher re-derives its adapter from a spec on every
	// poll; the spec it reads cannot have changed unless this moved, so an
	// unchanged counter turns that rebuild into a load.
	defsGen atomic.Uint64
)

func bumpDefsGen() { defsGen.Add(1) }

// builtinDefs are the definitions compiled into this build, which
// ResetDefinitions restores.
func builtinDefs() map[string]Spec {
	// HeaderCwd: the session file's first record names the working directory,
	// and the usage lines after it do not. Without that, every session under
	// the store would count for every project.
	return map[string]Spec{
		"pi":          {Roots: []string{"~/.pi/agent/sessions"}, HeaderCwd: true},
		"prime-agent": {Roots: []string{"~/.prime/agent/sessions"}, HeaderCwd: true},
		"feynman":     {Roots: []string{"~/.feynman/sessions"}, HeaderCwd: true},
		"omp":         {Roots: []string{"~/.omp/agent/sessions"}, HeaderCwd: true},
	}
}

// ResetDefinitions drops every definition [LoadDefinitions] added, leaving
// the ones compiled into this build. It is the undo that call otherwise has
// none of, and the counterpart to [UnregisterSpec]: like it, the definitions
// registry is process-wide, so a program (or a test in one) that teaches this
// package an agent has to be able to take it back out.
//
// Agents registered with [RegisterSpec] are adapters rather than definitions
// and are left alone; UnregisterSpec removes those.
func ResetDefinitions() {
	defsMu.Lock()
	defer defsMu.Unlock()
	defs = builtinDefs()
	bumpDefsGen()
}

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
// false for it; [Supported] is the question that covers every agent. That holds
// for one a definitions file tried to define as well, since LoadDefinitions
// skips an entry an adapter outranks.
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

// maxDefinitionsBytes bounds the agent definitions file. It is a JSON object
// of transcript locations, so a real one is a few kilobytes; a file past this
// is a mistake or an attack, and either way must not be read into memory to be
// rejected. The read is capped here rather than by a Stat so a file grown
// between the two cannot slip past the bound, and json.Unmarshal below builds
// a second copy of everything it decodes, so an uncapped read costs twice what
// the file is.
const maxDefinitionsBytes = 8 << 20

// ErrInvalidDefinitions is returned by LoadDefinitions when the file exists
// but cannot be used: invalid JSON, colliding agent names after normalization,
// or a file past maxDefinitionsBytes. JSON errors are wrapped, so errors.As
// still recovers the parse position.
var ErrInvalidDefinitions = errors.New("malformed agent definitions")

// ErrCollidingDefinitions marks an agents.json holding two names that NFC
// reduces to one canonical key, an overlap the per-name checks in
// LoadDefinitions cannot see. It is wrapped inside an [ErrInvalidDefinitions]
// error that names the file and both spellings, so a caller can tell a file
// whose JSON is wrong from one whose agent names are, and say which.
var ErrCollidingDefinitions = errors.New("agent names collide after NFC normalization")

// redactedError renders a diagnostic with the home directory folded out while
// keeping the wrapped cause reachable through errors.Is and errors.As.
//
// The fold has to run over the whole rendered message, not just the path this
// function interpolates: an *fs.PathError from the os package carries its own
// copy of the absolute path, so a message that names the file twice would leak
// the account through the second copy. Formatting the message and then
// discarding the chain to fold it would cost the callers their errors.Is
// checks, so the fold is a wrapper rather than a rewrite.
type redactedError struct {
	msg   string
	cause error
}

func (e redactedError) Error() string { return e.msg }
func (e redactedError) Unwrap() error { return e.cause }

// defsErr builds a LoadDefinitions diagnostic with the home directory folded
// out. Every error this function returns goes through it, since every one
// names a file under $HOME. The cause is joined rather than interpolated so
// errors.Is still matches every error named in the message, which is the
// contract LoadDefinitions documents for ErrInvalidDefinitions.
func defsErr(causes []error, format string, args ...any) error {
	return redactedError{
		msg:   core.RedactHome(fmt.Sprintf(format, args...)),
		cause: errors.Join(causes...),
	}
}

// LoadDefinitions reads agent definitions from a JSON file, teaching this
// package about agents it was not compiled to know, including where they keep
// their transcripts:
//
//	{"myagent": {"usage": {"roots": ["~/.myagent/sessions"]}}}
//
// A missing file is not an error, since most machines have none. A malformed,
// oversize or unreadable one is: running with a half-loaded agent set is worse
// than refusing. The error names the file, with $HOME folded to "~" so the
// line can be pasted into issues; errors.Is matches ErrInvalidDefinitions for
// a file that exists but cannot be used.
//
// A definition may replace another definition, including one compiled into
// this build: the pi family is defined rather than adapted, so a file naming
// feynman with different roots redirects it. What it cannot displace is a
// registered adapter, built-in or installed by RegisterSpec, so a usage entry
// for an agent already read that way (claude, codex, dsh, ...) is skipped
// rather than registered, the same as one naming no roots. Registering it
// would leave SpecFor reporting roots no watcher reads. Use [RegisterSpec] to
// read such an agent elsewhere; it displaces the built-in for as long as it
// is held.
//
// Names are canonicalized before registration, so two spellings that NFC
// reduces to one key (NFD "café" beside precomposed "café") would silently
// overwrite each other in defs. That overlap is refused instead: the file is
// ambiguous about which spec the surviving key should hold, and silently
// keeping whichever entry iterates last makes the loaded agent set depend on
// map order. The registry is left untouched. That one cause is also
// [ErrCollidingDefinitions], so a caller can name it without reading the
// message.
//
// Loading is additive per name, so a second load of the same agent replaces
// that agent rather than adding a second copy. [ResetDefinitions] drops
// everything this call added.
func LoadDefinitions(path string) error {
	// Every diagnostic below names the file, and the file is an absolute path
	// under $HOME ($GAUNTLET_HOME/agents.json, or ~/.gauntlet/agents.json): it
	// names the account, and these are the lines pasted into issues. defsErr
	// folds the home out of all of them, the way warnIgnoredGauntletHome and
	// the ssh store's reader both spell the same path, so the reason the file
	// could not be used survives while the account name does not.
	data, err := readCapped(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if errors.Is(err, errDefinitionsTooLarge) {
			return defsErr([]error{ErrInvalidDefinitions, err}, "%s: %s: %v", ErrInvalidDefinitions, path, err)
		}
		return defsErr([]error{err}, "agent definitions %s: %v", path, err)
	}
	data = bytes.TrimPrefix(data, utf8BOM)
	var file definitionFile
	if err := json.Unmarshal(data, &file); err != nil {
		return defsErr([]error{ErrInvalidDefinitions, err}, "%s: %s: %v", ErrInvalidDefinitions, path, err)
	}
	if file == nil {
		return defsErr([]error{ErrInvalidDefinitions}, "%s: %s: expected an object, not null", ErrInvalidDefinitions, path)
	}
	type pendingSpec struct {
		name string
		spec Spec
	}
	var pending []pendingSpec
	seen := make(map[string]string, len(file))
	for name, def := range file {
		if def == nil {
			return defsErr([]error{ErrInvalidDefinitions}, "%s: %s: agent %q must be an object, not null", ErrInvalidDefinitions, path, name)
		}
		canonical := canonicalTool(name)
		if canonical == "" || def.Usage == nil {
			continue // a launch-only definition says nothing about tokens
		}
		if _, registered := registeredAdapter(canonical); registered {
			// A registered adapter, built-in or installed by RegisterSpec,
			// outranks every definition, so registering this entry would leave
			// SpecFor reporting transcript roots that Watch never reads.
			// Skipping keeps the registry the answer to "what did this file
			// register", which is how a program finds the entries a file
			// failed to apply.
			continue
		}
		if prev, dup := seen[canonical]; dup {
			return defsErr([]error{ErrInvalidDefinitions, ErrCollidingDefinitions},
				"%s: %s: %s: %q and %q both reduce to %q",
				ErrInvalidDefinitions, path, ErrCollidingDefinitions, prev, name, canonical)
		}
		spec := Spec{
			Roots:      slices.Clone(def.Usage.Roots),
			Suffix:     def.Usage.Suffix,
			Suffixes:   slices.Clone(def.Usage.Suffixes),
			Cumulative: def.Usage.Cumulative,
			HeaderCwd:  def.Usage.HeaderCwd,
		}
		if len(specRoots(spec)) == 0 {
			// A spec with no root registers nothing, so it cannot hold a
			// canonical name: recording it in seen would let a definition the
			// registry never sees veto the one that does.
			continue
		}
		seen[canonical] = name
		pending = append(pending, pendingSpec{name: canonical, spec: spec})
	}
	defsMu.Lock()
	defer defsMu.Unlock()
	for _, p := range pending {
		defs[p.name] = p.spec
	}
	if len(pending) > 0 {
		bumpDefsGen()
	}
	return nil
}

// errDefinitionsTooLarge marks a definitions file past the cap. It is folded
// into ErrInvalidDefinitions at the call site, because a file too large to
// hold definitions is one LoadDefinitions already refuses, and the caller
// contract is that every refusal of a file that exists is that error: a
// second kind would split it in two for no reason a caller could act on.
var errDefinitionsTooLarge = errors.New("agent definitions file is too large")

// readCapped reads the definitions file whole, refusing one past
// maxDefinitionsBytes. One byte past the cap is read so a truncated file
// cannot decode as a complete document, the same shape snapshotValue applies
// to a transcript snapshot. A missing file is returned as the os error it is,
// so the caller keeps telling "no definitions here" apart from "definitions
// unreadable": the first is the ordinary case, the second is a fault.
func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxDefinitionsBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDefinitionsBytes {
		return nil, fmt.Errorf("%w: over %d bytes", errDefinitionsTooLarge, maxDefinitionsBytes)
	}
	return data, nil
}

// DefinitionsPath is where agent definitions live by default. It follows
// gauntlet's location so one file serves both tools.
//
// GAUNTLET_HOME is honored only when absolute, the same rule the XDG base
// directories get in openCodeDBPath and defaultKnownHostsPath. A relative
// value would place agents.json under whatever directory the run started in,
// where a missing file is not an error: the defined agents would simply never
// appear, looking like agents producing no tokens. Falling back to the
// documented default keeps the run reading the file it always read; the
// startup warning (warnIgnoredGauntletHome) names the ignored value.
func DefinitionsPath() string {
	if h := os.Getenv("GAUNTLET_HOME"); filepath.IsAbs(h) {
		return filepath.Join(h, "agents.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gauntlet", "agents.json")
}
