// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// FuzzConsumeZstdRecordWalk drives the record walk on top of dsh's
// concatenated frames, which FuzzDecodeZstdPrefix only reaches as far as the
// frame boundary. The frames here carry the fuzzer's bytes, split across frame
// boundaries at arbitrary offsets, so a record straddles two frames and has to
// survive the carry the next poll prepends to the window's first line: a carry
// dropped, prepended twice, or an offset committed past a record that was only
// half read turns into tokens that are lost or billed twice.
//
// The oracle is the two sides of that read/commit boundary. Draining the file
// the way a poller does, one capped window at a time and resuming at each
// committed offset, must total exactly what one uncapped read of the whole
// file totals. A window that skips the uncommitted tail, or counts it a second
// time, shows up as a mismatch. The committed offset is also checked to stay
// inside the file and to land on a frame boundary, so the next poll cannot
// resume inside a frame and re-read its bytes under a fresh offset.
func FuzzConsumeZstdRecordWalk(f *testing.F) {
	header := dshHeader("/tmp/work") + "\n"
	msg := dshMessage(10, 2, 5) + "\n"
	// One record split across two frames, which is what the carry exists for.
	cut := len(msg) / 2
	seeds := [][]byte{
		nil,
		{},
		[]byte("\n"),
		[]byte(msg),
		[]byte(header + msg),
		[]byte(msg[:cut] + msg[cut:]),
		// A header alone owns nothing, so every later frame's counts are
		// dropped: the walk must still commit the frames it decoded.
		[]byte(header),
		// Counts at the edge of int, which is what the saturating add is for.
		[]byte(dshMessageUsage(1<<40, 1<<40, 1<<40, 1<<40, 1<<40) + "\n"),
		// Not JSON at all, and a line longer than the record cap.
		[]byte("not json\n\x00\xff\n" + string(make([]byte, 0))),
		// A usage chunk repeats the message it follows and must not count.
		[]byte(dshUsageChunk(260, 90, 1200) + "\n"),
	}
	for _, s := range seeds {
		f.Add(byte(0), s)
	}
	// Arbitrary bytes as frames: the header walk and the decoder both have to
	// refuse them without losing the reader's place.
	f.Add(byte(1), []byte{0x28, 0xb5, 0x2f, 0xfd})
	f.Add(byte(1), []byte(`{"type":"assistant/message","data":`))
	f.Add(byte(3), append(append([]byte{}, msg...), 0x28, 0xb5))

	f.Fuzz(func(t *testing.T, mode byte, data []byte) {
		// An unframed stream puts the fuzzer's bytes in front of the frame
		// walker itself, where a hostile header, a reserved block type or a
		// truncated tail is the input. A framed stream puts the same bytes
		// inside real frames, which is what the record walk above has to
		// split, carry and commit.
		var (
			stream []byte
			cut    int
		)
		frames := 1 + int(mode>>2)%4
		if mode&1 == 0 {
			// Split at arbitrary offsets, so records straddle frame
			// boundaries rather than lining up with them.
			for i := range frames {
				end := len(data) * (i + 1) / frames
				stream = append(stream, zstdFrame(t, string(data[cut:end]))...)
				cut = end
			}
		} else {
			stream = append(stream, data...)
		}
		// A torn trailing frame is the common real case: the writer was
		// interrupted mid-append, so the last frame is short.
		if mode&2 != 0 && len(stream) > 1 {
			stream = stream[:len(stream)-1]
		}

		path := filepath.Join(t.TempDir(), "session.jsonl.zstd")
		if err := os.WriteFile(path, stream, 0o644); err != nil {
			t.Fatal(err)
		}
		// The walk audits a frame it could not decode, which is expected here.
		// Silence it so a failing run reports the failure, not the log.
		restore := swapAudit(slog.New(slog.NewTextHandler(io.Discard, nil)))
		t.Cleanup(restore)

		// The uncapped read is the oracle, so it runs with a window that
		// cannot truncate the file.
		old := zstdTailBytes.Load()
		zstdTailBytes.Store(1 << 30)
		want := fold(mustReadZstd(t, path, 0))
		// The poller's own window, sized to the widest frame in the stream so
		// every window holds at least one whole frame. A window narrower than
		// the frames it has to read would never advance the offset, which is
		// the cap doing its job and not the walk making progress.
		zstdTailBytes.Store(int64(widestFrame(stream)))
		t.Cleanup(func() { zstdTailBytes.Store(old) })

		// Drain the way a poller does: resume at each committed offset until
		// one window commits nothing more.
		w := newDshWatcher(t)
		var (
			got  values
			pos  int64
			pass int
		)
		for range int64(len(stream)) + 2 {
			recs, next, ok, _ := w.readZstd(t, path, pos)
			if !ok {
				break
			}
			for _, r := range recs {
				got = addTo(got, r)
			}
			if next < pos || next > int64(len(stream)) {
				t.Fatalf("committed offset %d outside [%d, %d]", next, pos, len(stream))
			}
			if next == pos {
				break
			}
			// A commit ends on a frame boundary: resuming inside one would
			// re-read bytes the committed offset already accounts for, and
			// the carry would then hold a record whose head was counted.
			if !frameBoundaries(stream)[next] {
				t.Fatalf("committed offset %d lands inside a frame", next)
			}
			pos = next
			pass++
		}
		// A stream with no complete frame has nothing to walk, so a drain that
		// never advances is the right answer for it.
		if pass == 0 && widestFrame(stream) > 0 {
			t.Fatalf("no window ever advanced past offset 0 over %d bytes", len(stream))
		}
		if got != want {
			t.Fatalf("drained %+v, one uncapped read %+v", got, want)
		}
		// A carry larger than the record cap would pin memory for the life of
		// the run and be prepended to a record no parser accepts.
		for _, carry := range w.zstdCarry {
			if len(carry) > maxLineBytes {
				t.Fatalf("carry of %d bytes exceeds the record cap", len(carry))
			}
		}
		// The same bytes must walk the same way twice.
		if again := fold(mustReadZstd(t, path, 0)); again != want {
			t.Fatalf("unstable read: %+v then %+v", want, again)
		}
	})
}

// widestFrame is the size of the largest complete frame in src, or 0 when it
// holds none. A stream the frame walker cannot read is one no window can make
// progress on, so the harness needs to tell that case from a stalled drain.
func widestFrame(src []byte) int {
	widest := int64(0)
	for off := range frameBoundaries(src) {
		if off > widest {
			widest = off
		}
	}
	return int(widest)
}

// frameBoundaries is every offset in src a frame ends at, zero included. A
// committed offset has to be one of them: the walk consumes whole frames, so
// an offset inside one would be resumed from bytes already accounted for.
func frameBoundaries(src []byte) map[int64]bool {
	out := map[int64]bool{0: true}
	for off := 0; off < len(src); {
		n, ok := zstdFrameLen(src[off:])
		if !ok {
			break
		}
		off += n
		out[int64(off)] = true
	}
	return out
}

// mustReadZstd reads one window and fails the run if the read itself failed,
// which is the case the harness above compares against.
func mustReadZstd(t *testing.T, path string, off int64) []values {
	t.Helper()
	recs, _, ok, _ := newDshWatcher(t).readZstd(t, path, off)
	if !ok {
		t.Fatalf("uncapped read at %d failed", off)
	}
	return recs
}

// newDshWatcher is the watcher a dsh watcher's read path works with: the dsh
// adapter, the carry map consumeZstd writes, and the verdict memo owns needs.
func newDshWatcher(t *testing.T) *Watcher {
	t.Helper()
	return &Watcher{
		dir:        t.TempDir(),
		ad:         adapter{kind: perMessage, parse: parseDsh, sessionCwd: genericSessionCwd},
		zstdCarry:  map[string][]byte{},
		owner:      map[string]bool{},
		dirVerdict: map[string]dirVerdict{},
	}
}

// readZstd reads one window of path from off, the way readNew does: seek to
// the committed offset, then let consumeZstd take the window. consumeZstd
// reads from wherever the handle sits, so the seek belongs here.
func (w *Watcher) readZstd(t *testing.T, path string, off int64) (recs []values, complete int64, ok bool, rerr error) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	return w.consumeZstd(path, f, off)
}
