// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

// Package agentusage reads live token counts out of the transcripts agent CLIs
// already write to disk.
//
// Typical use: LoadDefinitions, Discover running agents, Watch each process
// (or Process.Watch), then Poll or Run for Sample values. EnableOpenCodeDB
// opts into opencode's machine-wide SQLite store; crush is read whenever the
// sqlite build tag is on.
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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Sample is cumulative usage observed since the watcher attached.
type Sample struct {
	// Output is generated tokens observed since attach.
	Output int
	// Thinking is the reasoning share of Output, when the agent reports it
	// separately: what the model spent before it wrote anything the user sees.
	Thinking int
	// Total is the largest per-request context size seen, not a sum: summing
	// those would count the same conversation once per turn.
	Total int
	// Input is billed prompt tokens, accrued per request the same way Output is.
	Input int
	// At is when the reading was taken, so successive samples make a rate.
	At time.Time
}

// Empty reports whether nothing has been observed yet. Thinking-only samples
// count: an agent that reports reasoning without a billed output is still a
// reading, and callers that skip Empty samples must not drop it.
func (s Sample) Empty() bool {
	return !values{output: s.Output, thinking: s.Thinking, total: s.Total, input: s.Input}.present()
}

// Rate returns output tokens per second between two samples, and whether it
// could be computed at all. Both samples need a timestamp: a missing one is
// not a reading, and treating it as the zero instant would invent a rate off
// a first sample whose counter has already grown. It never extrapolates:
// without two readings and a positive span there is no rate to report.
// Prompt growth is InputRate.
func Rate(prev, cur Sample) (float64, bool) {
	return deltaRate(prev.At, cur.At, prev.Output, cur.Output)
}

// InputRate returns billed prompt tokens per second between two samples, and
// whether it could be computed. Same rules as Rate: both samples need a
// timestamp, and no positive span or no growth means no rate, not a zero.
func InputRate(prev, cur Sample) (float64, bool) {
	return deltaRate(prev.At, cur.At, prev.Input, cur.Input)
}

func deltaRate(prevAt, curAt time.Time, prevN, curN int) (float64, bool) {
	if prevAt.IsZero() || curAt.IsZero() {
		return 0, false
	}
	span := curAt.Sub(prevAt).Seconds()
	if span <= 0 || curN <= prevN {
		return 0, false
	}
	return float64(curN-prevN) / span, true
}

// valueKind says how an adapter's numbers accumulate.
type valueKind uint8

const (
	// perMessage values are added up: each line carries one message's usage.
	perMessage valueKind = iota
	// cumulative values already include everything before them, so the
	// watcher subtracts the first value it sees.
	cumulative
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

// fileStamp is what a transcript looked like when it was last read.
type fileStamp struct {
	mtimeNanos int64
	size       int64
}

type values struct {
	output   int
	thinking int
	total    int
	input    int
}

// present is the same rule as Sample.Empty inverted: any counter is a
// reading. Sources and parsers that skip "empty" records must use this, or
// a thinking-only line is dropped before it can become a Sample.
func (v values) present() bool {
	return v.output > 0 || v.thinking > 0 || v.total > 0 || v.input > 0
}

// maxSaneTokens bounds one counter a transcript line may contribute. Real
// usage never approaches it; anything larger is corruption or hostility, and
// reporting nothing beats displaying a lie (or overflowing the totals).
const maxSaneTokens = 1 << 40

// counter coerces a decoded transcript counter to its contribution: negative
// or absurd magnitudes read as absent, the same judgment asInt makes for the
// generic walker.
func counter(n int) int {
	if n < 0 || n > maxSaneTokens {
		return 0
	}
	return n
}

// counter64 is counter for values that arrive as int64 from a database
// column, so a magnitude that does not fit in int is rejected before the
// conversion rather than wrapping.
func counter64(n int64) int {
	if n < 0 || n > maxSaneTokens {
		return 0
	}
	return int(n)
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
		suffix:     dshZstdSuffix,
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

var (
	// ErrEmptyTool is returned by RegisterSpec when the agent name is blank.
	ErrEmptyTool = errors.New("usage spec needs an agent name")
	// ErrNoRoots is returned by RegisterSpec when the spec names no transcript
	// directories. errors.Is matches it through the formatted error that
	// includes the agent name.
	ErrNoRoots = errors.New("usage spec has no roots")
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
			out = append(out, expandHome(strings.ReplaceAll(r, "{dir}", dir)))
		}
		return out
	}
	suffix := spec.Suffix
	if suffix == "" {
		suffix = ".jsonl"
	}
	kind := perMessage
	if spec.Cumulative {
		kind = cumulative
	}
	ad := adapter{
		roots:  rootsFor,
		suffix: suffix,
		kind:   kind,
		parse:  parseGeneric,
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
	adapters[tool] = ad
	adaptersMu.Unlock()
	return nil
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
func (w *Watcher) refreshAdapter() {
	if _, registered := registeredAdapter(w.tool); registered {
		return
	}
	spec, defined := definedSpec(w.tool)
	if !defined {
		return
	}
	if ad, ok := specAdapter(spec); ok {
		w.ad = ad
	}
}

func expandHome(p string) string {
	if p == "~" {
		if dir, err := os.UserHomeDir(); err == nil {
			return dir
		}
		return p
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if dir, err := os.UserHomeDir(); err == nil {
			return filepath.Join(dir, p[2:])
		}
	}
	return p
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

// Watcher tails one agent's transcripts from the moment it attached.
type Watcher struct {
	// source is set for agents whose usage is not in files (opencode, crush).
	// When it is present, every field below that describes file state is unused.
	source tokenSource
	// dirs are the spellings a source matches against, for agents that record
	// the directory they were started in rather than its resolved form.
	dirs    []string
	ad      adapter
	tool    string
	dir     string
	since   time.Time
	offsets map[string]int64 // file -> bytes already accounted for
	// preexisting marks transcripts that were already on disk when the watcher
	// attached. Their earlier content belongs to a previous run; a file that
	// appears afterwards belongs entirely to this attach.
	preexisting map[string]bool
	// stamps records what a transcript looked like when it was last read, so
	// idle files are skipped without opening them. Size rides along with the
	// mtime because a coarse clock (NTFS, and Windows' lazy last-write update
	// for an open handle) can leave two writes sharing one stamp: a file that
	// only grew would then look untouched and its records would be lost.
	stamps  map[string]fileStamp
	owner   map[string]bool // file -> belongs to this working directory (cached)
	cached  []string        // candidate files, refreshed on an interval
	scanned time.Time
	// Cumulative adapters need a baseline per file: usage recorded before the
	// watcher attached belongs to a previous run.
	base      map[string]int
	baseThink map[string]int
	baseInput map[string]int
	seen      map[string]values // file -> this attach's contribution
	total     map[string]int
	// sourceBase is per-session counters at attach for a sessionSource
	// (crush). completion_tokens and prompt_tokens are cumulative for the
	// session's life, so without this a continued session would dump its
	// history into this attach the first time it was updated. hasSessionBase
	// is the latch: an empty map is a valid snapshot (no sessions yet), and
	// a failed attach read must not look like that or a later successful
	// poll would count the whole store as growth.
	sourceBase     map[string]map[string]sessionCounts
	hasSessionBase bool

	// pollMu serializes reads: the ticker goroutine and a caller's final
	// synchronous Poll both walk the same offsets and counters.
	pollMu sync.Mutex

	mu     sync.Mutex
	sample Sample
}

// Tool is the agent this watcher follows.
func (w *Watcher) Tool() string {
	if w == nil {
		return ""
	}
	return w.tool
}

// Dir is the working directory this watcher attributes usage to.
func (w *Watcher) Dir() string {
	if w == nil {
		return ""
	}
	return w.dir
}

// Watch starts reading usage for one agent working in one directory.
//
// tool is the agent name (claude, codex, crush, …). dir is the working
// directory that attributes transcripts to this process. since bounds
// database-backed agents (opencode, crush): only usage recorded after that
// instant is counted. File transcripts are always tailed from their
// attach-time end, so since does not rewind them; pass time.Now() at attach.
//
// It returns nil when that agent keeps no readable transcript, which callers
// should treat as "no rate available" rather than an error.
func Watch(tool, dir string, since time.Time) *Watcher {
	tool = canonicalTool(tool)
	if source, ok := sourceFor(tool); ok {
		w := &Watcher{source: source, tool: tool, dir: resolveDir(dir), dirs: dirSpellings(dir), since: since}
		if source.session != nil {
			// A failed snapshot must not become an empty baseline: that
			// would credit every pre-attach token the first time the store
			// becomes readable. Leave sourceBase unset and retry on poll.
			if base, ok := source.session.sessions(w.dirs, time.Time{}); ok {
				w.sourceBase = base
				w.hasSessionBase = true
			}
		}
		return w
	}
	ad, ok := adapterFor(tool)
	if !ok {
		return nil
	}
	w := &Watcher{
		ad: ad, tool: tool, dir: resolveDir(dir), since: since,
		offsets: map[string]int64{}, preexisting: map[string]bool{}, stamps: map[string]fileStamp{},
		owner: map[string]bool{}, base: map[string]int{}, baseThink: map[string]int{},
		baseInput: map[string]int{},
		seen:      map[string]values{}, total: map[string]int{},
	}
	// Record where existing files end before anything is counted.
	for _, path := range w.candidates() {
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		w.offsets[path] = fi.Size()
		w.preexisting[path] = true
		if ad.kind == cumulative {
			// Cumulative counters only make sense against what the session had
			// already spent. That value is in the bytes being skipped, so it is
			// read once here; without it the first record after attaching would
			// become the baseline and this attach would measure zero forever.
			w.seedBaseline(path)
		}
	}
	// The seeding walk serves attach bookkeeping, not reads: leave the listing
	// unstamped so the first poll re-walks and sees sessions created between
	// attach and then. From that poll on, empty and non-empty listings share
	// the same rescanEvery freshness window.
	w.scanned = time.Time{}
	return w
}

// resolveDir is the form a working directory is compared in: absolute, with
// symlinks resolved, since that is what agents record.
func resolveDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs
}

// dirSpellings lists the paths a process's working directory can be recorded under:
// the resolved one, and the one the caller passed if it differs. On macOS
// every temporary directory is reached through a symlink, and an agent records
// whichever spelling it was started with; there the list also carries the
// other Unicode normalization forms of each spelling, since the file system
// treats them as one directory while agents record either.
func dirSpellings(dir string) []string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	spellings := []string{abs}
	if resolved := resolveDir(dir); resolved != abs {
		spellings = append(spellings, resolved)
	}
	var out []string
	for _, s := range spellings {
		for _, v := range dirVariants(s) {
			if !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	return out
}

// openTranscript opens path only if it still lives under one of this
// watcher's roots. A symlink swapped to point outside is refused, so a
// writable store cannot pull in a file from elsewhere.
func (w *Watcher) openTranscript(path string) (*os.File, error) {
	for _, root := range w.ad.roots(w.dir) {
		if root == "" {
			continue
		}
		f, err := openUnder(root, path)
		if err == nil {
			return f, nil
		}
	}
	return nil, os.ErrNotExist
}

func openUnder(root, path string) (*os.File, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, err
	}
	if !filepath.IsLocal(rel) {
		return nil, errOutsideRoot
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.Open(rel)
}

var errOutsideRoot = errors.New("path is outside the transcript root")

// baselineTailBytes bounds the seed read. Cumulative values only grow, so the
// last one in the file is the baseline, and the tail always holds it.
const baselineTailBytes = 256 << 10

// seedBaseline records what a pre-existing session had already spent.
// consumeAppend is used rather than a Scanner: a line past maxLineBytes
// aborts bufio.Scanner and would leave a stale (too-low) baseline from
// earlier records, so later attach-time totals look like this attach's
// growth. consumeAppend skips the oversized line and keeps reading, and
// a real I/O error leaves the baseline unset instead of committing a
// partial one.
func (w *Watcher) seedBaseline(path string) {
	f, err := w.openTranscript(path)
	if err != nil {
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return
	}
	off := int64(0)
	if fi.Size() > baselineTailBytes {
		off = fi.Size() - baselineTailBytes
		if _, err := f.Seek(off, 0); err != nil {
			return
		}
	}
	recs, _, ok := w.consumeAppend(f, off)
	if !ok {
		return
	}
	for _, v := range recs {
		w.base[path] = max(w.base[path], v.output)
		w.baseThink[path] = max(w.baseThink[path], v.thinking)
		w.baseInput[path] = max(w.baseInput[path], v.input)
	}
}

// Run polls until the context is canceled, calling onChange whenever the
// observed usage grows. It is meant to run in its own goroutine.
func (w *Watcher) Run(ctx context.Context, every time.Duration, onChange func(Sample)) {
	if w == nil {
		return
	}
	if every <= 0 {
		every = pollEvery
	}
	// A first read straight away: an agent that reports early should show a
	// rate early, rather than waiting out a tick.
	w.poll(onChange)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			w.poll(onChange) // one last read, so the tail of a run is not lost
			return
		case <-t.C:
			w.poll(onChange)
		}
	}
}

// readSource is one reading from a non-file source. A sessionSource is
// snapshotted at attach (or on the first successful poll if that read
// failed), so only growth since then is counted. A usageSource (opencode)
// reports this attach's usage in full each time via a timestamp filter.
//
// src is the source registered for this agent right now, which poll resolves
// each time so a withdrawn provider stops being read.
func (w *Watcher) readSource(src tokenSource) (values, bool) {
	if src.session != nil {
		return w.readSessionSource(src.session)
	}
	if src.usage != nil {
		return src.usage.read(w.dirs, w.since)
	}
	return values{}, false
}

// readSessionSource counts growth against the attach snapshot. If that
// snapshot has not landed yet, this call is the retry: a success becomes
// the baseline and reports nothing, so pre-attach tokens are never the
// first "growth".
func (w *Watcher) readSessionSource(ss sessionSource) (values, bool) {
	if !w.hasSessionBase {
		base, ok := ss.sessions(w.dirs, time.Time{})
		if !ok {
			return values{}, false
		}
		w.sourceBase = base
		w.hasSessionBase = true
		return values{}, false // attach baseline: nothing yet is this attach's
	}
	cur, ok := ss.sessions(w.dirs, w.since)
	if !ok {
		return values{}, false
	}
	var outN, inN int64
	for path, sess := range cur {
		base := w.sourceBase[path]
		for id, tokens := range sess {
			b := base[id]
			if d := tokens.output - b.output; d > 0 {
				if outN > int64(maxSaneTokens)-d {
					return values{}, false
				}
				outN += d
			}
			if d := tokens.input - b.input; d > 0 {
				if inN > int64(maxSaneTokens)-d {
					return values{}, false
				}
				inN += d
			}
		}
	}
	v := values{output: counter64(outN), input: counter64(inN)}
	return v, v.present()
}

// Poll reads whatever the transcripts have gained since the last read and
// returns the total. Callers use it for a final synchronous read once the
// agent has exited, since the last records land after the process is gone.
func (w *Watcher) Poll() Sample {
	if w == nil {
		return Sample{}
	}
	// Force a fresh walk: this is the caller's last chance to see a session
	// file created seconds ago, and a run short enough to finish inside
	// rescanEvery would otherwise report nothing at all. Clearing the stamp is
	// what forces it; the listing itself is what candidates rewrites. The cost
	// is one walk per final read, not one per periodic poll: Run's ticker goes
	// through poll, which still reuses the cached listing.
	w.pollMu.Lock()
	w.scanned = time.Time{}
	w.pollMu.Unlock()
	w.poll(nil)
	return w.Sample()
}

// Sample returns the usage observed so far.
func (w *Watcher) Sample() Sample {
	if w == nil {
		return Sample{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sample
}

func (w *Watcher) poll(onChange func(Sample)) {
	w.pollMu.Lock()
	var out, thinking, total, input int
	if w.source.present() {
		// The provider is resolved per poll rather than trusted from attach:
		// EnableOpenCodeDB(false) withdraws it, and a watcher that kept the
		// binding it was built with would go on reading the operator's
		// database after the opt-out. A withdrawn provider reports nothing,
		// so the sample stays where it was instead of jumping.
		src, ok := sourceFor(w.tool)
		if !ok {
			w.pollMu.Unlock()
			return
		}
		v, ok := w.readSource(src)
		if !ok {
			w.pollMu.Unlock()
			return
		}
		out, thinking, total, input = v.output, v.thinking, v.total, v.input
	} else {
		// A definition can be reloaded under a running watcher, so its adapter
		// is re-derived each poll; the same reason the source is re-resolved.
		w.refreshAdapter()
		for _, path := range w.candidates() {
			w.readNew(path)
		}
		for _, v := range w.seen {
			out = satAdd(out, v.output)
			thinking = satAdd(thinking, v.thinking)
			input = satAdd(input, v.input)
		}
		for _, v := range w.total {
			total = max(total, v)
		}
	}
	w.mu.Lock()
	grew := out > w.sample.Output || total > w.sample.Total || thinking > w.sample.Thinking || input > w.sample.Input
	if grew {
		w.sample = Sample{Output: out, Thinking: thinking, Total: total, Input: input, At: time.Now()}
	}
	s := w.sample
	w.mu.Unlock()
	w.pollMu.Unlock()
	// Callback after pollMu: onChange may Poll (final read, tests), and
	// holding the lock across it deadlocks that path.
	if grew && onChange != nil {
		onChange(s)
	}
}

// pollEvery is how often a transcript is re-read. It bounds how stale a live
// rate can be, so it is tighter than the directory rescan: reading one growing
// file is cheap, walking a store of thousands is not.
const pollEvery = 250 * time.Millisecond

// rescanEvery bounds how often the transcript store is walked. Session stores
// hold thousands of files and new ones appear rarely, so walking on every poll
// would cost far more than reading the one file that is growing.
const rescanEvery = time.Second

// recencyWindow is how long after a transcript's last write it stays in the
// walk. Attach still sees files that went idle just before we started (Watch
// runs this at since≈now); after that the window slides with wall time so a
// long-lived dashboard does not accumulate every session file ever written
// while it ran.
const recencyWindow = 2 * time.Minute

// rootListing is one walk of a transcript store, shared by every watcher
// reading the same root. Ten claude processes would otherwise WalkDir the
// same ~/.claude/projects tree independently every rescan.
type rootListing struct {
	files []string
	at    time.Time
}

var (
	rootListMu sync.Mutex
	rootLists  = map[string]rootListing{}
)

func rootListKey(root, suffix string) string { return root + "\x00" + suffix }

// pruneRootListsLocked drops listings older than maxAge. A clanker (or a
// {dir} spec) keys this map on the project path; without a bound, every tree
// an agent ever visited during a long --agents run stays pinned after the
// process is gone. Caller holds rootListMu.
func pruneRootListsLocked(now time.Time, maxAge time.Duration) {
	for k, c := range rootLists {
		if now.Sub(c.at) >= maxAge {
			delete(rootLists, k)
		}
	}
}

func (a adapter) fileSuffixes() []string {
	if len(a.suffixes) > 0 {
		return a.suffixes
	}
	return []string{a.suffix}
}

// listTranscripts returns recent files under root matching suffix. force
// bypasses the shared cache so a final Poll cannot miss a file created
// inside the last rescan window. The walk runs under rootListMu so
// concurrent watchers of the same root share one fill, and a slower empty
// result cannot overwrite a newer listing.
func listTranscripts(root, suffix string, cutoff time.Time, force bool) []string {
	key := rootListKey(root, suffix)
	rootListMu.Lock()
	defer rootListMu.Unlock()
	now := time.Now()
	if !force {
		pruneRootListsLocked(now, recencyWindow)
		if c, ok := rootLists[key]; ok && now.Sub(c.at) < rescanEvery {
			return append([]string(nil), c.files...)
		}
	}
	pruneRootListsLocked(now, rescanEvery)
	out := walkTranscripts(root, suffix, cutoff)
	rootLists[key] = rootListing{files: out, at: now}
	return append([]string(nil), out...)
}

func walkTranscripts(root, suffix string, cutoff time.Time) []string {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil
	}
	defer r.Close()
	var out []string
	_ = fs.WalkDir(r.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil || rel == "." {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(rel, suffix) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(cutoff) {
			return nil
		}
		out = append(out, filepath.Join(root, filepath.FromSlash(rel)))
		return nil
	})
	return out
}

// candidates lists transcript files recent enough to belong to this attach,
// reusing the previous walk for a few seconds. An empty result is cached too:
// until something matches, the store is walked once per rescanEvery rather
// than on every poll, and a session created in between surfaces when the
// window expires, the same bound a non-empty listing already works under.
func (w *Watcher) candidates() []string {
	force := w.scanned.IsZero()
	if !force && time.Since(w.scanned) < rescanEvery {
		return w.cached
	}
	cutoff := time.Now().Add(-recencyWindow)
	var out []string
	for _, root := range w.ad.roots(w.dir) {
		if root == "" {
			continue
		}
		for _, suffix := range w.ad.fileSuffixes() {
			out = append(out, listTranscripts(root, suffix, cutoff, force)...)
		}
	}
	w.forgetIdle(out)
	w.cached, w.scanned = out, time.Now()
	return out
}

// forgetIdle drops per-file bookkeeping for transcripts that have aged out of
// the walk and are not carrying this attach's counts. Without it, stamps,
// offsets and the owner cache grow by every session file that ever appeared
// during a long --agents run, including other projects'. Counted files keep
// their offsets so a later append is not read from the start and double-counted.
func (w *Watcher) forgetIdle(live []string) {
	keep := make(map[string]struct{}, len(live)+len(w.seen)+len(w.owner)+len(w.preexisting))
	for _, path := range live {
		keep[path] = struct{}{}
	}
	for path := range w.seen {
		keep[path] = struct{}{}
	}
	for path, mine := range w.owner {
		if mine {
			keep[path] = struct{}{}
		}
	}
	if w.ad.sessionCwd == nil {
		for path, pre := range w.preexisting {
			if pre {
				keep[path] = struct{}{}
			}
		}
	}
	var drop []string
	consider := func(path string) {
		if _, ok := keep[path]; !ok {
			drop = append(drop, path)
			keep[path] = struct{}{} // so a path in several maps is listed once
		}
	}
	for path := range w.stamps {
		consider(path)
	}
	for path := range w.offsets {
		consider(path)
	}
	for path := range w.owner {
		consider(path)
	}
	for path := range w.preexisting {
		consider(path)
	}
	for _, path := range drop {
		w.dropFile(path)
	}
}

func (w *Watcher) dropFile(path string) {
	delete(w.stamps, path)
	delete(w.offsets, path)
	delete(w.owner, path)
	delete(w.preexisting, path)
	delete(w.base, path)
	delete(w.baseThink, path)
	delete(w.baseInput, path)
	delete(w.total, path)
}

// readNew consumes the bytes appended to one transcript since the last poll.
//
// Polling runs four times a second over every recent transcript, and most of
// them are idle, so the mtime check happens on a plain stat: an untouched file
// costs one syscall instead of open+stat+close.
//
// Newline-terminated lines are always counted; a trailing fragment without
// its final newline is counted too, but only once it parses in full, and
// only then are its bytes committed to the offset. Half-written records are
// therefore re-read whole next poll instead of being silently lost, and a
// read error mid-file retries the whole span without having credited
// anything.
func (w *Watcher) readNew(path string) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	stamp := fileStamp{mtimeNanos: fi.ModTime().UnixNano(), size: fi.Size()}
	if w.stamps[path] == stamp {
		return // untouched since the last completed read
	}
	// A shrink is a rotation or rewrite: the header (and so the owner
	// verdict) may belong to a different session than the one we cached.
	if fi.Size() < w.offsets[path] {
		w.offsets[path] = 0
		delete(w.owner, path)
	}
	mine, decided := w.owns(path)
	if !decided {
		// No verdict yet, and no stamp: a transient open failure must not
		// look processed, and a header that has not been flushed yet is
		// retried until it appears. Watch already recorded the attach-time
		// offset for pre-existing files, so there is nothing to skip here.
		return
	}
	if !mine {
		w.offsets[path] = fi.Size() // keep skipping it cheaply
		w.stamps[path] = stamp
		return
	}
	f, err := w.openTranscript(path)
	if err != nil {
		return // unstamped: the next poll retries instead of treating this as done
	}
	defer f.Close()
	off := w.offsets[path]
	if fi.Size() == off {
		w.stamps[path] = stamp
		return
	}
	if _, err := f.Seek(off, 0); err != nil {
		return
	}
	var (
		recs     []values
		complete int64
		ok       bool
	)
	if isDshZstd(path) {
		recs, complete, ok = w.consumeZstd(f, off)
	} else {
		recs, complete, ok = w.consumeAppend(f, off)
	}
	if !ok {
		return // read failed: nothing counted, offset and stamp unchanged, retried next poll
	}
	for _, v := range recs {
		w.applyRecord(path, v)
	}
	w.offsets[path] = complete
	// A capped zstd read leaves bytes past this window. Stamping now would
	// make the next poll treat the file as idle and skip the rest. A torn
	// frame at EOF is still stamped: the next append changes size.
	if isDshZstd(path) && fi.Size()-off > zstdTailBytes {
		return
	}
	w.stamps[path] = stamp
}

// consumeAppend reads from off to EOF, returning parsed records and the
// offset just past the last committed record. ok is false on a mid-file
// read error so the caller leaves offset and stamp alone.
func (w *Watcher) consumeAppend(f *os.File, off int64) (recs []values, complete int64, ok bool) {
	var (
		line    []byte
		discard bool
		pos     = off
	)
	complete = off
	br := bufio.NewReaderSize(f, appendReaderBytes)
	for {
		chunk, rerr := br.ReadSlice('\n')
		pos += int64(len(chunk))
		if rerr == bufio.ErrBufferFull {
			if !discard {
				line = append(line, chunk...)
				if len(line) > maxLineBytes {
					// One record larger than the cap cannot parse; drop it
					// and resume at the next newline rather than stalling
					// every later record in this file behind it.
					discard = true
					line = line[:0]
				}
			}
			continue
		}
		if rerr == nil {
			if !discard {
				line = append(line, chunk...)
				if len(line) > maxLineBytes {
					discard = true
				} else {
					l := line[:len(line)-1] // drop the newline
					if n := len(l); n > 0 && l[n-1] == '\r' {
						l = l[:n-1]
					}
					recs = w.collect(recs, l)
				}
			}
			// discard is per record: a newline ends it so later lines
			// in this same consume are still counted.
			discard = false
			line = line[:0]
			complete = pos
			continue
		}
		if errors.Is(rerr, io.EOF) {
			// A trailing fragment is either a complete record whose writer
			// omitted the final newline, or half of one mid-write. It counts
			// only when it parses in full: a torn prefix stays uncommitted
			// and is re-read whole next poll instead of being lost.
			if !discard && (len(line) > 0 || len(chunk) > 0) {
				line = append(line, chunk...)
				if len(line) <= maxLineBytes {
					if v, cwd, parsed := w.ad.parse(line); parsed {
						recs = w.collectValue(recs, v, cwd)
						complete = pos
					}
				}
			}
			return recs, complete, true
		}
		return nil, 0, false
	}
}

// maxLineBytes bounds one transcript record: a single JSONL line larger than
// this is junk no parser here accepts.
const maxLineBytes = 8 << 20

// appendReaderBytes is the bufio fill size for transcript reads. A typical
// JSONL record fits; a giant one is assembled across fills until maxLineBytes.
const appendReaderBytes = 64 << 10

func (w *Watcher) collect(recs []values, line []byte) []values {
	v, cwd, ok := w.ad.parse(line)
	if !ok {
		return recs
	}
	return w.collectValue(recs, v, cwd)
}

func (w *Watcher) collectValue(recs []values, v values, cwd string) []values {
	// A transcript that names a different working directory belongs to
	// another process, or another project entirely.
	if cwd != "" && !w.sameDir(cwd) {
		return recs
	}
	if len(recs) == 0 || (w.ad.kind == cumulative && len(recs) == 1) {
		return append(recs, v)
	}
	cur := &recs[len(recs)-1]
	if w.ad.kind == cumulative {
		cur.output = max(cur.output, v.output)
		cur.thinking = max(cur.thinking, v.thinking)
		cur.input = max(cur.input, v.input)
	} else {
		cur.output = satAdd(cur.output, v.output)
		cur.thinking = satAdd(cur.thinking, v.thinking)
		cur.input = satAdd(cur.input, v.input)
	}
	cur.total = max(cur.total, v.total)
	return recs
}

// applyRecord folds one counted record into the per-file bookkeeping.
func (w *Watcher) applyRecord(path string, v values) {
	switch w.ad.kind {
	case perMessage:
		cur := w.seen[path]
		cur.output = satAdd(cur.output, v.output)
		cur.thinking = satAdd(cur.thinking, v.thinking)
		cur.input = satAdd(cur.input, v.input)
		w.seen[path] = cur
		// Output and input accrue per message (billed tokens). A "total" on
		// a per-message record is the context size at that point, so summing
		// it would be meaningless. The largest one seen is the honest figure.
		w.total[path] = max(w.total[path], v.total)
	case cumulative:
		if _, have := w.base[path]; !have {
			// Attaching mid-session, everything before now belongs to a
			// previous run, so the first value seen is the baseline. A
			// session that only appeared after attach is ours in full, and
			// baselining it would throw the first reading away, which on a
			// short watch is most of the signal.
			if w.preexisting[path] {
				w.base[path] = v.output
				w.baseThink[path] = v.thinking
				w.baseInput[path] = v.input
			} else {
				w.base[path], w.baseThink[path], w.baseInput[path] = 0, 0, 0
			}
		}
		cur := w.seen[path]
		if d := v.output - w.base[path]; d > cur.output {
			cur.output = d
		}
		if d := v.thinking - w.baseThink[path]; d > cur.thinking {
			cur.thinking = d
		}
		if d := v.input - w.baseInput[path]; d > cur.input {
			cur.input = d
		}
		w.seen[path] = cur
		if v.total > w.total[path] {
			w.total[path] = v.total
		}
	}
}

// ownerScanLines bounds the header search. These formats carry the working
// directory in the opening record, so a file this deep without one has none.
const ownerScanLines = 20

// owns reports whether a transcript belongs to this working directory. For
// agents whose usage lines repeat the cwd there is nothing to decide here;
// for the others the session header at the top of the file is read once and
// cached.
//
// The second return value says whether the verdict is final. A file caught
// between creation and its header flush yields no verdict yet: caching a
// refusal there would blackhole a session that in fact belongs here, so the
// decision is retried on a later poll instead.
func (w *Watcher) owns(path string) (mine, decided bool) {
	if w.ad.sessionCwd == nil {
		return true, true // decided per line instead
	}
	if mine, known := w.owner[path]; known {
		return mine, true
	}
	f, err := w.openTranscript(path)
	if err != nil {
		return false, false // transient: retry next poll
	}
	defer f.Close()
	if isDshZstd(path) {
		return w.ownsZstd(path, f)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	lines := 0
	for lines < ownerScanLines && sc.Scan() {
		if cwd, ok := w.ad.sessionCwd(sc.Bytes()); ok {
			mine := w.sameDir(cwd)
			w.owner[path] = mine
			return mine, true
		}
		lines++
	}
	if lines < ownerScanLines {
		// The file ended before the cap: the header may simply not be
		// written yet. Stay undecided and uncached.
		return false, false
	}
	// The scan cap was reached with no header anywhere in it, so there is
	// nothing to wait for: refuse durably rather than credit another
	// project's tokens to this working directory.
	w.owner[path] = false
	return false, true
}

func (w *Watcher) sameDir(cwd string) bool {
	if sameSpelling(cwd, w.dir) {
		return true
	}
	resolved, err := filepath.EvalSymlinks(cwd)
	return err == nil && sameSpelling(resolved, w.dir)
}

// satAdd sums two non-negative counters, saturating instead of wrapping: a
// transcript with absurd counts must read as enormous, never as negative.
func satAdd(a, b int) int {
	s := a + b
	if s < 0 {
		return math.MaxInt
	}
	return s
}

func satSub(a, b int) int {
	if a < b {
		return 0
	}
	return a - b
}
