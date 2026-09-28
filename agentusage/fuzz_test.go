// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// FuzzParseAgentLines drives every per-agent transcript line parser
// (parseClaude, parseQwen, parseCodex, parseDsh, parseKimi, parseGemini,
// parseAgy, parseGrokUpdate and the session-cwd readers) with arbitrary bytes.
// Transcripts are files on disk whose records embed whatever
// the model and its tools ingested, so a corrupted or hostile line must not be
// able to poison a reading: no counter is ever negative (they are summed)
// straight into displayed totals), an accepted line always carries numbers the
// envelope-agnostic walker also sees, a cwd reader never accepts an empty
// directory, and every parser answers identical input identically twice.
func FuzzParseAgentLines(f *testing.F) {
	seeds := []string{
		`{"type":"assistant","cwd":"/home/dev/proj","message":{"usage":{"input_tokens":900,"output_tokens":120,"cache_read_input_tokens":10,"cache_creation_input_tokens":5,"output_tokens_details":{"thinking_tokens":40}}}}`,
		`{"type":"assistant","usageMetadata":{"candidatesTokenCount":17,"thoughtsTokenCount":9,"totalTokenCount":76},"cwd":"/tmp"}`,
		`{"type":"assistant","payload":{"type":"token_count","info":{"total_token_usage":{"output_tokens":310,"reasoning_output_tokens":22,"total_tokens":9000}}}}`,
		`{"type":"assistant/message","usage":{"inputTokens":9245,"outputTokens":276,"reasoningTokens":144,"cacheReadTokens":0}}`,
		`{"type":"assistant/chunk","data":{"chunk":{"type":"usage","usage":{"inputTokens":10,"outputTokens":4,"reasoningTokens":2}}}}`,
		`{"type":"usage.record","usage":{"inputOther":900,"inputCacheRead":100,"inputCacheCreation":5,"output":120}}`,
		`{"type":"usage.record","usage":{"inputOther":-100,"inputCacheRead":-3,"output":-50}}`,
		`{"type":"token_count.measured","contextSize":32768}`,
		`{"type":"usage.record","usage":{"inputOther":9223372036854775807,"output":9223372036854775807}}`,
		`{"type":"usage.record","usage":"many","inputOther":1.5,"output":[1]}`,
		`{"type":"session_meta","payload":{"cwd":"/home/dev"}}`,
		`{"type":"assistant","message":{"usage":{"input_tokens":-100,"output_tokens":-50,"cache_read_input_tokens":-3,"thinking_tokens":-9}}}`,
		`{"type":"assistant","message":{"usage":{"input_tokens":9223372036854775807,"output_tokens":9223372036854775807,"cache_read_input_tokens":9223372036854775807,"cache_creation_input_tokens":9223372036854775807}}}`,
		`{"type":"assistant","usageMetadata":{"candidatesTokenCount":-17,"totalTokenCount":-76}}`,
		`{"type":"assistant","payload":{"type":"token_count","info":{"total_token_usage":{"output_tokens":-310,"reasoning_output_tokens":-22,"total_tokens":-9000}}}}`,
		`{"type":"assistant","message":{"usage":{"output_tokens":1.5,"input_tokens":[1]}}}`,
		`{"type":"assistant","message":{"usage":"many"},"cwd":["/"]}`,
		`{"type":"result","cwd":"/somewhere/else","payload":{"cwd":null}}`,
		`{"type":"x","payload":{"type":"token_count"}}`,
		`{"type":"tool_result","content":` + strings.Repeat(`{"usage":`, 12) + `{}` + strings.Repeat(`}`, 12) + `}`,
		"{not json", "", "[1]", "null", `{"a":`,

		// Gemini and agy share one record shape.
		`{"cwd":"/home/dev/proj","tokens":{"input":900,"output":120,"cached":10,"thoughts":40,"total":1060}}`,
		`{"workspace":"/tmp/p","tokens":{"input":100,"output":0,"cached":0,"thoughts":0,"total":100}}`,
		`{"usageMetadata":{"promptTokenCount":80,"candidatesTokenCount":12,"thoughtsTokenCount":5,"cachedContentTokenCount":3,"totalTokenCount":97}}`,
		`{"cwd":"/w","tokens":{"input":100,"output":0,"total":100},"toolCalls":[{"name":"read"}]}`,
		`{"cwd":"/w","tokens":{"input":100,"output":0,"total":100},"toolCalls":[]}`,
		`{"cwd":"/w","tokens":{"input":100,"output":0,"total":100},"toolCalls":null}`,
		`{"cwd":"/w","tokens":{"input":100,"output":0,"total":100},"toolCalls":{}}`,
		`{"tokens":{"input":-1,"output":-1,"cached":-1,"thoughts":-1,"total":-1}}`,
		`{"tokens":{"input":9223372036854775807,"output":9223372036854775807,"cached":9223372036854775807,"thoughts":9223372036854775807,"total":9223372036854775807}}`,
		`{"tokens":"many","cwd":[1]}`,

		// Grok's updates.jsonl turn_completed record.
		`{"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"turn_completed","elapsed_ms":5000,"usage":{"inputTokens":900,"outputTokens":120,"cachedReadTokens":10,"cacheCreationTokens":5,"reasoningTokens":40,"totalTokens":1075,"apiDurationMs":3000}}}}`,
		`{"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"tool_call","usage":{"inputTokens":900,"outputTokens":120}}}}`,
		`{"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"turn_completed","elapsed_ms":5000,"usage":{"inputTokens":-9,"outputTokens":-9,"totalTokens":-9,"apiDurationMs":-9}}}}`,
		`{"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"turn_completed","elapsed_ms":9223372036854775807,"usage":{"inputTokens":1,"outputTokens":1,"apiDurationMs":9223372036854775807}}}}`,
		`{"method":"_x.ai/session/update","params":{"update":"x","usage":1}}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		assertUsage := func(which string, v values, ok bool) {
			t.Helper()
			if !ok {
				return
			}
			if v.output < 0 || v.thinking < 0 || v.total < 0 || v.input < 0 {
				t.Fatalf("%s: negative counter for %q: %+v", which, line, v)
			}
			if _, _, genOK := parseGeneric(line); !genOK {
				t.Fatalf("%s: accepted %q but the generic walker sees no usage", which, line)
			}
		}

		v, cwd, ok := parseClaude(line)
		assertUsage("claude", v, ok)
		if v2, cwd2, ok2 := parseClaude(line); ok2 != ok || v2 != v || cwd2 != cwd {
			t.Fatalf("parseClaude not deterministic for %q", line)
		}

		q, qcwd, qok := parseQwen(line)
		assertUsage("qwen", q, qok)
		if q2, qcwd2, qok2 := parseQwen(line); qok2 != qok || q2 != q || qcwd2 != qcwd {
			t.Fatalf("parseQwen not deterministic for %q", line)
		}

		c, _, cok := parseCodex(line)
		assertUsage("codex", c, cok)
		if c2, _, cok2 := parseCodex(line); cok2 != cok || c2 != c {
			t.Fatalf("parseCodex not deterministic for %q", line)
		}

		if s, ok := codexSessionCwd(line); ok && s == "" {
			t.Fatalf("codexSessionCwd: accepted empty cwd for %q", line)
		}
		if s, ok := genericSessionCwd(line); ok && s == "" {
			t.Fatalf("genericSessionCwd: accepted empty cwd for %q", line)
		}

		d, _, dok := parseDsh(line)
		assertUsage("dsh", d, dok)
		if d2, _, dok2 := parseDsh(line); dok2 != dok || d2 != d {
			t.Fatalf("parseDsh not deterministic for %q", line)
		}

		k, _, kok := parseKimi(line)
		assertUsage("kimi", k, kok)
		if k2, _, kok2 := parseKimi(line); kok2 != kok || k2 != k {
			t.Fatalf("parseKimi not deterministic for %q", line)
		}

		g, gcwd, gok := parseGemini(line)
		assertUsage("gemini", g, gok)
		if g2, gcwd2, gok2 := parseGemini(line); gok2 != gok || g2 != g || gcwd2 != gcwd {
			t.Fatalf("parseGemini not deterministic for %q", line)
		}

		// agy writes the Gemini record shape from its own store, reached
		// through a different adapter, so it is a separate entry point.
		y, ycwd, yok := parseAgy(line)
		assertUsage("agy", y, yok)
		if y2, ycwd2, yok2 := parseAgy(line); yok2 != yok || y2 != y || ycwd2 != ycwd {
			t.Fatalf("parseAgy not deterministic for %q", line)
		}

		gr, grokcwd, grokOK := parseGrokUpdate(line)
		assertUsage("grok", gr, grokOK)
		if gr2, grcwd2, grOK2 := parseGrokUpdate(line); grOK2 != grokOK || gr2 != gr || grcwd2 != grokcwd {
			t.Fatalf("parseGrokUpdate not deterministic for %q", line)
		}
		if gr.span < 0 {
			t.Fatalf("grok: negative turn span for %q: %+v", line, gr)
		}

		assertToolCallsRepeat(t, line)
		assertGrokMethod(t, line)
	})
}

// assertToolCallsRepeat pins Gemini's dedup: the CLI writes a turn's counters
// a second time once its tool calls finish, and that second copy carries a
// toolCalls array. Counting it would bill the turn twice, so a record naming
// tool calls is never a reading, however many counters it holds.
func assertToolCallsRepeat(t *testing.T, line []byte) {
	t.Helper()
	raw := rawField(t, line, "toolCalls")
	if len(raw) == 0 || raw[0] != '[' || bytes.Equal(raw, []byte("[]")) {
		return
	}
	if _, _, ok := parseGemini(line); ok {
		t.Fatalf("gemini: billed a turn twice from a record naming tool calls: %q", line)
	}
}

// assertGrokMethod pins that only a real session update is read: the same
// words turn up inside tool results in the same log.
func assertGrokMethod(t *testing.T, line []byte) {
	t.Helper()
	raw := rawField(t, line, "method")
	if bytes.Equal(raw, []byte(`"_x.ai/session/update"`)) {
		return
	}
	if _, _, ok := parseGrokUpdate(line); ok {
		t.Fatalf("grok: read a line whose method is %s: %q", raw, line)
	}
}

// rawField returns one top-level field of a record, empty when the line is not
// a JSON object or carries no such field. The marker checks only fire on a
// field the record actually spelled out.
func rawField(t *testing.T, line []byte, key string) []byte {
	t.Helper()
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(line, utf8BOM)), &probe); err != nil {
		return nil
	}
	return bytes.TrimSpace(probe[key])
}
