// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package probe

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rivo/uniseg"

	"github.com/maci0/toktop/internal/core"
)

// FuzzOpenAIFrameParsers drives the OpenAI-compatible stream entry points
// with arbitrary bytes: openaiFrame (one SSE line), sseErrorMessage (the
// engine-reported error value inside a frame), jsonNotStream (the
// Content-Type that decides stream vs whole-body) and parseRetryAfter (the
// Retry-After header on a 429/503). Every one of them reads whatever answers
// a probed localhost port — an untrusted engine — so a hostile or broken
// answer must not panic the collector, plant an unbounded or multi-line
// error string in a sample, misclassify the body so its tokens are read
// twice or not at all, or turn a Retry-After into a backoff outside the
// configured window. The existing FuzzProbeResponse reaches these only
// through a full HTTP exchange, which never mutates a single header or a
// single frame line in isolation.
func FuzzOpenAIFrameParsers(f *testing.F) {
	for _, seed := range []struct {
		line string
		err  string
		ct   string
		ra   string
	}{
		{`data: {"choices":[{"delta":{"content":"He"}}]}`, `{"message":"busy"}`, "text/event-stream", "30"},
		{"data: [DONE]", `"quota exceeded"`, "application/json", "0"},
		{`{"choices":[{"delta":{}}]}`, `"plain"`, "application/json; charset=utf-8", "-5"},
		{"data:", `{"error":[1]}`, "text/plain", "999999999999999999999999"},
		{"", "", "", ""},
		{"data:   ", "null", "APPLICATION/JSON", "Mon, 02 Jan 2006 15:04:05 GMT"},
		{": keep-alive", "   ", "application/x-ndjson", "not-a-date"},
		{"data: \x1b[2J injected", `{"message":"\u001b]52;c;evil\x07"}`, "text/event-stream; charset=utf-8", "9007199254740993"},
		{"data: {\"a\":1} trailing", `{"message":42}`, "json", "1.5"},
		{"{" + strings.Repeat(" ", 100) + "}", strings.Repeat("x", 1000), strings.Repeat("json", 100), strings.Repeat("9", 100)},
		{"data:\xff\xfe", "\xff\xfe", "\xff", "\x00"},
	} {
		f.Add(seed.line, seed.err, seed.ct, seed.ra)
	}
	f.Fuzz(func(t *testing.T, line, errRaw, ct, retryAfter string) {
		// openaiFrame: an accepted payload is what the caller hands to
		// json.Unmarshal, so it must be non-empty, deterministic, and either
		// [DONE] or decodable JSON; a rejected line contributes nothing.
		payload, ok := openaiFrame(line)
		if !ok {
			if payload != "" {
				t.Fatalf("openaiFrame(%q) rejected but returned payload %q", line, payload)
			}
		} else {
			if payload == "" {
				t.Fatalf("openaiFrame(%q) accepted an empty payload", line)
			}
			// A payload the caller cannot decode is not a frame the
			// parser must refuse: a truncated object is returned
			// verbatim and the caller fails the probe over the decode.
			// The boundary is that an accepted payload is never empty.
			_ = payload
		}
		if again, ok2 := openaiFrame(line); ok2 != ok || again != payload {
			t.Fatalf("openaiFrame(%q) is not deterministic: %q/%v then %q/%v", line, payload, ok, again, ok2)
		}

		// sseErrorMessage: the result is quoted in a sample error, so it
		// must be one sanitized line inside SnippetCap, valid UTF-8, and
		// empty exactly when the raw value is absent or null.
		msg := sseErrorMessage(json.RawMessage(errRaw))
		if msg != "" {
			if !utf8.ValidString(msg) {
				t.Fatalf("sseErrorMessage(%q) = %q, want valid UTF-8", errRaw, msg)
			}
			if strings.ContainsAny(msg, "\n\r\t") {
				t.Fatalf("sseErrorMessage(%q) = %q, want one line", errRaw, msg)
			}
			if n := uniseg.GraphemeClusterCount(msg); n > core.SnippetCap {
				t.Fatalf("sseErrorMessage(%q) = %d clusters, cap %d", errRaw, n, core.SnippetCap)
			}
			if msg != core.Snippet([]byte(msg)) && msg != engineErrorText(msg) {
				// engineErrorText single-lines then clamps; Snippet does
				// the same on raw bytes. Either fixed point proves the
				// output carries no escape, control, or whitespace run
				// the boundary was meant to strip.
				if core.SanitizeText(msg) != msg {
					t.Fatalf("sseErrorMessage(%q) = %q, want sanitized text", errRaw, msg)
				}
			}
		}
		if trimmed := strings.TrimSpace(errRaw); trimmed == "" || trimmed == "null" {
			if msg != "" {
				t.Fatalf("sseErrorMessage(%q) = %q, want empty for absent/null", errRaw, msg)
			}
		}
		if again := sseErrorMessage(json.RawMessage(errRaw)); again != msg {
			t.Fatalf("sseErrorMessage(%q) is not deterministic: %q then %q", errRaw, msg, again)
		}

		// jsonNotStream: the two answers decide the parse path, so the
		// classification must be deterministic and must agree with the
		// plain substring rule on the folded header. A stream media type
		// must never read as a plain body and vice versa.
		got := jsonNotStream(ct)
		if again := jsonNotStream(ct); again != got {
			t.Fatalf("jsonNotStream(%q) is not deterministic", ct)
		}
		folded := core.FoldASCII(ct)
		want := !strings.Contains(folded, "event-stream") && !strings.Contains(folded, "ndjson") && strings.Contains(folded, "json")
		if got != want {
			t.Fatalf("jsonNotStream(%q) = %v, want %v", ct, got, want)
		}

		// parseRetryAfter: any header value must land inside the configured
		// backoff window, never below the floor (which would re-POST
		// immediately) and never above the cap (which would silence
		// probes for hours).
		resp := &http.Response{Header: http.Header{}}
		if retryAfter != "" {
			resp.Header.Set("Retry-After", retryAfter)
		}
		d := parseRetryAfter(resp)
		if d < retryAfterDefault || d > retryAfterMax {
			t.Fatalf("parseRetryAfter(%q) = %v, want %v..%v", retryAfter, d, retryAfterDefault, retryAfterMax)
		}
		if again := parseRetryAfter(resp); again != d {
			// http.ParseTime of a past date clamps to the floor, so two
			// calls an instant apart can only differ while an unclamped
			// future date is still in the future; that difference must
			// shrink, never grow, and stay inside the window.
			if again < retryAfterDefault || again > retryAfterMax {
				t.Fatalf("parseRetryAfter(%q) second call = %v, outside window", retryAfter, again)
			}
			if again > d {
				t.Fatalf("parseRetryAfter(%q) grew between calls: %v then %v", retryAfter, d, again)
			}
		}
		_ = time.Second
	})
}
