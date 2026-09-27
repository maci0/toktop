// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"strings"
	"testing"
)

// FuzzParseAgentLines drives every per-agent transcript line parser
// (parseClaude, parseQwen, parseCodex, parseDsh, parseKimi and the two
// session-cwd readers) with
// arbitrary bytes. Transcripts are files on disk whose records embed whatever
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
	})
}
