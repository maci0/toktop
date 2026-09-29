// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// DefaultPollInterval is how often a transcript is re-read, and what
// [Watcher.Run] polls at when its interval is not positive. It bounds how stale
// a live rate can be, so it is tighter than the directory rescan: reading one
// growing file is cheap, walking a store of thousands is not.
//
// It is exported so a caller naming an interval explicitly passes the package's
// own default rather than a number copied out of this documentation, which
// drifts the moment either side changes.
const DefaultPollInterval = 250 * time.Millisecond

// rescanEvery bounds how often the transcript store is walked. Session stores
// hold thousands of files and new ones appear rarely, so walking on every poll
// would cost far more than reading the one file that is growing.
const rescanEvery = time.Second

// recencyWindow is how long after a transcript's last write it stays in the
// walk. Attach deliberately ignores it and seeds every file the store holds
// (Watch runs this at since≈now), so an idle session's history is skipped
// rather than unread; from the first poll on, the window slides with the
// watcher's clock so a long-lived dashboard does not accumulate every session
// file ever written while it ran, and so a run driven by an injected clock
// ages the window in steps it controls rather than in whatever wall time the
// test happened to take. The mtimes it is compared against are still wall
// time: that is the clock the filesystem records in.
const recencyWindow = 2 * time.Minute

// walkWait bounds how long a caller waits on another goroutine's walk of the
// same root. It is well past the rescan interval a healthy walk fits inside, so
// hitting it means the filesystem, not the walk, is stuck.
const walkWait = 2 * time.Second

// rootListing is one walk of a transcript store, shared by every watcher
// reading the same root. Ten claude processes would otherwise WalkDir the
// same ~/.claude/projects tree independently every rescan.
type rootListing struct {
	files []string
	at    time.Time
	// walk is closed when the walk in flight for this key finishes, and is nil
	// when none is. A caller that finds one waits for it rather than starting a
	// second walk over the same tree.
	walk chan struct{}
}

var (
	rootListMu sync.Mutex
	rootLists  = map[string]rootListing{}
)

// audit builds the logger for the lines this file writes. It starts at the
// process logger, so a program that embeds this package hears about a walk
// that could not finish without configuring anything, and SetLogger hands it
// the host's own logger (toktop hands it the audit log). A var, so a test can
// point it at a handler it can read.
//
// Every read goes through auditLogger, and every write through SetLogger, both
// under auditMu. The watcher read loops call auditLogger from their own
// goroutines, and a Go func value is two words: a host that calls SetLogger
// after starting a watcher could otherwise leave a reader calling a new code
// pointer against the old closure word.
var (
	auditMu sync.Mutex
	audit   = slog.Default
)

// auditLogger returns the logger audit lines go to. Caller must not hold
// auditMu.
func auditLogger() *slog.Logger {
	auditMu.Lock()
	defer auditMu.Unlock()
	return audit()
}

// SetLogger sends the lines this package audits to l. The default is the
// process logger from [slog.Default]; a program that reads agents alongside
// its own output passes the logger it already writes to, so a failed walk is
// one line in the stream the program keeps rather than a second stream it
// does not. A nil logger restores the default.
//
// The lines carry no program name, because which program is reading agents is
// the host's to say. A host that needs its own name in the message prepends
// it in the handler it passes.
func SetLogger(l *slog.Logger) {
	auditMu.Lock()
	defer auditMu.Unlock()
	if l == nil {
		audit = slog.Default
		return
	}
	audit = func() *slog.Logger { return l }
}

// rootListKey names a listing by the root and the whole suffix set it was
// walked for, not by one suffix: an adapter with two suffixes (dsh's zstd and
// plain session logs) reads both out of the same tree, and a key per suffix
// would walk it twice per rescan, one lstat per file each time.
func rootListKey(root string, suffixes []string) string {
	return root + "\x00" + strings.Join(suffixes, "\x00")
}

// auditWalkFailure writes the one line a transcript walk that could not
// finish produces. Both values on it are folded to "~": the root is the
// store, and the error is the path the walk failed on, which is under that
// store and names the account the same way. Folding the root alone would
// leave the account spelled out in the value beside it, and a host that
// installs its own logger (SetLogger) has no fold of its own to catch it.
func auditWalkFailure(root string, err error) {
	auditLogger().Warn("agent transcript walk failed",
		"root", redactStorePath(root),
		"error", redactStorePath(core.Snippet([]byte(err.Error()))))
}

// pruneRootListsLocked drops listings older than maxAge. A clanker (or a
// {dir} spec) keys this map on the project path; without a bound, every tree
// an agent ever visited during a long --agents run stays pinned after the
// process is gone. Caller holds rootListMu.
//
// An entry with a walk in flight is never dropped, however old its placeholder
// looks. The placeholder is stamped at the start of the walk, so a walk slower
// than maxAge (a store with tens of thousands of files) is indistinguishable
// from a stale result by age alone. Dropping it would drop the claim too, and
// a second goroutine would walk the same tree concurrently; whichever finished
// last would publish, so a walk that started earlier could overwrite a newer
// listing and stamp a later at, suppressing a real refresh for a full maxAge.
// A walk that outlives every watcher is released by the walk itself, not here.
func pruneRootListsLocked(now time.Time, maxAge time.Duration) {
	for k, c := range rootLists {
		if c.walk == nil && core.Age(now, c.at) >= maxAge {
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

// hasAnySuffix reports whether name carries one of suffixes, so a walk can
// serve a whole suffix set without a walk per suffix.
func hasAnySuffix(name string, suffixes []string) bool {
	for _, s := range suffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// listTranscripts returns recent files under root matching any of suffixes.
// force bypasses the shared cache so a final Poll cannot miss a file created
// inside the last rescan window.
//
// now is the calling watcher's clock, and the cache is stamped with it rather
// than a wall-clock read: a watcher driven by an injected clock decides when
// its listings go stale, so two runs of the same seed return the same files
// however long each took. The cache is process-wide, so a caller that mixes
// clocks shares freshness with the others; mixing is a configuration error,
// not a supported mode.
//
// Concurrent watchers of the same root and suffix set share one walk, and only
// that walk blocks: a store with thousands of files takes long enough that
// holding rootListMu across it stalled every watcher reading an unrelated root,
// and with one goroutine per agent process one slow store paused all of them.
// The walk therefore runs outside the map lock, with an in-flight channel as the
// per-root claim on it. A slower empty result still cannot overwrite a newer
// listing: only the goroutine holding the claim writes one.
//
// The second result says whether the listing is the whole answer for this
// root. It is false for a walk that could not finish and for a caller that gave
// up waiting on one, both of which report the absence of files rather than
// their absence from the store. A caller that keeps a listing may not record
// either as its own: stamping one bounds how long the missing half of the store
// goes unread, and ageing bookkeeping against it releases the read positions of
// transcripts that are still on disk, which is what re-reads them from byte
// zero and bills a session twice.
func listTranscripts(root string, suffixes []string, cutoff, now time.Time, force bool) ([]string, bool) {
	key := rootListKey(root, suffixes)
	for {
		rootListMu.Lock()
		pruneRootListsLocked(now, rescanEvery)
		c := rootLists[key]
		if !force && c.walk == nil && !c.at.IsZero() && core.Age(now, c.at) < rescanEvery {
			out := append([]string(nil), c.files...)
			rootListMu.Unlock()
			return out, true
		}
		if c.walk != nil {
			// A walk for this root is already running. Wait for its result and
			// re-check: the walk it publishes answers this call, and a force
			// caller that lost the race still gets one of its own.
			//
			// The wait is bounded. A walk over a store on a stalled mount can
			// block in the kernel, and nothing can cancel it; an unbounded wait
			// here would wedge every watcher sharing the root and the shutdown
			// that drains them. A tick that gives up reports no transcripts and
			// the next one re-claims the walk once it lands.
			walk := c.walk
			rootListMu.Unlock()
			// A stopped timer, not time.After: taking the <-walk branch leaves a
			// two-second timer behind in the runtime heap for a wait that is
			// already over, and contended walks park many callers per tick.
			timer := time.NewTimer(walkWait)
			select {
			case <-walk:
				timer.Stop()
				continue
			case <-timer.C:
				auditWalkFailure(key, fmt.Errorf("gave up after %s waiting for a transcript walk", walkWait))
				return nil, false
			}
		}
		// This call owns the walk for key. The placeholder carries the current
		// instant and an open walk, so the prune pass leaves it alone however
		// long the walk runs, and no files, so a reader that arrives now takes
		// the wait branch above rather than reading an absent result as an
		// empty store.
		done := make(chan struct{})
		rootLists[key] = rootListing{at: now, walk: done}
		rootListMu.Unlock()

		// The claim is released from a defer, not at the end of the body. Every
		// other caller for this key parks on <-walk, bounded by walkWait above,
		// and the goroutine holding the walk is a watcher's Run loop, which owns
		// pollMu and nothing else can release: a panic here would wedge one
		// agent's ticker forever and then hang agentwatch.stopOne on its done
		// channel, taking the whole discovery pass with it. Stamped with the
		// instant the walk started, not a second clock read, so the listing's age
		// stays a function of the caller's clock alone.
		var (
			files  []string
			fresh  = now
			walked bool
		)
		defer func() {
			// Only a walk that ran to completion publishes a fresh stamp. A panic
			// in walkTranscripts arrives here with walked still false, and
			// publishing then would install an empty listing dated now, which every
			// other watcher on this root would serve for a whole rescan window.
			if !walked {
				fresh = time.Time{}
			}
			rootListMu.Lock()
			rootLists[key] = rootListing{files: files, at: fresh}
			close(done)
			rootListMu.Unlock()
		}()

		files, err := walkTranscripts(root, suffixes, cutoff)
		complete := err == nil
		walked = true
		if err != nil {
			// A walk that could not finish is not an empty store, and caching
			// its partial result under a fresh stamp would read as one: every
			// session under the subtree that failed would go unreported for a
			// whole rescan window, with nothing on screen to say why. The claim
			// is still released, but the entry is left unstamped so the next
			// caller re-walks rather than serving this one, and the reason is
			// audited because the only other symptom is agents reporting no
			// tokens.
			auditWalkFailure(root, err)
			files, fresh = nil, time.Time{}
		}
		return append([]string(nil), files...), complete
	}
}

// walkTranscripts lists the transcripts under one root that match any of
// suffixes, reporting a failure to finish. A walk that stops partway (an
// unreadable subtree, a filesystem error) has seen some of the store and not the
// rest, which is a different answer from an empty one: the caller must not cache
// the partial list as a fresh listing.
//
// One walk serves the whole suffix set: dsh's store holds both compressed and
// plain session logs, and walking it once per suffix paid the tree traversal
// and the stat of every file in it twice per rescan for a list that is the
// union either way.
//
// A root that is not there is an empty store. Clanker keeps its log in
// <project>/state, which does not exist until the agent writes it, and a
// process whose working directory was a deleted build temp is the same
// answer. Warning on every rescan logged one line per vanished directory.
// A root that exists but cannot be opened is still a failure.
func walkTranscripts(root string, suffixes []string, cutoff time.Time) ([]string, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer r.Close()
	var out []string
	// Recorded rather than returned: the walk continues past a skipped entry
	// so one unreadable subtree does not abandon the rest of the store, and
	// the error is what tells the caller the list is short.
	var incomplete error
	err = fs.WalkDir(r.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			// Swallowing it would let the walk finish over the rest and hand
			// back a partial list indistinguishable from a whole one, which is
			// the result the caller must be told about. Skip just this entry:
			// the walk continues to the end either way, so one unreadable
			// subtree does not cost the rest of the store.
			//
			// A path that is not there is the ordinary race between the walk
			// and a session being rotated, and a store that loses files every
			// poll would audit a failure on every rescan and publish nothing.
			// Only a store that could not be read is a failure.
			if incomplete == nil && !errors.Is(err, fs.ErrNotExist) {
				incomplete = fmt.Errorf("walk %s: %w", rel, err)
			}
			return nil
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() || !hasAnySuffix(rel, suffixes) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(cutoff) {
			return nil
		}
		out = append(out, filepath.Join(root, filepath.FromSlash(rel)))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if incomplete != nil {
		return nil, incomplete
	}
	return out, nil
}

// candidates lists transcript files recent enough to belong to this attach,
// reusing the previous walk for a few seconds. An empty result is cached too:
// until something matches, the store is walked once per rescanEvery rather
// than on every poll, and a session created in between surfaces when the
// window expires, the same bound a non-empty listing already works under.
func (w *Watcher) candidates() []string {
	return w.walkCandidates(w.instant().Add(-recencyWindow), true)
}

// attachCandidates lists every transcript in the store, however long it has
// been idle, so Watch can record where each one ends. The recency window
// exists to bound a long run's memory; applying it at attach would leave an
// idle session's history both unread and unskipped, and the next append to it
// would be read from byte zero. The result never enters the shared listing
// cache, so a polling watcher never inherits the wider set.
func (w *Watcher) attachCandidates() []string {
	return w.walkCandidates(time.Time{}, false)
}

// walkCandidates lists the transcripts under the adapter's roots that are
// newer than cutoff. With cache it shares (and refreshes) the process-wide
// listing and the watcher's own freshness window; without it the walk stands
// alone, which is what the attach-time seed needs.
//
// Every instant here is the watcher's own clock, so the recency cutoff and
// the rescan window advance together under an injected clock instead of on
// wall time. A replay that steps the clock one poll interval at a time ages
// the listing the same way however long the replay actually took.
func (w *Watcher) walkCandidates(cutoff time.Time, cache bool) []string {
	if w.adGone {
		// The definition that named this agent's store was withdrawn, so there
		// is no tree to walk. Falling through would use the adapter the
		// previous generation left behind, which is the disowned store.
		return nil
	}
	now := w.instant()
	force := w.scanned.IsZero()
	if cache && !force && core.Age(now, w.scanned) < rescanEvery {
		return w.cached
	}
	// The roots are derived again for each walk rather than once per watcher.
	// A definition reloaded under a running watcher can redirect them, which
	// is why refreshAdapter drops them, and kimi keeps one session directory
	// per working directory: a watcher that attached before the agent's first
	// session in this directory has to see that directory appear. roots() is a
	// function of the working directory and this walk's instant, and this is
	// one call per rescan window rather than one per poll.
	w.roots = nil
	var (
		out      []string
		complete = true
	)
	// One walk per root for the whole suffix set, the way listTranscripts keys
	// its shared listing: the files are the union either way, and a store with
	// two extensions in it (dsh) was paying the traversal and the per-file stat
	// twice per rescan to reach the same answer.
	suffixes := w.ad.fileSuffixes()
	for _, root := range w.rootsLocked(now) {
		if root == "" {
			continue
		}
		if cache {
			files, ok := listTranscripts(root, suffixes, cutoff, now, force)
			if !ok {
				complete = false
			}
			out = append(out, files...)
			continue
		}
		files, err := walkTranscripts(root, suffixes, cutoff)
		if err != nil {
			// The attach walk has no listing cache to leave unstamped, so it
			// is audited here and the read is abandoned: seeding offsets from
			// a store that could not be walked would record "read to the
			// end" for files the walk never reached, and the next append to
			// one of them would be skipped.
			auditWalkFailure(root, err)
			return nil
		}
		out = append(out, files...)
	}
	if !cache {
		return out
	}
	// A walk that missed part of the store is not a listing to remember. The
	// shared cache already refuses to stamp one; stamping it here as well would
	// keep this watcher off its roots for a rescan window, and forgetIdle would
	// read the shortfall as files aged out and release the read positions of
	// transcripts that are still on disk. The next poll re-walks, and the
	// freshness stamp stays zero so the walk is claimed rather than served.
	if !complete {
		// The recency sweep cannot run on a partial listing, but the caps that
		// bound the per-file bookkeeping can: a walk that missed part of the
		// store is exactly the walk a store produces while it grows, and
		// skipping the caps for as long as one root keeps failing to walk
		// leaves every map keyed by path unbounded for the life of the run.
		// The last complete listing stands in for the shortfall, so a file
		// merely missed by this walk is not mistaken for an aged-out one.
		w.capFileState(append(slices.Clone(w.cached), out...))
		return out
	}
	w.forgetIdle(out)
	w.cached, w.scanned = out, now
	return out
}

// forgetIdle drops per-file bookkeeping for transcripts that have aged out of
// the walk and are not carrying this attach's counts. Without it, stamps,
// offsets and the owner cache grow by every session file that ever appeared
// during a long --agents run, including other projects'. Counted files keep
// their offsets so a later append is not read from the start and double-counted,
// and an own verdict outlives the window for the same reason, bounded by
// trimOwned.
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
	if !w.ad.perFileOwner() {
		for path, pre := range w.preexisting {
			if pre {
				keep[path] = struct{}{}
			}
		}
	} else {
		// A transcript that was on disk when this watcher attached is only
		// forgotten once it is judged to belong to another project: an
		// unjudged one is exactly the idle session whose skip position would
		// otherwise be lost, and reading it later from byte zero credits its
		// whole history to this attach.
		for path, pre := range w.preexisting {
			if mine, judged := w.owner[path]; pre && !(judged && !mine) {
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
	// The failure latches are per-file bookkeeping like the rest, and a
	// transcript that never opened or was never attributed is in none of the
	// maps above: it has no stamp and no offset to age it out. Without this a
	// store that fails to read or to attribute (a partial mount, a store
	// written by a newer build) leaves one latch entry per file it ever failed
	// on, for the life of the run.
	for path := range w.readFailed {
		consider(path)
	}
	for path := range w.ownsFailed {
		consider(path)
	}
	for _, path := range drop {
		w.dropFile(path)
	}
	w.capFileState(live)
}

// capFileState applies the two backstops that bound the bookkeeping for
// transcripts that aged out of the walk. It runs on every walk, including one
// that missed part of the store: forgetIdle above cannot, because a file
// missing from a partial listing may still be on disk, but these two are the
// only bound on how far the maps grow, and a store whose walk fails while its
// other root keeps producing sessions is the case they exist for. live is the
// set of paths to spare, so a caller walking a partial listing passes what it
// did see alongside what the last complete walk saw.
func (w *Watcher) capFileState(live []string) {
	inWalk := make(map[string]struct{}, len(live))
	for _, path := range live {
		inWalk[path] = struct{}{}
	}
	w.trimCounted(inWalk)
	w.trimOwned(inWalk)
	w.trimLatches(inWalk)
}

// latchCap bounds the failure latches held for transcripts outside the walk.
// Same value as the caps above, for the same reason: forgetIdle ages these out
// only when a walk covers the whole store, so a store that fails to walk while
// its other root keeps starting sessions would leave one latch per file it ever
// failed on, for the life of the run.
//
// A release costs one more audit line the next time that file fails, and only
// for a file this walk did not reach, so the cap is spent on the least
// recently written of them.
const latchCap = stateCap

// trimLatches releases the latches of transcripts past latchCap that the walk
// did not reach. A latch on a transcript still in the walk is kept whatever its
// age: dropping it would re-log the same unreadable file on every poll.
func (w *Watcher) trimLatches(inWalk map[string]struct{}) {
	cut := make([]aged, 0, len(w.readFailed)+len(w.ownsFailed))
	listed := make(map[string]struct{}, cap(cut))
	add := func(m map[string]bool) {
		for path := range m {
			if _, ok := inWalk[path]; ok {
				continue
			}
			if _, dup := listed[path]; dup {
				continue
			}
			listed[path] = struct{}{}
			cut = append(cut, aged{path: path, mtimeNanos: w.stamps[path].mtimeNanos})
		}
	}
	add(w.readFailed)
	add(w.ownsFailed)
	if len(cut) <= latchCap {
		return
	}
	slices.SortFunc(cut, func(a, b aged) int { return cmp.Compare(a.mtimeNanos, b.mtimeNanos) })
	for _, a := range cut[:len(cut)-latchCap] {
		delete(w.readFailed, a.path)
		delete(w.ownsFailed, a.path)
	}
}

// countedCap bounds the per-file bookkeeping for transcripts that have aged
// out of the walk but still carry this attach's counts. Those are kept so a
// later append is read from where it stopped instead of from byte zero, and
// every one of them is an entry in seen, total, base, baseThink, baseInput,
// stamps and offsets. A watcher left running against a long-lived agent would
// otherwise hold one of each per session file written while it ran.
const countedCap = 512

// aged is a transcript the trim passes may release, with the last write the
// stamp recorded. Oldest first, since the transcript least likely to be
// appended to is the one whose remaining growth is cheapest to lose.
type aged struct {
	path       string
	mtimeNanos int64
}

// trimCounted releases the bookkeeping of the least recently written counted
// transcripts once there are more than countedCap of them. Ageing a file out
// is the recency window's job; this is the backstop for a store that keeps
// every file inside it, where the window never drops anything.
//
// A released transcript is not forgotten, only cut loose: its offset is seeded
// to the file's current end, so a later append is still read as growth rather
// than re-read from byte zero. What is lost is the remainder of a session that
// went idle while the run held more sessions than the cap. The published
// sample drops by that much, and Sample.Delta reports no growth, so the next
// reading is measured from the smaller figure.
//
// A path released here re-enters trimOwned's budget below on the next rescan:
// the counts are gone, so it is a judged transcript carrying nothing, and the
// skip position alone is what the owner verdict exists to keep.
func (w *Watcher) trimCounted(inWalk map[string]struct{}) {
	if len(w.seen) <= countedCap {
		return
	}
	cut := make([]aged, 0, len(w.seen))
	for path := range w.seen {
		if _, ok := inWalk[path]; ok {
			continue
		}
		cut = append(cut, aged{path: path, mtimeNanos: w.stamps[path].mtimeNanos})
	}
	// The paths still in the walk are spared, so the budget left for the rest
	// is the cap less however many the walk reached. inWalk is built from live
	// alone, so its length is that count.
	room := countedCap - len(inWalk)
	if room < 0 {
		room = 0
	}
	if len(cut) <= room {
		return
	}
	slices.SortFunc(cut, func(a, b aged) int { return cmp.Compare(a.mtimeNanos, b.mtimeNanos) })
	for _, a := range cut[:len(cut)-room] {
		w.releaseOffset(a.path)
	}
}

// stateCap bounds the per-file bookkeeping of transcripts a per-file-owner
// adapter judged to belong to this watcher but that no longer appear in the
// walk: the owner verdict, the stamp, the skip position and any zstd carry.
// A foreign verdict is not counted, since forgetIdle releases one the moment
// its file ages out; an own verdict is kept because losing it re-reads the
// whole transcript on its next appearance, and trimCounted's release of the
// counts is what hands the path here.
//
// Same value as countedCap, and the same release order: least recently
// written first. Without it a store that starts sessions faster than they age
// out leaves an entry per session written during the run, in each of those
// maps, for as long as the watcher lives: a machine-wide dsh watch walks every
// session on the host, so the key space is every session anyone has started.
//
// A released transcript is forgotten outright rather than cut loose with a
// seeded skip position, because the seeded position is the entry: holding it
// is what the cap is bounding. What that costs is the whole of a released
// session rather than the tail of it, should the session be appended to after
// coming back from this far out of the window. The oldest go first, the same
// order the counted-file cap uses, so the transcripts least likely to be
// appended to are the ones the cap spends that on.
const stateCap = countedCap

// trimOwned releases the oldest judged transcripts past stateCap, so the
// bookkeeping behind a verdict that outlived the recency window cannot grow by
// every session the agent has ever started.
func (w *Watcher) trimOwned(inWalk map[string]struct{}) {
	var cut []aged
	for path, mine := range w.owner {
		if !mine {
			continue
		}
		if _, ok := inWalk[path]; ok {
			continue
		}
		if _, counted := w.seen[path]; counted {
			continue
		}
		cut = append(cut, aged{path: path, mtimeNanos: w.stamps[path].mtimeNanos})
	}
	if len(cut) <= stateCap {
		return
	}
	slices.SortFunc(cut, func(a, b aged) int { return cmp.Compare(a.mtimeNanos, b.mtimeNanos) })
	for _, a := range cut[:len(cut)-stateCap] {
		w.dropFile(a.path)
	}
}

// releaseOffset cuts a transcript loose without forgetting the file: the skip
// position is seeded to the file's current end, so a later append is still
// read as growth rather than re-read from byte zero. What is lost is the
// remainder of a session that went idle, which the next reading is measured
// against. A file that was never stamped, or that cannot be stat'd any more,
// is dropped outright: seeding a guess would re-read a whole session and bill
// it twice.
func (w *Watcher) releaseOffset(path string) {
	if w.stamps[path].mtimeNanos == 0 {
		w.dropFile(path)
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		w.dropFile(path)
		return
	}
	w.offsets[path] = fi.Size()
	w.forgetCounts(path)
}

// forgetCounts drops what a transcript contributed to this attach without
// forgetting the file: the read position and its stamp are reset separately,
// by the caller that decided the bytes on disk are no longer the bytes it
// read. Cumulative baselines go too, so the next record after a rewrite
// re-baselines instead of being measured against a session that is gone.
func (w *Watcher) forgetCounts(path string) {
	delete(w.seen, path)
	delete(w.total, path)
	delete(w.base, path)
	delete(w.baseThink, path)
	delete(w.baseInput, path)
}

// dropFile also releases the zstd carry. A carry is the unterminated tail of a
// transcript whose bytes are already committed, so it is a per-file buffer the
// rest of the bookkeeping does not index: without this delete a long run kept
// one per aged-out transcript, each up to maxLineBytes, and a path that
// reappeared would have the stale tail prepended to a file that never wrote
// it.
//
// The read-failure latch goes the same way. Only a committed read clears it, so
// a transcript that failed once and then aged out kept its path for the life
// of the process, as does the attribution latch.
func (w *Watcher) dropFile(path string) {
	delete(w.stamps, path)
	delete(w.offsets, path)
	delete(w.owner, path)
	delete(w.preexisting, path)
	delete(w.zstdCarry, path)
	delete(w.readFailed, path)
	delete(w.ownsFailed, path)
	w.forgetCounts(path)
}
