// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/maci0/toktop/internal/core"
)

// dsh's default session log is concatenated independent Zstandard frames
// (RFC 8878): a header frame, then one frame per durable append. The
// uncompressed spelling is still `.jsonl`. Both are JSONL of the same
// records; only the physical encoding differs.

const (
	dshZstdSuffix = ".jsonl.zstd"
	// dshHeaderBytes is enough for the opening header frame (one JSON
	// object). A larger prefix is fine as long as the header comes first:
	// the first frame naming a cwd decides ownership.
	dshHeaderBytes = 64 << 10
	// zstdBlockHeaderLen is the 3-byte Block_Header that precedes every
	// block (RFC 8878 §3.1.1.2). Header.HeaderSize does not include it.
	zstdBlockHeaderLen = 3
	// zstdChecksumLen is the optional 4-byte content checksum after the
	// last block.
	zstdChecksumLen = 4
	// zstdMaxFrameBytes rejects a claimed frame larger than this. A real
	// append batch is a few events; anything past the cap is hostility or
	// a desynced reader, and walking it would pin a huge buffer.
	zstdMaxFrameBytes = 8 << 20
	// zstdMaxDecodeMemory bounds decompressed output of one frame.
	zstdMaxDecodeMemory = 32 << 20
	// zstdMaxPlainBytes bounds the decompressed output of one decodeZstdPrefix
	// call across every frame in the window. zstdMaxDecodeMemory is applied by
	// the decoder per DecodeAll call, so a window of many small frames resets
	// it on each iteration: an RLE frame costs 10 compressed bytes and yields
	// 128 KiB, so 8 MiB of such frames would grow the accumulator to ~100 GiB
	// with every individual frame well inside the cap. The walk stops at the
	// cap instead, and the remaining frames are read on the next poll.
	zstdMaxPlainBytes = 32 << 20
	// zstdInitialPlainBytes is the accumulator's first capacity. A poll reads
	// one tail of at most zstdTailBytes compressed, which decodes to a small
	// fraction of a megabyte of events in practice; the reservation covers the
	// usual walk instead of a realloc per frame.
	zstdInitialPlainBytes = 64 << 10
)

// zstdTailBytes is the most newly-appended compressed bytes one poll will
// read. Complete frames inside the window are counted; a torn frame at the
// end is retried next poll. Without a cap, a session that grew by hundreds
// of megabytes between polls would pin that whole tail in one buffer.
//
// Atomic because every dsh watcher's goroutine reads it on every poll while a
// caller (or a test) lowering the cap to exercise a torn frame writes it.
var zstdTailBytes atomic.Int64

func init() { zstdTailBytes.Store(zstdMaxFrameBytes) }

// RFC 8878 block types in the 3-byte block header. Type 3 is reserved
// and rejected by the default arm in zstdFrameLen.
const (
	zstdBlockRaw        = 0
	zstdBlockRLE        = 1
	zstdBlockCompressed = 2
)

var (
	dshDecOnce sync.Once
	dshDec     *zstd.Decoder
	dshDecErr  error
)

func dshDecoder() (*zstd.Decoder, error) {
	dshDecOnce.Do(func() {
		dshDec, dshDecErr = zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(zstdMaxDecodeMemory),
		)
	})
	return dshDec, dshDecErr
}

func isDshZstd(path string) bool {
	return strings.HasSuffix(path, dshZstdSuffix)
}

// dshUsage is one model call's own counts. They are disjoint:
// inputTokens holds uncached input only, cached input arrives separately in
// cacheReadTokens/cacheWriteTokens, and totalTokens is the whole call. The
// snake_case fields are the older `assistant-message` spelling, still read
// for logs written by earlier builds.
type dshUsage struct {
	InputTokens      int `json:"inputTokens"`
	OutputTokens     int `json:"outputTokens"`
	TotalTokens      int `json:"totalTokens"`
	CacheReadTokens  int `json:"cacheReadTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`
	ReasoningTokens  int `json:"reasoningTokens"`

	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	ReasoningSnake   int `json:"reasoning_tokens"`
}

// parseDsh reads one session-log line. A v3 log nests the provider's counts
// under data.usage on the completed `assistant/message` record; earlier
// builds wrote them at the top level of an `assistant-message` record, which
// is read too.
//
// Only the completed message is counted. The streaming `assistant/chunk`
// whose chunk.type is usage repeats the same numbers, so counting it as well
// would double every turn. `compaction/summary` carries usage too, but the
// harness's own usage fold leaves it out of durable session totals, so
// counting it here would disagree with what dsh itself reports.
func parseDsh(line []byte) (values, string, bool) {
	line = bytes.TrimPrefix(line, utf8BOM)
	var rec struct {
		Type  string    `json:"type"`
		Usage *dshUsage `json:"usage"`
		Data  struct {
			Usage *dshUsage `json:"usage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(line, &rec); err != nil {
		return values{}, "", false
	}
	if rec.Type != "assistant/message" && rec.Type != "assistant-message" {
		return values{}, "", false
	}
	u := rec.Data.Usage
	if u == nil {
		u = rec.Usage
	}
	if u == nil {
		return values{}, "", false
	}
	out := counter(u.OutputTokens)
	if out == 0 {
		out = counter(u.CompletionTokens)
	}
	think := counter(u.ReasoningTokens)
	if think == 0 {
		think = counter(u.ReasoningSnake)
	}
	uncached := counter(u.InputTokens)
	if uncached == 0 {
		uncached = counter(u.PromptTokens)
	}
	// Billed prompt tokens: cached input was charged for too, the same fold
	// parseClaude applies.
	prompt := satAdd(uncached, satAdd(counter(u.CacheReadTokens), counter(u.CacheWriteTokens)))
	tot := counter(u.TotalTokens)
	if tot == 0 {
		tot = satAdd(prompt, out)
	}
	// A log carrying a total below the output it reports is as unsupported a
	// reading as one that omits it, so both are floored the way foldCounters
	// and the opencode sqlite reader floor theirs.
	tot = floorTotal(tot, out)
	v := values{output: out, thinking: think, total: tot, input: prompt}
	if !v.present() {
		return values{}, "", false
	}
	return v, "", true
}

// consumeZstd reads newly appended concatenated frames from off and counts
// the complete records among them. A torn last frame is left uncommitted, so
// the read is capped at zstdTailBytes and the leftover complete frames are
// picked up on the next poll.
//
// Frames are not records: one record can straddle the frame boundary this
// read stopped at, and the window's own end can cut one too. So the
// unterminated last segment is carried over (w.zstdCarry) and re-parsed with
// its head next poll, the way consumeAppend holds a trailing fragment. It
// counts only when it parses in full; committing the offset past a fragment
// that did not would drop those bytes permanently.
//
// A read or a first-frame decode that produced nothing is returned as the
// cause, not as a bare failure: the caller latches and reports it, and a nil
// cause would clear the latch instead, leaving an unreadable session looking
// like an idle one.
func (w *Watcher) consumeZstd(path string, f *os.File, off int64) (recs []values, complete int64, ok bool, rerr error) {
	src, err := io.ReadAll(io.LimitReader(f, zstdTailBytes.Load()))
	if err != nil {
		return nil, 0, false, fmt.Errorf("read zstd tail: %w", err)
	}
	plain, n, err := decodeZstdPrefix(src)
	if err != nil {
		if n == 0 {
			return nil, 0, false, fmt.Errorf("decode zstd frame at offset %d: %w", off, err)
		}
		// A complete frame failed to decode partway into the window. The
		// frames before it are good and the offset stops at the bad one, so
		// the walk stays in sync; what is lost is every record from here on,
		// on this poll and every poll after it, because the same frame is
		// read again. Nothing downstream can tell that from an idle session,
		// so the frame is named.
		auditLogger().Warn("agent transcript frame failed to decode",
			"path", redactStorePath(path),
			"offset", off+int64(n),
			"error", redactStorePath(core.Snippet([]byte(err.Error()))))
	}
	head := w.zstdCarry[path]
	w.zstdCarry[path] = nil
	first := true
	// One line of lookahead instead of a slice of them. The split is lazy, so
	// the lines cost nothing until they are parsed, and a window that
	// decompresses to megabytes of newlines (which any process able to write
	// the file can produce) never becomes millions of slice headers held at
	// once. A line is processed once the next one proves it terminated; the
	// last one is the unterminated tail.
	var pending []byte
	havePending := false
	take := func(line []byte, terminated bool) {
		line = bytes.TrimRight(line, "\r")
		if first && len(head) > 0 {
			line = append(head, line...)
		}
		first = false
		if terminated {
			if len(line) == 0 {
				return
			}
			// Same record cap the plain JSONL path applies (consumeAppend): a
			// frame can decompress to more than maxLineBytes, and the parser
			// rejects it either way, so drop it and keep reading.
			if len(line) > maxLineBytes {
				return
			}
			recs = w.collect(recs, line)
			return
		}
		// The unterminated tail. A complete record whose writer omitted the
		// final newline still parses, so it counts; anything else waits.
		switch {
		case len(line) == 0:
		case len(line) > maxLineBytes: // junk no parser accepts; drop rather than carry forever
		default:
			if v, cwd, parsed := w.ad.parse(line); parsed {
				recs = w.collectValue(recs, v, cwd)
			} else {
				w.zstdCarry[path] = bytes.Clone(line)
			}
		}
	}
	for line := range bytes.SplitSeq(plain, []byte("\n")) {
		if havePending {
			take(pending, true)
		}
		pending, havePending = line, true
	}
	if havePending {
		take(pending, false)
	}
	return recs, off + int64(n), true, nil
}

// ownsZstd decides ownership from the header frame. An incomplete opening
// frame is not a refusal: the writer may still be flushing it.
func (w *Watcher) ownsZstd(path string, f *os.File) (mine, decided bool) {
	buf := make([]byte, dshHeaderBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		w.auditOwns(path, err)
		return false, false
	}
	if n == 0 {
		// No frame has been flushed yet, so there is nothing to attribute.
		// This is the case the retry is for and it is not a failure to report.
		return false, false
	}
	plain, consumed, derr := decodeZstdPrefix(buf[:n])
	if derr != nil && consumed > 0 {
		// A session cannot be attributed to a directory when a frame in the
		// middle of the window will not decode. The header was still read, so
		// the answer is reported as not mine rather than as unknown, and the
		// frame is named so the reason is not invisible.
		auditLogger().Warn("agent transcript frame failed to decode",
			"path", redactStorePath(path),
			"offset", consumed,
			"error", redactStorePath(core.Snippet([]byte(derr.Error()))))
	}
	if consumed == 0 {
		if derr != nil {
			w.auditOwns(path, derr)
		}
		return false, false
	}
	for line := range bytes.SplitSeq(plain, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if cwd, ok := w.ad.sessionCwd(line); ok {
			mine := w.sameDir(cwd)
			w.owner[path] = mine
			return mine, true
		}
	}
	// The first complete frames had no cwd. A project-scoped root still owns
	// the file when the frames decoded: a native log has no header to wait
	// for. A frame that did not decode is not that case.
	if derr == nil && w.ad.rootOwns {
		w.owner[path] = true
		return true, true
	}
	w.owner[path] = false
	return false, true
}

// pathSlug is the single path component an agent uses for a working
// directory. Separators and ':' become '-', so the name is legal on Windows
// and does not introduce another directory. A component that already
// contains '-' is not recoverable from the slug; callers compare a path
// they have, they do not decode one.
func pathSlug(dir string) string {
	if dir == "" {
		return ""
	}
	// Both separators, on either OS: a Windows path written into a fixture
	// still has to become one component when the test runs on Linux, where
	// filepath.ToSlash leaves '\' alone.
	s := strings.ReplaceAll(filepath.Clean(dir), "\\", "/")
	s = strings.TrimPrefix(s, "/")
	s = strings.ReplaceAll(s, ":", "-")
	s = strings.ReplaceAll(s, "/", "-")
	// ".." is a working directory like any other, and a caller that joins the
	// slug onto a store path would walk out of it, so it is dropped with "."
	// rather than passed through as the one component that names a parent.
	if s == "" || s == "." || s == ".." {
		return ""
	}
	return s
}

// dshDirName is the session directory the harness creates for one working
// directory, under both ~/.dsh/sessions and ~/.dsh-native/sessions.
func dshDirName(dir string) string {
	slug := pathSlug(dir)
	if slug == "" {
		return ""
	}
	return "--" + slug + "--"
}

// dshRoots is the project directory inside each dsh store. The store holds
// every project, so the walk is the one directory this working directory
// names rather than the whole tree.
func dshRoots(dir string, _ time.Time) []string {
	var out []string
	for _, root := range []string{home(".dsh", "sessions"), home(".dsh-native", "sessions")} {
		if root == "" {
			continue
		}
		for _, spelling := range dirSpellings(dir) {
			name := dshDirName(spelling)
			if name == "" {
				continue
			}
			out = append(out, filepath.Join(root, name))
		}
	}
	return uniqueRoots(out)
}

// dshHostRoots is both session stores, not one project's directory. dsh web
// writes into whichever project it was asked about; the server process's
// own cwd is the harness, which is not one of those projects.
func dshHostRoots(string, time.Time) []string {
	var out []string
	for _, root := range []string{home(".dsh", "sessions"), home(".dsh-native", "sessions")} {
		if root != "" {
			out = append(out, root)
		}
	}
	return out
}

// decodeZstdPrefix decompresses every complete frame at the front of src
// and reports how many compressed bytes those frames occupied. An
// incomplete tail is left unconsumed. A complete frame that fails to
// decode stops the walk: skipping it would desync the reader from the
// next magic.
func decodeZstdPrefix(src []byte) (plain []byte, consumed int, err error) {
	dec, err := dshDecoder()
	if err != nil {
		return nil, 0, err
	}
	// One reservation up front. Without it every frame reallocates and copies
	// the accumulator so far, so a window of N small frames costs O(N^2)
	// bytes moved. A window larger than zstdInitialPlainBytes still grows the
	// slice, but geometrically rather than once per frame.
	plain = make([]byte, 0, zstdInitialPlainBytes)
	off := 0
	for off < len(src) && len(plain) < zstdMaxPlainBytes {
		n, ok := zstdFrameLen(src[off:])
		if !ok {
			break
		}
		out, derr := dec.DecodeAll(src[off:off+n], nil)
		if derr != nil {
			return plain, off, derr
		}
		// The cap is checked per frame, not once per loop: a frame is allowed
		// to decode to zstdMaxDecodeMemory, so testing len(plain) alone let one
		// frame carry the accumulator a full frame past the budget. What does
		// not fit stays unconsumed and is read on the next poll.
		if len(plain)+len(out) > zstdMaxPlainBytes {
			break
		}
		plain = append(plain, out...)
		off += n
	}
	if len(plain) == 0 {
		return nil, off, nil
	}
	return plain, off, nil
}

// zstdFrameLen is the compressed size of the first complete frame in src,
// or not-ok when the frame is truncated, too large, or not zstd.
func zstdFrameLen(src []byte) (int, bool) {
	var h zstd.Header
	if err := h.Decode(src); err != nil {
		return 0, false
	}
	if h.Skippable {
		n := h.HeaderSize + int(h.SkippableSize)
		if n < h.HeaderSize || n > zstdMaxFrameBytes || len(src) < n {
			return 0, false
		}
		return n, true
	}
	off := h.HeaderSize
	for {
		if off+zstdBlockHeaderLen > len(src) {
			return 0, false
		}
		bh := uint32(src[off]) | uint32(src[off+1])<<8 | uint32(src[off+2])<<16
		last := bh&1 != 0
		btype := (bh >> 1) & 3
		size := int(bh >> 3)
		off += zstdBlockHeaderLen
		switch btype {
		case zstdBlockRLE:
			off++
		case zstdBlockRaw, zstdBlockCompressed:
			off += size
		default:
			return 0, false
		}
		if off < h.HeaderSize || off > zstdMaxFrameBytes {
			return 0, false
		}
		if last {
			break
		}
	}
	if h.HasCheckSum {
		off += zstdChecksumLen
	}
	if off > zstdMaxFrameBytes || len(src) < off {
		return 0, false
	}
	return off, true
}
