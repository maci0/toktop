// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

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
	stamps map[string]fileStamp
	owner  map[string]bool // file -> belongs to this working directory (cached)
	// dirVerdict memoizes sameDir per cwd string. A store under ~/.claude
	// repeats the same handful of project directories on every record, and
	// each miss costs a full EvalSymlinks walk; the map turns that per-record
	// cost into a map probe. It is cleared when it outgrows dirVerdictMax,
	// so a store full of one-off cwds cannot grow it without bound.
	dirVerdict map[string]bool
	// defsGen is the definitions generation w.ad was derived from, and
	// fromDefs whether it was derived at all: see refreshAdapter.
	defsGen  uint64
	fromDefs bool
	// roots is the expanded form of ad.roots(w.dir), cached for the same
	// reason the adapter is: it is recomputed on every openTranscript.
	roots []string
	// zstdCarry holds the unterminated trailing record of a zstd transcript,
	// which the frame-boundary read in consumeZstd cannot tell from a
	// complete one. Its head is prepended to the next window's first line.
	zstdCarry map[string][]byte
	cached    []string // candidate files, refreshed on an interval
	scanned   time.Time
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

	// now is the watcher's own clock. It stamps the published sample, which
	// becomes an event id and a feed timestamp, and it ages the recency and
	// rescan windows in candidates.go. The transcript mtimes those windows are
	// compared against stay wall time in every mode. Guarded by mu, like
	// sample: SetNow and read run on different goroutines whenever a caller
	// sets the clock after starting Run.
	now func() time.Time

	mu     sync.Mutex
	sample Sample
}

// fileStamp is what a transcript looked like when it was last read.
type fileStamp struct {
	mtimeNanos int64
	size       int64
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
// should treat as "no rate available" rather than an error. Err names that
// case for a caller that reports it:
//
//	if w := agentusage.Watch(tool, dir, time.Now()); w.Err() != nil {
//		// no readable usage for this agent
//	}
func Watch(tool, dir string, since time.Time) *Watcher {
	tool = canonicalTool(tool)
	if source, ok := sourceFor(tool); ok {
		w := &Watcher{source: source, tool: tool, dir: resolveDir(dir), dirs: dirSpellings(dir), since: since, now: time.Now}
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
		ad: ad, tool: tool, dir: resolveDir(dir), since: since, now: time.Now,
		offsets: map[string]int64{}, preexisting: map[string]bool{}, stamps: map[string]fileStamp{},
		zstdCarry: map[string][]byte{},
		owner:     map[string]bool{}, dirVerdict: map[string]bool{},
		base: map[string]int{}, baseThink: map[string]int{},
		baseInput: map[string]int{},
		seen:      map[string]values{}, total: map[string]int{},
	}
	// Record where existing files end before anything is counted. Every
	// transcript already in the store is seeded, not only the ones written in
	// the last recencyWindow: a session that went idle before the dashboard
	// started is still on disk, and leaving its end unrecorded would make the
	// next append to it be read from byte zero, crediting the whole earlier
	// session to this attach.
	//
	// The window is anchored on since, the instant the caller passed in, not a
	// second clock read here: the attach walk is then a function of its
	// arguments alone, so a caller replaying a run reproduces the same set of
	// seeded files. Callers pass the launch instant, which is what
	// time.Now() gave them at attach.
	recent := since.Add(-recencyWindow)
	for _, path := range w.attachCandidates() {
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		w.offsets[path] = fi.Size()
		w.preexisting[path] = true
		if ad.kind == cumulative && fi.ModTime().After(recent) {
			// Cumulative counters only make sense against what the session had
			// already spent. That value is in the bytes being skipped, so it is
			// read once here; without it the first record after attaching would
			// become the baseline and this attach would measure zero forever.
			// Only recent transcripts get it: a machine-wide store holds
			// thousands, and a session idle for minutes baselining on its
			// first post-attach record costs a few seconds of delta, not its
			// history.
			w.seedBaseline(path)
		}
	}
	// The seeding walk serves attach bookkeeping, not reads: leave the listing
	// unstamped so the first poll re-walks and sees sessions created between
	// attach and then. From that poll on, empty and non-empty listings share the
	// same rescanEvery freshness window.
	w.scanned = time.Time{}
	return w
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

// Err reports whether this watcher can read anything, and is the reason to
// show a caller who asked for one it cannot have.
//
// A watcher is either usable or nil: Watch never returns one that is bound to
// nothing, so Err is nil on every live watcher and matches
// [ErrUnsupportedTool] on the nil one. Like Tool and Dir it is safe to call on
// the result without a nil check:
//
//	if w := agentusage.Watch(tool, dir, time.Now()); w.Err() != nil {
//		return w.Err() // the agent is known, but keeps nothing readable here
//	}
//
// A nil watcher means the agent is unknown here, or keeps transcripts no build
// of this package can read, or has a definition naming no roots. The error says
// only that: the caller already knows which agent it asked about.
func (w *Watcher) Err() error {
	if w != nil {
		return nil
	}
	return fmt.Errorf("%w: no usage source is registered for this agent", ErrUnsupportedTool)
}

// SetNow overrides the clock that stamps published samples, and with them the
// event ids a caller derives from them. Call before Run: a simulated or frozen
// clock replays the same readings onto the same timeline, so a replayed run
// produces the same ids and the dashboard's id window drops the duplicates
// instead of counting them twice.
//
// The windows this watcher applies to itself, the recency window and the
// transcript rescan interval, follow this clock too, so a run steps them
// instead of waiting them out. What it does not move is the other side of
// every comparison: file mtimes and the session-store timestamps that since
// bounds stay wall time, because that is the clock the filesystem and the
// stores record in. Anchor the injected clock to the wall instant the run
// started and the two agree.
func (w *Watcher) SetNow(fn func() time.Time) {
	if w == nil {
		return
	}
	if fn == nil {
		fn = time.Now
	}
	w.mu.Lock()
	w.now = fn
	w.mu.Unlock()
}

// clock returns the injected stamp clock, or the wall clock for a Watcher built
// without one. It reads the field under mu, and callers invoke the result with
// no lock held: the clock is caller-supplied, and calling it under mu is a
// self-deadlock the moment it re-enters the watcher.
func (w *Watcher) clock() func() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.now == nil {
		return time.Now
	}
	return w.now
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
	for _, root := range w.rootsLocked() {
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

// rootsLocked returns this watcher's expanded roots, deriving them the first
// time. openTranscript runs once per changed transcript per poll, and
// ad.roots re-expands ~ and substitutes {dir} on every call; the roots of a
// fixed adapter and directory do not move, so they are computed once and
// dropped again when refreshAdapter swaps the adapter.
//
// Caller holds pollMu.
func (w *Watcher) rootsLocked() []string {
	if w.roots == nil {
		w.roots = w.ad.roots(w.dir)
	}
	return w.roots
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
	var (
		recs []values
		ok   bool
	)
	if isDshZstd(path) {
		recs, _, ok = w.consumeZstd(path, f, 0)
	} else {
		recs, _, ok = w.consumeAppend(f, off)
	}
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
// observed usage changes, growth or the drop a rewritten transcript causes.
// A caller that reports deltas should re-baseline on a sample smaller than
// the one it last reported; [Sample.Delta] is that rule in one call. It is
// meant to run in its own goroutine.
//
// every is how often the transcripts are re-read; a non-positive value uses
// the package default (250ms).
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
			if ctx.Err() != nil {
				w.poll(onChange)
				return
			}
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
	// rescanEvery would otherwise report nothing at all. The stamp is cleared
	// inside read, under the same pollMu the walk runs under, so a ticker poll
	// landing between the two cannot re-stamp scanned and make this read
	// reuse its listing. The cost is one walk per final read, not one per
	// periodic poll: Run's ticker goes through poll, which still reuses the
	// cached listing.
	w.pollForce(nil, true)
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

func (w *Watcher) poll(onChange func(Sample)) { w.pollForce(onChange, false) }

func (w *Watcher) pollForce(onChange func(Sample), force bool) {
	s, changed := w.read(force)
	// Callback after read has released pollMu: onChange may Poll (final read,
	// tests), and holding the lock across it deadlocks that path.
	if changed && onChange != nil {
		onChange(s)
	}
}

// read takes one reading and publishes it, reporting the new sample only when
// the observed counts differ from the published ones. Both mutexes are
// released by defer, so a panic
// raised while parsing an agent's transcript cannot leave a watcher holding
// pollMu for the rest of the dashboard's life with no goroutine left to
// unlock it.
//
// force clears the listing's freshness stamp before the walk, so this read
// re-lists its roots rather than reusing a listing another poll stamped while
// this call was between the two.
func (w *Watcher) read(force bool) (Sample, bool) {
	w.pollMu.Lock()
	defer w.pollMu.Unlock()
	if force {
		w.scanned = time.Time{}
	}
	var out, thinking, total, input int
	if w.source.present() {
		// The provider is resolved per poll rather than trusted from attach:
		// EnableOpenCodeDB(false) withdraws it, and a watcher that kept the
		// binding it was built with would go on reading the operator's
		// database after the opt-out. A withdrawn provider reports nothing,
		// so the sample stays where it was instead of jumping.
		src, ok := sourceFor(w.tool)
		if !ok {
			return Sample{}, false
		}
		v, ok := w.readSource(src)
		if !ok {
			return Sample{}, false
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
	// Stamped before mu is taken: reading the clock needs mu, and calling it
	// under mu would deadlock against this watcher's own accessor.
	at := w.clock()()
	w.mu.Lock()
	defer w.mu.Unlock()
	// Counts are what the transcripts say right now, not a running total that
	// only ever rises: a transcript rewritten to a shorter length is re-read
	// from its start, and the figures it replaces are ones it no longer
	// records. Publishing the drop is what keeps a rewrite from being billed
	// twice; callers that differencing see a smaller sample and re-baseline.
	changed := out != w.sample.Output || total != w.sample.Total ||
		thinking != w.sample.Thinking || input != w.sample.Input
	if changed {
		w.sample = Sample{Output: out, Thinking: thinking, Total: total, Input: input, At: at}
	}
	return w.sample, changed
}
