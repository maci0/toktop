// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

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
	// roots are directories to scan, given the process's working directory and
	// the instant the walk is running on the watcher's clock. Most agents keep
	// transcripts under $HOME and ignore it; agents that keep them inside the
	// project (clanker) use it. An adapter that caches what it read (kimi
	// lists the store itself) ages that cache on now, so the walk's own
	// timeline decides when it is re-read.
	roots func(dir string, now time.Time) []string
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
	// parseFile reads a transcript that is one JSON document rewritten in
	// place, rather than a log of lines. nil for the line-oriented stores.
	parseFile func(data []byte) (v values, cwd string, ok bool)
	// snapshot is set with parseFile: the file is read from the start whenever
	// it changes, because a rewrite of the same length is not an append.
	snapshot bool
	// sessionCwd reads the working directory from a session header line, for
	// agents whose usage records do not repeat it. nil when every usage line
	// carries its own cwd.
	sessionCwd func(line []byte) (string, bool)
	// sessionCwdFile reads the working directory from a file beside the
	// transcript, for an agent that records it there rather than in the
	// transcript itself (kimi keeps it in the state.json of the session
	// directory its agents/ directory sits under). nil when the transcript or
	// its header names the directory.
	sessionCwdFile func(path string) (string, bool)
	// rootOwns says a transcript whose header never names a working directory
	// still belongs to this watcher. The roots are already one project's
	// directory. A header that does name a directory still decides.
	rootOwns bool
}

// perFileOwner reports whether ownership is decided per transcript file
// rather than per usage line, from the header line or from a file beside it.
func (a adapter) perFileOwner() bool {
	return a.sessionCwd != nil || a.sessionCwdFile != nil
}

var adaptersMu sync.RWMutex

var adapters = map[string]adapter{
	"claude": {
		roots:  func(string, time.Time) []string { return []string{home(".claude", "projects")} },
		suffix: ".jsonl",
		kind:   perMessage,
		parse:  parseClaude,
	},
	// qwen-code keeps per-project chat transcripts with Gemini-style
	// usageMetadata on each assistant message.
	"qwen": {
		roots:  func(string, time.Time) []string { return []string{home(".qwen", "projects")} },
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
	// `dsh web` server is the exception: that process writes every project's
	// sessions, so its watcher reads both stores whole. A dsh run inside one
	// project still reads only that project's directory.
	// Both stores key a project as --<slug>--, where slug is the absolute
	// path with separators turned into '-'. ~/.dsh/sessions is the harness
	// default. ~/.dsh-native/sessions is the same log written without zstd,
	// and those files often have no cwd of their own: the directory is the
	// attribution, and a header that does name one still wins.
	"dsh": {
		roots:      dshRoots,
		suffixes:   []string{dshZstdSuffix, ".jsonl"},
		kind:       perMessage,
		parse:      parseDsh,
		sessionCwd: genericSessionCwd,
		rootOwns:   true,
	},

	// clanker keeps its own token log inside the repository it runs in, one
	// record per request, so the project directory is the attribution.
	"clanker": {
		roots:  func(dir string, _ time.Time) []string { return []string{filepath.Join(dir, "state")} },
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
		roots:      func(string, time.Time) []string { return []string{home(".copilot", "session-state")} },
		suffix:     "events.jsonl",
		kind:       perMessage,
		parse:      parseGeneric,
		sessionCwd: genericSessionCwd,
	},
	"codex": {
		roots:      func(string, time.Time) []string { return []string{home(".codex", "sessions")} },
		suffix:     ".jsonl",
		kind:       cumulative,
		parse:      parseCodex,
		sessionCwd: codexSessionCwd,
	},
	// Kimi Code CLI writes one event log per session and agent under
	// ~/.kimi-code/sessions/<workDirKey>/<session>/agents/<id>/wire.jsonl,
	// with a usage.record event per model call (see kimi.go). Its store holds
	// one directory per project and hundreds of thousands of files, so the
	// roots are the project directories this working directory owns rather
	// than the store: kimiRoots derives them. A subagent's log sits under the
	// same session directory and counts too, its tokens being its own.
	//
	// The derived directory is named for a twelve-hex-digit hash of the
	// working directory, so the session's own recorded cwd confirms the
	// attribution rather than trusting the name: a collision would otherwise
	// bill another project's tokens to this one.
	"kimi": {
		roots:          kimiRoots,
		suffix:         "wire.jsonl",
		kind:           perMessage,
		parse:          parseKimi,
		sessionCwdFile: kimiSessionCwd,
	},
	// Gemini CLI keeps one JSONL chat per session under
	// ~/.gemini/tmp/<project>/chats. A gemini record's tokens are that turn's
	// own counts. The project directory is named by .project_root beside chats/.
	"gemini": {
		roots:          func(string, time.Time) []string { return []string{home(".gemini", "tmp")} },
		suffix:         ".jsonl",
		kind:           perMessage,
		parse:          parseGeminiRecord,
		sessionCwdFile: geminiSessionCwd,
	},
	// agy (Antigravity CLI) logs steps to
	// brain/<id>/.system_generated/logs/transcript.jsonl. A step that carries
	// no usage counts as nothing. The workspace is the one history.jsonl
	// records for that conversation id, or cache/last_conversations.json
	// when the history has no line for it. The step itself does not name it.
	"agy": {
		roots:          func(string, time.Time) []string { return []string{home(".gemini", "antigravity-cli")} },
		suffix:         "transcript.jsonl",
		kind:           perMessage,
		parse:          parseGeminiRecord,
		sessionCwdFile: agySessionCwd,
	},
	// cursor-agent appends one JSONL per chat under
	// ~/.cursor/projects/<slug>/agent-transcripts. The slug is the working
	// directory with separators turned into '-'. A line counts only when it
	// carries token usage; the directory is the attribution, since the
	// transcript itself does not repeat the cwd.
	"cursor-agent": {
		roots:      cursorRoots,
		suffix:     ".jsonl",
		kind:       perMessage,
		parse:      parseGeneric,
		sessionCwd: genericSessionCwd,
		rootOwns:   true,
	},
	// Grok appends updates.jsonl for the life of a session. A turn's counts
	// are the turn_completed record, including how long the turn took.
	// usage.json is the same totals rewritten in place and is not read:
	// counting both would bill the turn twice.
	"grok": {
		roots:          grokRoots,
		suffix:         "updates.jsonl",
		kind:           perMessage,
		parse:          parseGrokUpdate,
		sessionCwdFile: grokSessionCwd,
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

// DefaultSuffix is the transcript extension a [Spec] that names none
// matches, the one an agent writing ordinary JSONL needs. It is exported for
// the reason [DefaultPollInterval] is: a caller naming an extension
// explicitly passes the package's own default rather than a string copied
// out of the documentation, which drifts the moment either side changes.
const DefaultSuffix = ".jsonl"

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
	rootsFor := func(dir string, _ time.Time) []string {
		out := make([]string, 0, len(patterns))
		for _, r := range patterns {
			out = append(out, core.ExpandHome(strings.ReplaceAll(r, "{dir}", dir)))
		}
		return out
	}
	// Blank suffixes are not patterns: one would match every file under the
	// root, and the parser would then be pointed at the agent's config. Suffix
	// gets the same treatment, so a spec written by hand with a padded value
	// falls back to the default rather than searching for files whose names end
	// in spaces.
	suffixes := make([]string, 0, len(spec.Suffixes))
	for _, s := range spec.Suffixes {
		if s = strings.TrimSpace(s); s != "" {
			suffixes = append(suffixes, s)
		}
	}
	suffix := strings.TrimSpace(spec.Suffix)
	if len(suffixes) > 0 {
		suffix = ""
	} else if suffix == "" {
		suffix = DefaultSuffix
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
	spec, defined := SpecFor(tool)
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
	spec, defined := SpecFor(w.tool)
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
