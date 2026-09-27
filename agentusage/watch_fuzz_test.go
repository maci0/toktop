// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// transcript is one fuzz iteration's file on disk. consumeAppend takes a real
// *os.File, so the bytes are written once and each read gets its own handle:
// the function only seeks, never writes, and a reopened handle keeps a
// resumed read independent of the one before it.
type transcript struct {
	path string
	data []byte
}

func newTranscript(t *testing.T, dir string, data []byte) transcript {
	t.Helper()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return transcript{path: path, data: data}
}

// read runs consumeAppend from off, returning the records and the offset just
// past the last one it was willing to commit.
//
// consumeAppend is positional: it reads from wherever the handle sits and
// takes off only to seed its offset arithmetic, so the seek belongs here, the
// way both real callers do it. Without it the framer would re-read the file
// from the start and return the same records under a growing offset.
func (tr transcript) read(t *testing.T, w *Watcher, off int64) (recs []values, complete int64, ok bool) {
	t.Helper()
	f, err := os.Open(tr.path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	return w.consumeAppend(f, off)
}

// addTo folds one consumed record into a running total. A committed record
// can never read as negative, since the counters are summed and a saturating
// add turns an overflow into MaxInt rather than a wrap.
func addTo(dst, r values) values {
	dst.output = satAdd(dst.output, r.output)
	dst.input = satAdd(dst.input, r.input)
	dst.thinking = satAdd(dst.thinking, r.thinking)
	if r.total > dst.total {
		dst.total = r.total
	}
	return dst
}

func fold(recs []values) values {
	var out values
	for _, r := range recs {
		out = addTo(out, r)
	}
	return out
}

// FuzzConsumeAppend drives the transcript line framer with arbitrary bytes at
// an arbitrary resume offset. A transcript is appended to by another process
// and can stop mid-record, so the framer is where a torn write, a hostile
// length, or a bogus offset turns into lost or inflated counts: these are the
// numbers added to what the user is billed.
//
// The harness pairs the two sides of the read/commit boundary. Draining the
// file the way a poller does, resuming at each offset the previous read
// committed, must total exactly what one read of the whole file totals: a
// resume that skips the uncommitted tail, or counts it a second time, shows
// up as a mismatch. The committed offset is also checked to stay inside the
// file and land on a record boundary, so a later poll cannot start mid-line
// and re-interpret a fragment.
func FuzzConsumeAppend(f *testing.F) {
	seeds := [][]byte{
		nil,
		[]byte("\n"),
		[]byte(`{"output_tokens":3,"input_tokens":5,"thinking_tokens":2,"total_tokens":9}` + "\n"),
		// No trailing newline: a complete record whose writer has not flushed
		// the terminator yet.
		[]byte(`{"output_tokens":3,"input_tokens":5,"total_tokens":9}`),
		// A torn prefix of the record above, which must stay uncommitted and
		// be re-read whole on the next poll.
		[]byte(`{"output_tokens":3,"input`),
		// A record spanning several reader fills, so the framer has to
		// reassemble a line across ErrBufferFull rather than take it whole.
		// The over-cap discard path needs maxLineBytes of input per iteration
		// and has its own unit test.
		append(bytes.Repeat([]byte("x"), appendReaderBytes*3), []byte(`,"output_tokens":7,"total_tokens":9}`+"\n")...),
		// CRLF terminators and a BOM, both seen in the wild.
		[]byte("\xef\xbb\xbf" + `{"output_tokens":4,"total_tokens":4}` + "\r\n"),
		// Counts at the edge of int, which is what the saturating add is for.
		[]byte(`{"output_tokens":9223372036854775807,"input_tokens":9223372036854775807,"thinking_tokens":9223372036854775807,"total_tokens":9223372036854775807}` + "\n"),
		// A working directory that resolves nowhere, plus one that does.
		[]byte(`{"output_tokens":1,"cwd":"/nonexistent/deeply/nested"}` + "\n"),
		[]byte(`{"output_tokens":1,"cwd":"/"}` + "\n"),
		// Deeply nested JSON, to reach the walker's depth cap.
		[]byte(`{"a":{"b":{"c":{"d":{"e":{"f":{"g":{"h":{"i":{"output_tokens":5}}}}}}}}}}` + "\n"),
	}
	for _, s := range seeds {
		f.Add(byte(0), s)
	}
	f.Fuzz(func(t *testing.T, offSeed byte, data []byte) {
		// Spread the seed over every position, including inside a record and
		// past the end, so the resume path is not only ever tested at zero.
		off := int64(offSeed) * 64 % (int64(len(data)) + 1)

		w := &Watcher{
			dir: t.TempDir(),
			ad:  adapter{kind: perMessage, parse: parseGeneric, sessionCwd: genericSessionCwd},
		}
		tr := newTranscript(t, t.TempDir(), data)

		recs, complete, ok := tr.read(t, w, off)
		if !ok {
			if recs != nil || complete != 0 {
				t.Fatalf("failed read returned %d records at offset %d", len(recs), complete)
			}
			return
		}
		if complete < off || complete > int64(len(data)) {
			t.Fatalf("committed offset %d outside [%d, %d]", complete, off, len(data))
		}
		if complete > off && complete < int64(len(data)) && data[complete-1] != '\n' {
			t.Fatalf("committed offset %d does not land on a record boundary", complete)
		}
		for i, r := range recs {
			if r.output < 0 || r.input < 0 || r.thinking < 0 || r.total < 0 {
				t.Fatalf("record %d is negative: %+v", i, r)
			}
		}

		// The same bytes must read the same way twice.
		again, complete2, ok2 := tr.read(t, w, off)
		if ok2 != ok || complete2 != complete || fold(again) != fold(recs) {
			t.Fatalf("unstable read: %+v at %d, then %+v at %d", fold(recs), complete, fold(again), complete2)
		}

		// Drain the file the way a poller does, resuming at each committed
		// offset until it stops advancing. The tally the poller accumulates
		// across its reads must equal one read of the whole file: a resume
		// that skips the uncommitted tail, or counts it a second time, is a
		// wrong number in the sample.
		//
		// This starts at zero rather than at the fuzzed offset, since only a
		// boundary the framer itself chose is a legitimate place to resume.
		var (
			drained values
			pos     int64
		)
		// Each pass that advances commits at least one byte, so the file size
		// bounds the number of passes and the loop cannot wedge here.
		for range int64(len(data)) + 2 {
			pass, next, passOK := tr.read(t, w, pos)
			if !passOK {
				t.Fatalf("drain read at %d failed where a full read succeeded", pos)
			}
			for _, r := range pass {
				drained = addTo(drained, r)
			}
			if next == pos {
				break
			}
			pos = next
		}
		once, _, _ := tr.read(t, w, 0)
		if drained != fold(once) {
			t.Fatalf("draining from 0 totaled %+v, one read totaled %+v", drained, fold(once))
		}
	})
}

// FuzzOwnsHeaderScan drives the session-header lookup that decides whether a
// transcript belongs to this working directory. The verdict is what keeps one
// project's tokens out of another's total, so a false "mine" is a wrong
// number on screen and a false refusal is a session that silently stops
// reporting. The oracle is a plain scan of the whole file: if no line names a
// directory that resolves to this one, no bounded scan can have found one.
func FuzzOwnsHeaderScan(f *testing.F) {
	seeds := [][]byte{
		nil,
		[]byte("\n"),
		[]byte(`{"cwd":"/"}` + "\n"),
		[]byte(`{"type":"session_meta","payload":{"cwd":"/home/dev/proj"}}` + "\n"),
		// The header past the scan cap: the file is refused, not scanned on.
		append(bytes.Repeat([]byte("noise\n"), ownerScanLines), []byte(`{"cwd":"/"}`+"\n")...),
		// A line long enough to span several fills, header or not.
		append(bytes.Repeat([]byte("x"), appendReaderBytes*3), []byte("\n"+`{"cwd":"/"}`+"\n")...),
		[]byte("\xef\xbb\xbf" + `{"cwd":"/"}` + "\r\n"),
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		ad := adapter{
			kind:       perMessage,
			parse:      parseGeneric,
			sessionCwd: genericSessionCwd,
			roots:      func(string) []string { return []string{dir} },
		}
		tr := newTranscript(t, dir, data)

		mine, decided := newOwnerWatcher(dir, ad).owns(tr.path)
		if !mine && !namesDir(t, dir, ad, data) {
			return
		}
		if !namesDir(t, dir, ad, data) {
			t.Fatalf("claimed a transcript that names no directory of ours")
		}

		// The verdict is cached per path, so a second call must agree even
		// once the cache is warm.
		again, againDecided := newOwnerWatcher(dir, ad).owns(tr.path)
		if mine != again || decided != againDecided {
			t.Fatalf("unstable ownership verdict: %v/%v then %v/%v", mine, decided, again, againDecided)
		}
	})
}

// newOwnerWatcher mirrors what Watch sets up for owns: the adapter plus the
// verdict cache it writes into. A zero Watcher would panic on the nil map.
func newOwnerWatcher(dir string, ad adapter) *Watcher {
	return &Watcher{dir: dir, ad: ad, owner: map[string]bool{}}
}

// namesDir reports whether any line in data names a directory that resolves to
// dir, the fact owns can only ever be discovering.
func namesDir(t *testing.T, dir string, ad adapter, data []byte) bool {
	t.Helper()
	w := &Watcher{dir: dir, ad: ad}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if cwd, ok := ad.sessionCwd(bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))); ok && w.sameDir(cwd) {
			return true
		}
	}
	return false
}
