// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// readNew consumes the bytes appended to one transcript since the last poll.
//
// Polling revisits every recent transcript on the caller's interval, which
// [Watcher.Run] defaults to [DefaultPollInterval] but does not floor (a
// shorter explicit interval is honoured, and [Watcher.Poll] re-walked the
// watcher's files on every call), and most of them are idle, so the mtime
// check happens on a plain stat: an untouched file costs one syscall instead
// of open+stat+close.
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
	// The counts cached for those bytes go with it. Rewinding the offset
	// alone would re-read the same records and add them on top of what was
	// already counted, billing a transcript's tokens twice.
	if fi.Size() < w.offsets[path] {
		w.forgetCounts(path)
		w.offsets[path] = 0
		delete(w.owner, path)
		// The carry is the unterminated tail of the bytes just abandoned, and
		// the rewind below reads the new version from byte zero. Left in place
		// it would be prepended to that version's first line, which then fails
		// to parse and is dropped with the offset already past it.
		delete(w.zstdCarry, path)
		// The bytes behind the pre-existing flag are the ones being abandoned:
		// it said "this file was on disk at attach, so its cumulative counters
		// began before this watch". The version now being read from byte zero is
		// a session that appeared after attach, so it counts in full. Left in
		// place, a rotated-in cumulative session takes its first reading as a
		// baseline and that reading is never reported.
		delete(w.preexisting, path)
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
	if w.ad.snapshot {
		w.readSnapshot(path, fi.Size(), stamp)
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
		recs, complete, ok = w.consumeZstd(path, f, off)
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
	if isDshZstd(path) && fi.Size()-off > zstdTailBytes.Load() {
		return
	}
	w.stamps[path] = stamp
}

// readSnapshot parses a transcript that is one document rewritten in place.
// The offset is the file's length after a successful read, so an unchanged
// file still short-circuits on its stamp; a rewrite is read from the start
// even when the new text is the same length as the old.
func (w *Watcher) readSnapshot(path string, size int64, stamp fileStamp) {
	f, err := w.openTranscript(path)
	if err != nil {
		return
	}
	defer f.Close()
	v, ok := w.snapshotValue(f)
	if !ok {
		return
	}
	w.applyRecord(path, v)
	w.offsets[path] = size
	w.stamps[path] = stamp
}

// snapshotValue reads one snapshot document from the start of f.
func (w *Watcher) snapshotValue(f *os.File) (values, bool) {
	if w.ad.parseFile == nil {
		return values{}, false
	}
	if _, err := f.Seek(0, 0); err != nil {
		return values{}, false
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(maxLineBytes)+1))
	if err != nil || len(data) > maxLineBytes {
		return values{}, false
	}
	v, cwd, ok := w.ad.parseFile(data)
	if !ok || (cwd != "" && !w.sameDir(cwd)) {
		return values{}, false
	}
	return v, true
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
	line = bytes.TrimPrefix(line, utf8BOM)
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
		cur.span += v.span
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
	if !w.ad.perFileOwner() {
		return true, true // decided per line instead
	}
	if mine, known := w.owner[path]; known {
		return mine, true
	}
	if w.ad.sessionCwdFile != nil {
		cwd, ok := w.ad.sessionCwdFile(path)
		if !ok {
			return false, false // transient: retry next poll
		}
		mine := w.sameDir(cwd)
		w.owner[path] = mine
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
	// maxLineBytes, not a smaller cap: a record the record path accepts must
	// not stop this scan. bufio.Scanner aborts on ErrTooLong rather than
	// skipping, so a header line between the two caps left the scan short of
	// ownerScanLines, returned "undecided", and blocked readNew on every poll
	// for the life of the session.
	sc.Buffer(make([]byte, 0, appendReaderBytes), maxLineBytes)
	lines := 0
	for lines < ownerScanLines && sc.Scan() {
		line := bytes.TrimPrefix(sc.Bytes(), utf8BOM)
		if cwd, ok := w.ad.sessionCwd(line); ok {
			mine := w.sameDir(cwd)
			w.owner[path] = mine
			return mine, true
		}
		lines++
	}
	if err := sc.Err(); err != nil {
		return false, false
	}
	// No cwd anywhere in what was written. A root that is already one
	// project's directory owns the file: dsh-native and cursor-agent logs
	// never grow a header. Anywhere else, a short file may still be flushing
	// its header, and a long one has none and must not be credited here.
	if w.ad.rootOwns && lines > 0 {
		w.owner[path] = true
		return true, true
	}
	if lines < ownerScanLines {
		return false, false
	}
	w.owner[path] = false
	return false, true
}

// dirVerdictMax bounds the sameDir memo. A watcher's own store names a
// handful of projects; a store spanning hundreds only costs one cleared map.
const dirVerdictMax = 256

func (w *Watcher) sameDir(cwd string) bool {
	if sameSpelling(cwd, w.dir) {
		return true
	}
	if mine, seen := w.dirVerdict[cwd]; seen {
		return mine
	}
	mine := false
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		mine = sameSpelling(resolved, w.dir)
	}
	// Lazily: a Watcher built by Watch carries the map, one assembled by a
	// caller or a test does not, and the verdict is worth caching either way.
	if w.dirVerdict == nil {
		w.dirVerdict = map[string]bool{}
	}
	if len(w.dirVerdict) >= dirVerdictMax {
		clear(w.dirVerdict)
	}
	w.dirVerdict[cwd] = mine
	return mine
}
