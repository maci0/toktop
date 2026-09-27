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
	pruneRootListsLocked(now, rescanEvery)
	if !force {
		if c, ok := rootLists[key]; ok && now.Sub(c.at) < rescanEvery {
			return append([]string(nil), c.files...)
		}
	}
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
	return w.walkCandidates(time.Now().Add(-recencyWindow), true)
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
func (w *Watcher) walkCandidates(cutoff time.Time, cache bool) []string {
	force := w.scanned.IsZero()
	if cache && !force && time.Since(w.scanned) < rescanEvery {
		return w.cached
	}
	var out []string
	for _, root := range w.ad.roots(w.dir) {
		if root == "" {
			continue
		}
		for _, suffix := range w.ad.fileSuffixes() {
			if cache {
				out = append(out, listTranscripts(root, suffix, cutoff, force)...)
				continue
			}
			out = append(out, walkTranscripts(root, suffix, cutoff)...)
		}
	}
	if !cache {
		return out
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

func (w *Watcher) dropFile(path string) {
	delete(w.stamps, path)
	delete(w.offsets, path)
	delete(w.owner, path)
	delete(w.preexisting, path)
	w.forgetCounts(path)
}
