package remote

import (
	"errors"
	"strings"
	"testing"
)

// FuzzPeerOutputBuffers drives the two buffers a remote peer's bytes land in.
// The peer answers the exec channel, so both the content and the chunk
// boundaries are its choice: x/crypto/ssh copies in whatever sizes the wire
// delivers, and a peer that streams at line rate delivers them for as long as
// runTimeout allows.
//
// Both are hand-rolled ring arithmetic, and both were checked only against
// fixed write sizes, so the shapes that matter here are the ones a hand-
// written list does not reach: a write that straddles the cap by a byte, a
// write that is itself larger than the whole buffer, and the empty write that
// has to leave the retained tail exactly as it was.
//
// The oracle is the definition of what each buffer promises. stderrBuf keeps
// the last stderrBufferBytes of everything written, so its String is always
// the tail of the whole stream and never longer. stdoutBuf refuses past
// stdoutCap rather than truncating, because a caller that parsed the prefix
// anyway would report a short process table as the real one, so the collected
// length is capped, Overflowed is set exactly once the cap is passed, and the
// write that crosses it fails with errStdoutOverflow.
func FuzzPeerOutputBuffers(f *testing.F) {
	for _, seed := range []string{
		"",
		"a",
		"short line\n",
		strings.Repeat("x", stderrBufferBytes-1),
		strings.Repeat("x", stderrBufferBytes),
		strings.Repeat("x", stderrBufferBytes+1),
		strings.Repeat("x", 2*stderrBufferBytes),
		"\x1b[31mred\x1b[0m\n",
		"\xff\xfe invalid utf8",
		"\x00\x01\x7f",
		strings.Repeat("\n", 1000),
	} {
		f.Add(seed, 1, 1)
		f.Add(seed, 7, 3)
		f.Add(seed, stderrBufferBytes/2, stderrBufferBytes/2)
		f.Add(seed, 0, 0)
	}
	// A chunk wider than either cap, so the write that replaces the whole
	// buffer is reached from the first iteration.
	f.Add(strings.Repeat("y", stdoutCap+1), stdoutCap+1, 1)
	f.Add(strings.Repeat("y", stdoutCap), stdoutCap, 1)

	f.Fuzz(func(t *testing.T, chunk string, first, second int) {
		// Chunk sizes are wire-chosen, so they range over the whole positive
		// int; the negative half is not a shape a writer produces.
		if first < 0 {
			first = 0
		}
		if second < 0 {
			second = 0
		}
		// A stream of unbounded size is what the caps exist to stop, so the
		// per-iteration work is bounded instead: two writes plus a fixed
		// sweep, which is every ring transition the buffers have.
		const writes = 6

		var errBuf stderrBuf
		var outBuf stdoutBuf
		var stream strings.Builder
		total := 0

		feed := func(p []byte) {
			if n, err := errBuf.Write(p); n != len(p) || err != nil {
				t.Fatalf("stderrBuf.Write(%d bytes) = %d, %v; want %d, nil", len(p), n, err, len(p))
			}
			// A short write is legal for a writer that has stopped accepting,
			// so stdoutBuf is allowed to report fewer bytes than it was given.
			// What it may not do is keep them: the cap is the whole promise.
			_, err := outBuf.Write(p)
			if err != nil && !errors.Is(err, errStdoutOverflow) {
				t.Fatalf("stdoutBuf.Write = %v, want nil or errStdoutOverflow", err)
			}
			stream.Write(p)
			total += len(p)
		}

		size := first
		for i := range writes {
			if i%2 == 1 {
				size = second
			}
			feed([]byte(repeatChunk(chunk, size)))

			// Read the buffers back on every write: the state a peer's next
			// write lands on is the state under test, and reading it only at
			// the end would leave the intermediate transitions unchecked.
			all := stream.String()
			want := all
			if len(want) > stderrBufferBytes {
				want = want[len(want)-stderrBufferBytes:]
			}
			if got := errBuf.String(); got != want {
				t.Fatalf("after %d writes stderrBuf holds %d bytes, want the last %d of %d written",
					i+1, len(got), len(want), total)
			}
			if got := len(outBuf.String()); got > stdoutCap {
				t.Fatalf("after %d writes stdoutBuf holds %d bytes, over the %d cap", i+1, got, stdoutCap)
			}
		}

		// The overflow flag is set exactly when the stream has passed the cap,
		// never before and never by a write that still fits.
		if over := outBuf.Overflowed(); over != (total > stdoutCap) {
			t.Fatalf("Overflowed() = %v after %d bytes, want %v", over, total, total > stdoutCap)
		}
		// Past the cap the prefix survives so the overflow is diagnosable, and
		// it is the cap exactly: an over-long answer is never held in full.
		if want := min(total, stdoutCap); len(outBuf.String()) != want {
			t.Fatalf("stdoutBuf holds %d bytes after %d written, want %d", len(outBuf.String()), total, want)
		}
		// A stream that fits keeps every byte, so the collected prefix is the
		// whole answer rather than a truncated one that reads as complete.
		if total <= stdoutCap && outBuf.String() != stream.String() {
			t.Fatalf("stdoutBuf holds %q for a %d byte stream that fits", outBuf.String(), total)
		}
	})
}

// repeatChunk builds the n bytes one write carries, cycling the seed so
// successive writes of the same chunk are told apart in a failure message.
func repeatChunk(seed string, n int) string {
	if seed == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(n)
	for i := range n {
		b.WriteByte(seed[i%len(seed)])
	}
	return b.String()
}
