// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// dsh's default session log is concatenated independent Zstandard frames
// (RFC 8878): a header frame, then one frame per durable append. The
// uncompressed spelling is still `.jsonl`. Both are JSONL of the same
// records; only the physical encoding differs.

const (
	dshZstdSuffix = ".jsonl.zstd"
	// dshHeaderBytes is enough for the opening header frame (one JSON
	// object). A larger prefix is fine: extra complete frames are ignored
	// when deciding ownership.
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
)

// zstdTailBytes is the most newly-appended compressed bytes one poll will
// read. Complete frames inside the window are counted; a torn frame at the
// end is retried next poll. Without a cap, a session that grew by hundreds
// of megabytes between polls would pin that whole tail in one buffer.
var zstdTailBytes int64 = zstdMaxFrameBytes

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
	v := values{output: out, thinking: think, total: tot, input: prompt}
	if !v.present() {
		return values{}, "", false
	}
	return v, "", true
}

// consumeZstd reads newly appended concatenated frames from off, counts
// complete ones, and leaves a torn last frame uncommitted so the next poll
// re-reads it whole. The read is capped at zstdTailBytes so a large append
// batch cannot pin an unbounded buffer; leftover complete frames are
// picked up on the next poll.
func (w *Watcher) consumeZstd(f *os.File, off int64) (recs []values, complete int64, ok bool) {
	src, err := io.ReadAll(io.LimitReader(f, zstdTailBytes))
	if err != nil {
		return nil, 0, false
	}
	plain, n, err := decodeZstdPrefix(src)
	if err != nil && n == 0 {
		return nil, 0, false
	}
	for line := range bytes.SplitSeq(plain, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		recs = w.collect(recs, line)
	}
	return recs, off + int64(n), true
}

// ownsZstd decides ownership from the header frame. An incomplete opening
// frame is not a refusal: the writer may still be flushing it.
func (w *Watcher) ownsZstd(path string, f *os.File) (mine, decided bool) {
	buf := make([]byte, dshHeaderBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return false, false
	}
	if n == 0 {
		return false, false
	}
	plain, consumed, _ := decodeZstdPrefix(buf[:n])
	if consumed == 0 {
		return false, false
	}
	for line := range bytes.SplitSeq(plain, []byte("\n")) {
		if cwd, ok := w.ad.sessionCwd(line); ok {
			mine := w.sameDir(cwd)
			w.owner[path] = mine
			return mine, true
		}
	}
	// The first complete frames had no cwd. That is the header; waiting
	// longer will not invent one.
	w.owner[path] = false
	return false, true
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
	off := 0
	for off < len(src) {
		n, ok := zstdFrameLen(src[off:])
		if !ok {
			break
		}
		out, derr := dec.DecodeAll(src[off:off+n], nil)
		if derr != nil {
			return plain, off, derr
		}
		plain = append(plain, out...)
		off += n
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
