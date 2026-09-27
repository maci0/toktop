// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/logcfg"
)

// pollEvery is how often a transcript is re-read. It bounds how stale a live
// rate can be, so it is tighter than the directory rescan: reading one growing
// file is cheap, walking a store of thousands is not.
const pollEvery = 250 * time.Millisecond

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

// audit builds the logger for the lines this file writes. A var so a test can
// point it at a handler it can read.
var audit = logcfg.Logger

func rootListKey(root, suffix string) string { return root + "\x00" + suffix }

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

// listTranscripts returns recent files under root matching suffix. force
// bypasses the shared cache so a final Poll cannot miss a file created
// inside the last rescan window.
//
// now is the calling watcher's clock, and the cache is stamped with it rather
// than a wall-clock read: a watcher driven by an injected clock decides when
// its listings go stale, so two runs of the same seed return the same files
// however long each took. The cache is process-wide, so a caller that mixes
// clocks shares freshness with the others; mixing is a configuration error,
// not a supported mode.
//
// Concurrent watchers of the same root share one walk, and only that walk
// blocks: a store with thousands of files takes long enough that holding
// rootListMu across it stalled every watcher reading an unrelated root, and
// with one goroutine per agent process one slow store paused all of them. The
// walk therefore runs outside the map lock, with an in-flight channel as the
// per-root claim on it. A slower empty result still cannot overwrite a newer
// listing: only the goroutine holding the claim writes one.
func listTranscripts(root, suffix string, cutoff, now time.Time, force bool) []string {
	key := rootListKey(root, suffix)
	for {
		rootListMu.Lock()
		pruneRootListsLocked(now, rescanEvery)
		c := rootLists[key]
		if !force && c.walk == nil && !c.at.IsZero() && core.Age(now, c.at) < rescanEvery {
			out := append([]string(nil), c.files...)
			rootListMu.Unlock()
			return out
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
			select {
			case <-walk:
				continue
			case <-time.After(walkWait):
				return nil
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
			files []string
			fresh = now
		)
		defer func() {
			rootListMu.Lock()
			rootLists[key] = rootListing{files: files, at: fresh}
			close(done)
			rootListMu.Unlock()
		}()

		files, err := walkTranscripts(root, suffix, cutoff)
		if err != nil {
			// A walk that could not finish is not an empty store, and caching
			// its partial result under a fresh stamp would read as one: every
			// session under the subtree that failed would go unreported for a
			// whole rescan window, with nothing on screen to say why. The claim
			// is still released, but the entry is left unstamped so the next
			// caller re-walks rather than serving this one, and the reason is
			// audited because the only other symptom is agents reporting no
			// tokens.
			audit().Warn("toktop: agent transcript walk failed",
				"root", core.RedactHome(root),
				"error", core.Snippet([]byte(err.Error())))
			files, fresh = nil, time.Time{}
		}
		return append([]string(nil), files...)
	}
}

// walkTranscripts lists the transcripts under one root, reporting a failure to
// finish. A walk that stops partway (an unreadable subtree, a filesystem error)
// has seen some of the store and not the rest, which is a different answer from
// an empty one: the caller must not cache the partial list as a fresh listing.
func walkTranscripts(root, suffix string, cutoff time.Time) ([]string, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []string
	err = fs.WalkDir(r.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			// Returning nil would swallow a subtree that could not be read and
			// let the walk finish over the rest, which is the partial result
			// the caller must be told about. Skip just this entry: a single
			// vanished file is not a reason to abandon the store, and the walk
			// continues to the end either way.
			return nil
		}
		if rel == "." {
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
	if err != nil {
		return nil, err
	}
	return out, nil
}

// candidates lists transcript files recent enough to belong to this attach,
// reusing the previous walk for a few seconds. An empty result is cached too:
// until something matches, the store is walked once per rescanEvery rather
// than on every poll, and a session created in between surfaces when the
// window expires, the same bound a non-empty listing already works under.
func (w *Watcher) candidates() []string {
	return w.walkCandidates(w.clock()().Add(-recencyWindow), true)
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
	now := w.clock()()
	force := w.scanned.IsZero()
	if cache && !force && core.Age(now, w.scanned) < rescanEvery {
		return w.cached
	}
	var out []string
	for _, root := range w.rootsLocked() {
		if root == "" {
			continue
		}
		for _, suffix := range w.ad.fileSuffixes() {
			if cache {
				out = append(out, listTranscripts(root, suffix, cutoff, now, force)...)
				continue
			}
			files, err := walkTranscripts(root, suffix, cutoff)
			if err != nil {
				// The attach walk has no listing cache to leave unstamped, so it
				// is audited here and the read is abandoned: seeding offsets from
				// a store that could not be walked would record "read to the
				// end" for files the walk never reached, and the next append to
				// one of them would be skipped.
				audit().Warn("toktop: agent transcript walk failed",
					"root", core.RedactHome(root),
					"error", core.Snippet([]byte(err.Error())))
				return nil
			}
			out = append(out, files...)
		}
	}
	if !cache {
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
	for _, path := range drop {
		w.dropFile(path)
	}
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
func (w *Watcher) dropFile(path string) {
	delete(w.stamps, path)
	delete(w.offsets, path)
	delete(w.owner, path)
	delete(w.preexisting, path)
	delete(w.zstdCarry, path)
	w.forgetCounts(path)
}
