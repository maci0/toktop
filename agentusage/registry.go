// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

// Package agentusage reports the token usage AI coding agents record on disk.
//
// Typical use: LoadDefinitions, Discover running agents, Watch each process
// (or Process.Watch), then Poll or Run for Sample values. A Sample is the
// total since the watcher attached, so a caller reporting events takes the
// growth from Sample.Delta. EnableOpenCodeDB opts into opencode's
// machine-wide SQLite store; crush is read whenever the sqlite build tag is
// on.
//
// An agent is read one of two ways. Transcript agents (claude, qwen, dsh,
// clanker, copilot, codex) appear in the adapters table, each naming where
// its logs live under a working directory and how one line becomes a Sample.
// RegisterSpec adds one this package does not ship with, and LoadDefinitions
// reads the same declaration from a JSON file, DefinitionsPath being the
// default location.
//
// Database agents are registered as sources instead: crush is built in,
// opencode is added by EnableOpenCodeDB because its store is machine-wide
// and the operator opts into it.
//
// Discover finds the agent processes running now, and Watch reads the
// transcripts of the one working in a directory, so a caller can take a
// Sample on an interval without knowing which agent is underneath.
//
// The agent registry is process-wide, since one process reports one set of
// agents. RegisterSpec and UnregisterSpec add and remove an adapter, and
// LoadDefinitions and ResetDefinitions do the same for a definitions file, so
// a program that teaches this package an agent can take it back out.
//
// The registry calls and Watch are safe to call from several goroutines at
// once, and a Watcher is safe to use while its Run is going: Poll takes the
// read lock a running Run also takes, so a final read after the agent exits
// is a call like any other rather than a race. A definitions file reloaded
// after a watcher started reaches it on the next poll, and a source
// withdrawn by EnableOpenCodeDB(false) stops being read the same way; an
// adapter installed by RegisterSpec is fixed for the life of the process, and
// a watcher keeps the one it attached with.
//
// Every method on a nil *Watcher is safe to call on the result of Watch:
// Tool and Dir report the empty string, Err matches ErrUnsupportedTool, Poll
// and Sample report the zero Sample, Run returns at once, and SetNow does
// nothing. A caller skipping the agents it cannot read therefore asks Err,
// which is what names that case, rather than testing the pointer:
//
//	if w := agentusage.Watch(tool, dir, time.Now()); w.Err() != nil {
//		// no readable usage for this agent
//	}
//
// The crush and opencode sources need a SQLite driver, so they exist only
// under the sqlite build tag. Without it the package still compiles, and
// Supported reports those agents unreadable.
//
// Agents differ in what they print to stdout: some report token usage as they
// stream, some only at exit, some never. They agree on something else, though,
// which is that they keep a structured session transcript, and that transcript
// carries per-message usage with timestamps. Tailing it gives a live rate
// without root, without intercepting anyone's network traffic, and without
// asking the agent to behave differently.
//
// The design constraints that shape everything here:
//
//   - Only count usage after the watcher attached. Session transcripts persist
//     across runs, so the watcher records where each file ended when it
//     attached and reads only what is appended after that. Database-backed
//     agents (opencode, crush) use the since argument the same way; file
//     transcripts are always tailed from their attach-time end.
//   - Attribute the transcript to the right process. Each adapter ties its
//     files to a working directory: recorded per record, read from the
//     session header, or implicit because the log lives inside the project
//     directory itself (clanker), so the cwd is the key.
//   - Never invent a number. An agent whose transcript cannot be found, parsed,
//     or attributed simply reports nothing, and the dashboard shows no rate.
package agentusage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/core"
)

type adapter struct {
	// roots are directories to scan, given the process's working directory.
	// Most agents keep transcripts under $HOME and ignore it; agents that keep
	// them inside the project (clanker) use it.
	roots func(dir string) []string
	// suffix filters transcript files under a root by literal suffix match.
	suffix string
	// suffixes, when set, replaces suffix: dsh writes `.jsonl.zstd` by
	// default and `.jsonl` when compression is off, so both must match.
	suffixes []string
	// kind says how to combine the parsed values.
	kind valueKind
	// parse extracts usage and the recorded working directory from one line.
	// ok is false for lines that carry neither.
	parse func(line []byte) (v values, cwd string, ok bool)
	// sessionCwd reads the working directory from a session header line, for
	// agents whose usage records do not repeat it. nil when every usage line
	// carries its own cwd.
	sessionCwd func(line []byte) (string, bool)
}

var adaptersMu sync.RWMutex

var adapters = map[string]adapter{
	"claude": {
		roots:  func(string) []string { return []string{home(".claude", "projects")} },
		suffix: ".jsonl",
		kind:   perMessage,
		parse:  parseClaude,
	},
	// qwen-code keeps per-project chat transcripts with Gemini-style
	// usageMetadata on each assistant message.
	"qwen": {
		roots:  func(string) []string { return []string{home(".qwen", "projects")} },
		suffix: ".jsonl",
		kind:   perMessage,
		parse:  parseQwen,
	},
	// dsh (DeepSeek Harness) writes one session log per run under
	// ~/.dsh/sessions/--<normalized-cwd>--/<id>/, named session.v<N>.jsonl.zstd
	// (session.jsonl.zstd for generation zero, and .jsonl when compression is
	// off), with the cwd in an opening header record. Default encoding is
	// concatenated zstd frames; the counts are the provider's, nested under
	// data.usage on the completed assistant/message record.
	//
	// The store is machine-wide, so ownership follows the session's recorded
	// cwd, like opencode's directory column and every other adapter here. A
	// `dsh web` server launched in one directory therefore reads the sessions
	// of that directory, not the ones it hosts for other projects.
	"dsh": {
		roots:      func(string) []string { return []string{home(".dsh", "sessions")} },
		suffixes:   []string{dshZstdSuffix, ".jsonl"},
		kind:       perMessage,
		parse:      parseDsh,
		sessionCwd: genericSessionCwd,
	},

	// clanker keeps its own token log inside the repository it runs in, one
	// record per request, so the project directory is the attribution.
	"clanker": {
		roots:  func(dir string) []string { return []string{filepath.Join(dir, "state")} },
		suffix: "token_stats.jsonl",
		kind:   perMessage,
		parse:  parseGeneric,
	},
	// GitHub Copilot CLI appends one event per line to
	// ~/.copilot/session-state/<session>/events.jsonl. Only the opening
	// session.start record carries the working directory (under
	// data.context.cwd); the assistant.message records carry OpenAI-shaped
	// usage, which the generic parser already reads.
	"copilot": {
		roots:      func(string) []string { return []string{home(".copilot", "session-state")} },
		suffix:     "events.jsonl",
		kind:       perMessage,
		parse:      parseGeneric,
		sessionCwd: genericSessionCwd,
	},
	"codex": {
		roots:      func(string) []string { return []string{home(".codex", "sessions")} },
		suffix:     ".jsonl",
		kind:       cumulative,
		parse:      parseCodex,
		sessionCwd: codexSessionCwd,
	},
}

// overrides records what each RegisterSpec displaced, keyed like adapters and
// guarded by adaptersMu. It is not an adapter source: adapterFor still reads
// adapters, and an entry here only says what UnregisterSpec puts back.
var overrides = map[string]displaced{}

var (
	// ErrEmptyTool is returned by RegisterSpec when the agent name is blank.
	ErrEmptyTool = errors.New("usage spec needs an agent name")
	// ErrNoRoots is returned by RegisterSpec when the spec names no transcript
	// directories. errors.Is matches it through the formatted error that
	// includes the agent name.
	ErrNoRoots = errors.New("usage spec has no roots")
	// ErrUnsupportedTool is what Watcher.Err reports for the nil watcher Watch
	// returns: the agent keeps nothing this package can read, because no source
	// is registered for it, no definition names its transcripts, or its
	// definition names none. It is a fact about the agent, not a failure, so a
	// caller that has nothing to display treats it like any other empty
	// reading.
	ErrUnsupportedTool = errors.New("agent has no readable usage source")
)

// specAdapter builds the file adapter a definition's spec describes. Pure: it
// writes nothing, so both RegisterSpec and adapterFor can use it.
func specAdapter(spec Spec) (adapter, bool) {
	// {dir} lets a definition point at a store inside the agent's working
	// directory, the way clanker keeps its own. Blank roots are dropped
	// rather than becoming empty WalkDir targets.
	patterns := specRoots(spec)
	if len(patterns) == 0 {
		return adapter{}, false
	}
	rootsFor := func(dir string) []string {
		out := make([]string, 0, len(patterns))
		for _, r := range patterns {
			out = append(out, core.ExpandHome(strings.ReplaceAll(r, "{dir}", dir)))
		}
		return out
	}
	// Blank suffixes are not patterns: one would match every file under the
	// root, and the parser would then be pointed at the agent's config.
	suffixes := make([]string, 0, len(spec.Suffixes))
	for _, s := range spec.Suffixes {
		if strings.TrimSpace(s) != "" {
			suffixes = append(suffixes, s)
		}
	}
	suffix := spec.Suffix
	if len(suffixes) > 0 {
		suffix = ""
	} else if suffix == "" {
		suffix = ".jsonl"
	}
	kind := perMessage
	if spec.Cumulative {
		kind = cumulative
	}
	ad := adapter{
		roots:    rootsFor,
		suffix:   suffix,
		suffixes: suffixes,
		kind:     kind,
		parse:    parseGeneric,
	}
	if spec.HeaderCwd {
		ad.sessionCwd = genericSessionCwd
	}
	return ad, true
}

// RegisterSpec adds a transcript adapter for a defined agent. It returns
// ErrEmptyTool or an error wrapping ErrNoRoots when the spec cannot be used.
func RegisterSpec(tool string, spec Spec) error {
	tool = canonicalTool(tool)
	if tool == "" {
		return ErrEmptyTool
	}
	ad, ok := specAdapter(spec)
	if !ok {
		return fmt.Errorf("usage spec for %q has no roots: %w", tool, ErrNoRoots)
	}
	adaptersMu.Lock()
	defer adaptersMu.Unlock()
	prev, had := adapters[tool]
	// Only the first registration remembers what it displaced, so repeated
	// RegisterSpec calls followed by one UnregisterSpec land back where the
	// process started rather than on the second spec.
	if _, overridden := overrides[tool]; !overridden {
		overrides[tool] = displaced{prev: prev, had: had}
	}
	adapters[tool] = ad
	return nil
}

// displaced is the adapter a RegisterSpec replaced, so UnregisterSpec can put
// it back. had is false when the agent had no registered adapter to displace,
// which is the definition-derived case: dropping the override is what lets the
// definition apply again.
type displaced struct {
	prev adapter
	had  bool
}

// UnregisterSpec removes the adapter [RegisterSpec] installed for an agent and
// restores the one it replaced, reporting whether a registration was there to
// remove. It is how a program that teaches this package an agent, or fakes one
// in its own tests, takes that back: the registry is process-wide, so without
// it every later test in the same binary inherits the spec.
//
// The agent name is canonicalized like everywhere else, so " pi " and "pi"
// unregister the same agent. A name that was never registered removes nothing
// and reports false.
//
// The restored adapter is a built-in one if a built-in was displaced, and the
// agent's loaded definition if there was no registered adapter to displace.
// A non-file source (opencode) is registered by EnableOpenCodeDB, not here,
// and is left alone. Watchers already running keep the adapter they attached
// with.
func UnregisterSpec(tool string) bool {
	tool = canonicalTool(tool)
	if tool == "" {
		return false
	}
	adaptersMu.Lock()
	defer adaptersMu.Unlock()
	prev, ok := overrides[tool]
	if !ok {
		return false
	}
	if prev.had {
		adapters[tool] = prev.prev
	} else {
		delete(adapters, tool)
	}
	delete(overrides, tool)
	return true
}

// registeredAdapter returns the adapter explicitly registered for an agent.
func registeredAdapter(tool string) (adapter, bool) {
	adaptersMu.RLock()
	defer adaptersMu.RUnlock()
	ad, ok := adapters[tool]
	return ad, ok
}

// adapterFor returns the file adapter for an agent: the one registered for it,
// or the one its loaded definition describes. Deriving rather than registering
// keeps a reader from writing the registry, and lets a definition reloaded at
// runtime reach watchers that are already running.
func adapterFor(tool string) (adapter, bool) {
	if ad, ok := registeredAdapter(tool); ok {
		return ad, true
	}
	spec, defined := definedSpec(tool)
	if !defined {
		return adapter{}, false
	}
	return specAdapter(spec)
}

// refreshAdapter re-derives a definition-backed watcher's adapter, so a
// reloaded definition reaches a watcher that is already running. A registered
// adapter (a built-in, or an explicit RegisterSpec) is fixed for the process
// and is left alone, which also keeps a test's patched adapter in place.
//
// A watcher re-derives from definitions on every poll, and the derivation
// clones the spec's roots, expands ~ and substitutes {dir} in each: all of it
// discarded work unless a definitions file was reloaded in between. The
// generation counter says whether that happened, and the steady state is one
// atomic load.
func (w *Watcher) refreshAdapter() {
	if _, registered := registeredAdapter(w.tool); registered {
		return
	}
	gen := defsGen.Load()
	if w.fromDefs && w.defsGen == gen {
		return
	}
	spec, defined := definedSpec(w.tool)
	if !defined {
		// The reload stopped naming this agent, so the store the adapter
		// walks is one no spec claims any more. Returning with the adapter in
		// place is what pinned it: a watcher that derived from a definition
		// would keep walking and billing that store for the life of the
		// process, long after ResetDefinitions or a file that no longer lists
		// the agent took the definition away. A redirect is handled by the
		// derive below, so only the withdrawal needs a branch here.
		//
		// The counts already published stay, exactly as a withdrawn source
		// leaves them: the watcher simply has nothing to read, and the sample
		// holds its last value rather than dropping to zero and reading as an
		// agent whose sessions were deleted.
		w.forgetSpec()
		return
	}
	ad, ok := specAdapter(spec)
	if !ok {
		// A spec with no usable root is as unreadable as no spec: it names
		// nothing to walk, so it is withdrawn the same way rather than
		// leaving the previous adapter walking a tree this one disowns.
		w.forgetSpec()
		return
	}
	// A load that landed while this derived leaves the adapter and the
	// counter disagreeing; recording the newer one would pin a stale adapter
	// for the watcher's life, so leave both unset and derive again next poll.
	if defsGen.Load() != gen {
		return
	}
	w.ad = ad
	w.roots = nil // the old adapter's expanded roots belong to the old one
	// The listing belongs to the old adapter too: it names the roots and
	// suffixes that adapter walked, so reusing it for a rescan window after
	// a reload that redirected either keeps this poll reading the tree the
	// spec just disowned. Re-listing is what the generation change already
	// costs, and one walk is the price.
	//
	// The per-file bookkeeping stays. Offsets and counts are read positions
	// in files, not answers about the agent: dropping them would re-read
	// records already counted (billing a transcript twice) and reset totals
	// for a watcher whose definition was reloaded for an unrelated reason.
	w.cached, w.scanned = nil, time.Time{}
	w.defsGen = gen
	w.fromDefs = true
	w.adGone = false
}

// forgetSpec stops a watcher whose definition was withdrawn from walking the
// store that spec named. The adapter itself is kept rather than zeroed, because
// a watcher built on one has no usable nil adapter (roots and parse are called
// unconditionally), and a load that names the agent again re-derives over it
// on the next poll. The generation is recorded so the withdrawal is settled in
// one pass instead of being re-decided four times a second, and the listing
// and the expanded roots go with it, since both name the disowned tree.
func (w *Watcher) forgetSpec() {
	if !w.fromDefs {
		return // never derived from a definition, so nothing to withdraw
	}
	w.adGone = true
	w.cached, w.scanned = nil, time.Time{}
	w.roots = nil
	w.defsGen = defsGen.Load()
}

// Supported reports whether live usage can be read for an agent.
func Supported(tool string) bool {
	tool = canonicalTool(tool)
	if _, ok := sourceFor(tool); ok {
		return true
	}
	// A definition that names a transcript root is readable even before a
	// watcher has been built for it, and callers ask this to decide whether a
	// rate is possible at all. adapterFor rejects the blank-root spec
	// RegisterSpec rejects, so this cannot promise a rate Watch would refuse.
	_, ok := adapterFor(tool)
	return ok
}

func home(parts ...string) string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(append([]string{dir}, parts...)...)
}
