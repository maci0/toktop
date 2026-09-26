// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// dshHeader is the opening record of a session log, where the working
// directory of the run is recorded.
func dshHeader(cwd string) string {
	return `{"type":"session","version":3,"id":"session-1","createdAt":1789581724239,"cwd":` +
		jsonPath(cwd) + `,"isSeeded":false,"delegationDepth":0,"agentPreset":"standard"}`
}

// dshMessage is a completed model call in the shape a v3 log writes: the
// provider's counts nested under data.usage, beside the assistant message.
func dshMessage(out, think, in int) string {
	return dshMessageUsage(out, think, in, 0, 0)
}

// dshMessageUsage adds the cache buckets. They are billed input, and
// totalTokens includes them along with the output.
func dshMessageUsage(out, think, in, cacheRead, cacheWrite int) string {
	total := in + cacheRead + cacheWrite + out
	return `{"type":"assistant/message","seq":18,"time":1789275997979,"data":{` +
		`"turn":1,"step":1,` +
		`"message":{"role":"assistant","content":[{"type":"text","text":"ok"}]},` +
		`"usage":{"inputTokens":` + strconv.Itoa(in) +
		`,"outputTokens":` + strconv.Itoa(out) +
		`,"totalTokens":` + strconv.Itoa(total) +
		`,"cacheReadTokens":` + strconv.Itoa(cacheRead) +
		`,"cacheWriteTokens":` + strconv.Itoa(cacheWrite) +
		`,"reasoningTokens":` + strconv.Itoa(think) +
		`}}}`
}

func dshUsageChunk(out, think, in int) string {
	return `{"type":"assistant/chunk","data":{"chunk":{"type":"usage","usage":{"inputTokens":` +
		strconv.Itoa(in) + `,"outputTokens":` + strconv.Itoa(out) + `,"reasoningTokens":` + strconv.Itoa(think) + `}}}}`
}

func zstdFrame(t testing.TB, plain string) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderCRC(true), zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	return enc.EncodeAll([]byte(plain), nil)
}

// skippableFrame is one RFC 8878 skippable frame: magic 0x184D2A5X, a
// 4-byte little-endian size, then payload.
func skippableFrame(nibble byte, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.LittleEndian.PutUint32(out, 0x184D2A50|uint32(nibble&0xF))
	binary.LittleEndian.PutUint32(out[4:], uint32(len(payload)))
	copy(out[8:], payload)
	return out
}

// skippableClaim is a skippable header whose size field may not match the
// bytes that follow, including claims past zstdMaxFrameBytes.
func skippableClaim(size uint32) []byte {
	out := make([]byte, 8)
	binary.LittleEndian.PutUint32(out, 0x184D2A50)
	binary.LittleEndian.PutUint32(out[4:], size)
	return out
}

func appendBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
}

func TestParseDshCountsAssistantMessageOnly(t *testing.T) {
	v, _, ok := parseDsh([]byte(dshMessage(260, 90, 1200)))
	if !ok {
		t.Fatal("assistant/message with usage was rejected")
	}
	if v.output != 260 || v.thinking != 90 || v.input != 1200 || v.total != 1460 {
		t.Fatalf("got %+v, want output 260 thinking 90 input 1200 total 1460", v)
	}

	if _, _, ok := parseDsh([]byte(dshUsageChunk(260, 90, 1200))); ok {
		t.Fatal("usage chunk must not count: it repeats the message")
	}

	// compaction/summary reports its own call, which dsh's durable usage fold
	// leaves out; counting it here would disagree with what dsh reports.
	summary := `{"type":"compaction/summary","seq":2652,"time":1,"data":{"usage":` +
		`{"inputTokens":751302,"outputTokens":4012,"totalTokens":786034,"cacheReadTokens":30720}}}`
	if _, _, ok := parseDsh([]byte(summary)); ok {
		t.Fatal("compaction summary must not count")
	}

	// Cache buckets are billed input, and totalTokens includes them.
	v, _, ok = parseDsh([]byte(dshMessageUsage(100, 10, 1000, 500, 300)))
	if !ok || v.input != 1800 || v.output != 100 || v.thinking != 10 || v.total != 1900 {
		t.Fatalf("cache buckets not folded into input: ok=%v %+v", ok, v)
	}

	legacy := `{"type":"assistant-message","usage":{"prompt_tokens":50,"completion_tokens":12,"reasoning_tokens":4}}`
	v, _, ok = parseDsh([]byte(legacy))
	if !ok || v.output != 12 || v.thinking != 4 || v.input != 50 || v.total != 62 {
		t.Fatalf("snake_case spelling lost: ok=%v %+v", ok, v)
	}
}

// TestParseDshV3RecordReadsNestedUsage drives one record with the exact shape
// of a dsh v3 session line: the counts sit under data.usage, the cache read is
// billed input, and totalTokens is the call's full total. Before this, only a
// top-level usage object was read, so every real dsh log reported nothing.
func TestParseDshV3RecordReadsNestedUsage(t *testing.T) {
	line := `{"type":"assistant/message","seq":18,"time":1789275997979,"data":{` +
		`"turn":1,"step":1,` +
		`"message":{"role":"assistant","content":[{"type":"reasoning","text":"x"},{"type":"text","text":"y"}],` +
		`"source":{"kind":"model","provider":"deepseek-official","model":"deepseek-flash"},` +
		`"id":"22b2b763-cb7d-451d-86ad-d883d84c04d2"},` +
		`"usage":{"inputTokens":1739,"outputTokens":100,"totalTokens":29743,` +
		`"cacheReadTokens":27904,"reasoningTokens":42},"stream":{"chunks":[]}}}`

	v, cwd, ok := parseDsh([]byte(line))
	if !ok {
		t.Fatal("real v3 assistant/message was rejected")
	}
	if cwd != "" {
		t.Fatalf("usage line reported cwd %q, want none (the header owns it)", cwd)
	}
	if v.input != 1739+27904 {
		t.Fatalf("input %d, want 29643 (uncached plus cache read)", v.input)
	}
	if v.output != 100 || v.thinking != 42 || v.total != 29743 {
		t.Fatalf("got %+v, want output 100 thinking 42 total 29743", v)
	}
}

func TestDshZstdSessionLog(t *testing.T) {
	store := withStore(t, "dsh")
	work := t.TempDir()
	path := filepath.Join(store, "session.jsonl.zstd")

	w := Watch("dsh", work, time.Now())
	if w == nil {
		t.Fatal("dsh should be supported")
	}

	header := zstdFrame(t, dshHeader(work)+"\n")
	first := zstdFrame(t, dshUsageChunk(110, 40, 800)+"\n"+dshMessage(110, 40, 800)+"\n")
	appendBytes(t, path, append(header, first...))
	w.poll(nil)
	if got := w.Sample().Output; got != 110 {
		t.Fatalf("output %d, want 110 (message only, not the chunk)", got)
	}
	if got := w.Sample().Thinking; got != 40 {
		t.Fatalf("thinking %d, want 40", got)
	}
	if got := w.Sample().Input; got != 800 {
		t.Fatalf("input %d, want 800", got)
	}

	second := zstdFrame(t, dshMessageUsage(150, 50, 200, 900, 0)+"\n")
	appendBytes(t, path, second)
	w.poll(nil)
	if got := w.Sample().Output; got != 260 {
		t.Fatalf("output %d, want 260 after second frame", got)
	}
	if got := w.Sample().Thinking; got != 90 {
		t.Fatalf("thinking %d, want 90 after second frame", got)
	}
	if got := w.Sample().Input; got != 1900 {
		t.Fatalf("input %d, want 1900 after the cached second call", got)
	}
	if got := w.Sample().Total; got != 1250 {
		t.Fatalf("total %d, want 1250: the largest call total, not a sum", got)
	}
}

func TestDshZstdIgnoresOtherProjects(t *testing.T) {
	store := withStore(t, "dsh")
	work, other := t.TempDir(), t.TempDir()
	w := Watch("dsh", work, time.Now())

	mine := filepath.Join(store, "mine", "session.jsonl.zstd")
	theirs := filepath.Join(store, "theirs", "session.jsonl.zstd")
	if err := os.MkdirAll(filepath.Dir(mine), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(theirs), 0o700); err != nil {
		t.Fatal(err)
	}
	appendBytes(t, mine, append(zstdFrame(t, dshHeader(work)+"\n"), zstdFrame(t, dshMessage(10, 0, 1)+"\n")...))
	appendBytes(t, theirs, append(zstdFrame(t, dshHeader(other)+"\n"), zstdFrame(t, dshMessage(9999, 0, 1)+"\n")...))
	w.poll(nil)
	if got := w.Sample().Output; got != 10 {
		t.Fatalf("another project's tokens leaked in: %d", got)
	}
}

func TestDshZstdIncompleteFrameIsReread(t *testing.T) {
	store := withStore(t, "dsh")
	work := t.TempDir()
	path := filepath.Join(store, "session.jsonl.zstd")
	w := Watch("dsh", work, time.Now())

	header := zstdFrame(t, dshHeader(work)+"\n")
	full := zstdFrame(t, dshMessage(77, 3, 10)+"\n")
	appendBytes(t, path, header)
	appendBytes(t, path, full[:len(full)/2])
	w.poll(nil)
	if got := w.Sample().Output; got != 0 {
		t.Fatalf("half frame counted as %d", got)
	}

	appendBytes(t, path, full[len(full)/2:])
	w.poll(nil)
	if got := w.Sample().Output; got != 77 {
		t.Fatalf("completed frame lost: %d", got)
	}
}

func TestDshPlainJSONLStillWorks(t *testing.T) {
	store := withStore(t, "dsh")
	work := t.TempDir()
	path := filepath.Join(store, "session.jsonl")
	w := Watch("dsh", work, time.Now())
	append_(t, path, dshHeader(work), dshMessage(40, 5, 9))
	w.poll(nil)
	if got := w.Sample().Output; got != 40 {
		t.Fatalf("plain jsonl output %d, want 40", got)
	}
}

func TestDshZstdHandlesCRLF(t *testing.T) {
	store := withStore(t, "dsh")
	work := t.TempDir()
	path := filepath.Join(store, "session.jsonl.zstd")
	w := Watch("dsh", work, time.Now())
	appendBytes(t, path, zstdFrame(t, dshHeader(work)+"\r\n"))
	appendBytes(t, path, zstdFrame(t, dshMessage(55, 10, 20)+"\r\n"))
	w.poll(nil)
	if got := w.Sample().Output; got != 55 {
		t.Fatalf("CRLF zstd output %d, want 55", got)
	}
	if got := w.Sample().Thinking; got != 10 {
		t.Fatalf("CRLF zstd thinking %d, want 10", got)
	}
}

func TestDshZstdTailCapContinuesNextPoll(t *testing.T) {
	store := withStore(t, "dsh")
	work := t.TempDir()
	path := filepath.Join(store, "session.jsonl.zstd")
	w := Watch("dsh", work, time.Now())

	header := zstdFrame(t, dshHeader(work)+"\n")
	first := zstdFrame(t, dshMessage(11, 0, 1)+"\n")
	second := zstdFrame(t, dshMessage(22, 0, 1)+"\n")
	appendBytes(t, path, header)
	appendBytes(t, path, first)
	appendBytes(t, path, second)

	old := zstdTailBytes
	// Big enough for the header and first message frame, too small for
	// the second: the leftover must be picked up on the next poll rather
	// than dropped or forcing a full-file buffer.
	zstdTailBytes = int64(len(header) + len(first) + len(second)/2)
	t.Cleanup(func() { zstdTailBytes = old })

	w.poll(nil)
	if got := w.Sample().Output; got != 11 {
		t.Fatalf("first poll output %d, want 11 (only the frames inside the cap)", got)
	}
	w.poll(nil)
	if got := w.Sample().Output; got != 33 {
		t.Fatalf("second poll output %d, want 33 after reading the leftover frame", got)
	}
}

func TestZstdFrameLenMatchesEncodeAll(t *testing.T) {
	a := zstdFrame(t, "one\n")
	b := zstdFrame(t, "two\n")
	src := append(append([]byte{}, a...), b...)
	n, ok := zstdFrameLen(src)
	if !ok || n != len(a) {
		t.Fatalf("first frame %d ok=%v, want %d", n, ok, len(a))
	}
	n2, ok := zstdFrameLen(src[n:])
	if !ok || n2 != len(b) {
		t.Fatalf("second frame %d ok=%v, want %d", n2, ok, len(b))
	}
	if _, ok := zstdFrameLen(src[:len(a)/2]); ok {
		t.Fatal("half frame must not look complete")
	}

	plain, consumed, err := decodeZstdPrefix(append(src, 0x28, 0xb5))
	if err != nil {
		t.Fatal(err)
	}
	if consumed != len(src) || string(plain) != "one\ntwo\n" {
		t.Fatalf("prefix decode consumed %d want %d, plain %q", consumed, len(src), plain)
	}
}

// FuzzDecodeZstdPrefix drives the concatenated-frame walker that tails
// dsh's default session.jsonl.zstd. Those files live in a writable agent
// store, so a hostile or torn frame must not panic, hang, or desync the
// reader: a complete-frame length stays inside the cap and the input, an
// incomplete tail is left unconsumed, a decode error stops on a frame
// boundary rather than skipping magic, and a successful prefix re-decodes
// to the same plaintext.
func FuzzDecodeZstdPrefix(f *testing.F) {
	header := zstdFrame(f, dshHeader("/tmp/work")+"\n")
	msg := zstdFrame(f, dshMessage(10, 2, 5)+"\n")
	concat := append(append([]byte{}, header...), msg...)
	emptySkip := skippableFrame(0, nil)
	skipThenMsg := append(skippableFrame(0xF, []byte("note")), msg...)

	for _, seed := range [][]byte{
		header,
		msg,
		concat,
		append(append([]byte{}, concat...), 0x28, 0xb5),
		header[:len(header)/2],
		emptySkip,
		skipThenMsg,
		skippableClaim(zstdMaxFrameBytes + 1),
		skippableClaim(0),
		skippableClaim(^uint32(0)),
		// Magic + descriptor + empty last raw block: complete, maybe
		// undecodable (tiny window), still a length the walker must bound.
		{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x00, 0x01, 0x00, 0x00},
		// Last reserved block type: must not look complete.
		{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x00, 0x07, 0x00, 0x00},
		// Last RLE block of four 'a's.
		{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x00, 0x23, 0x00, 0x00, 'a'},
		{0x28, 0xb5, 0x2f, 0xfd},
		{0x28, 0xb5},
		{0x1f, 0x8b},
		[]byte(`{"type":"session"}`),
		nil,
		{},
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, src []byte) {
		if n, ok := zstdFrameLen(src); ok {
			if n <= 0 || n > zstdMaxFrameBytes || n > len(src) {
				t.Fatalf("zstdFrameLen ok with n=%d len=%d", n, len(src))
			}
			n2, ok2 := zstdFrameLen(src[:n])
			if !ok2 || n2 != n {
				t.Fatalf("complete frame of %d bytes was not complete when sliced to itself", n)
			}
			if n > 0 {
				if _, ok3 := zstdFrameLen(src[:n-1]); ok3 {
					t.Fatalf("prefix of a %d-byte frame looked complete", n)
				}
			}
		}

		prefix := zstdCompletePrefix(src)
		if prefix < 0 || prefix > len(src) {
			t.Fatalf("complete prefix %d outside [0, %d]", prefix, len(src))
		}

		plain, consumed, err := decodeZstdPrefix(src)
		if consumed < 0 || consumed > len(src) {
			t.Fatalf("consumed %d outside [0, %d]", consumed, len(src))
		}
		if !zstdAtFrameBoundary(src, consumed) {
			t.Fatalf("consumed %d is not a frame boundary in %d-byte input", consumed, len(src))
		}
		if err == nil {
			if consumed != prefix {
				t.Fatalf("success consumed %d, complete prefix is %d", consumed, prefix)
			}
		} else if consumed >= prefix {
			t.Fatalf("decode error consumed %d, complete prefix is %d", consumed, prefix)
		}

		plain2, consumed2, err2 := decodeZstdPrefix(src)
		if consumed2 != consumed || (err2 == nil) != (err == nil) || !bytes.Equal(plain2, plain) {
			t.Fatal("decodeZstdPrefix is not deterministic")
		}

		if consumed > 0 {
			again, nAgain, errAgain := decodeZstdPrefix(src[:consumed])
			if errAgain != nil || nAgain != consumed || !bytes.Equal(again, plain) {
				t.Fatalf("complete prefix did not round-trip: n=%d err=%v", nAgain, errAgain)
			}
		}
	})
}

// zstdCompletePrefix is how far zstdFrameLen can walk from the front of
// src: every complete frame, stopping on a truncated or illegal one.
func zstdCompletePrefix(src []byte) int {
	off := 0
	for off < len(src) {
		n, ok := zstdFrameLen(src[off:])
		if !ok || n <= 0 {
			return off
		}
		off += n
	}
	return off
}

func zstdAtFrameBoundary(src []byte, off int) bool {
	if off < 0 || off > len(src) {
		return false
	}
	seen := 0
	for seen < off {
		n, ok := zstdFrameLen(src[seen:])
		if !ok || n <= 0 || seen+n > off {
			return false
		}
		seen += n
	}
	return seen == off
}
