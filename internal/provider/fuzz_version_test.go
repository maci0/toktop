// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package provider

import (
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// FuzzExtractVersionField drives the version probe's body parser. Any process
// listening on a candidate port answers /api/version, /version or
// /get_server_info, so the body is untrusted and the string that comes back is
// cached and re-rendered every frame. The value must therefore stay inside
// versionCap grapheme clusters (an 8 MB "version" member must not be held),
// be valid UTF-8 in NFC, and stay on one line: the plain-text branch is the
// one that would otherwise hand a body containing a newline straight through
// as a version. The same body must answer identically twice, and a body with
// nothing version-shaped in it must yield nothing rather than the first 128
// bytes of prose.
func FuzzExtractVersionField(f *testing.F) {
	for _, seed := range []string{
		`{"version":"0.5.4"}`,
		`{"Version":"0.5.4"}`,
		`{"backend_version_info":{"version":"0.4.2"}}`,
		`{"server_info":{"sglang_version":"0.4.2.post1"}}`,
		`{"versions":{"vllm_version":"0.6.0"}}`,
		`{"version":""}`,
		`{"version":9}`,
		`{"version":null}`,
		`{"version":["0.5.4"]}`,
		`{"version":{"version":"nested"}}`,
		`{"version":"` + strings.Repeat("9", 4096) + `"}`,
		`{"version":"` + strings.Repeat("9", 128) + `"}`,
		`{"version":"cafe\u0301"}`,
		`{"version":"0.5.4","server_info":{"version":"0.1.0"}}`,
		`"0.5.4"`,
		`"`,
		`0.5.4`,
		"0.5.4\n",
		"line one\nline two",
		"not a version at all, just a sentence of prose about nothing",
		"{}",
		`{"a":1}`,
		"",
		"   ",
		"\x00\x01",
		"\x1b[2J",
		"0.5.4\x1b[31m",
		strings.Repeat("v", 4096),
		strings.Repeat("\n", 4096),
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		text := string(body)
		got := extractVersionField(text)
		if again := extractVersionField(text); again != got {
			t.Fatalf("extractVersionField is not deterministic for %q: %q then %q", text, got, again)
		}
		if got == "" {
			return
		}
		if !utf8.ValidString(got) {
			t.Fatalf("extractVersionField(%q) = %q, not valid UTF-8", text, got)
		}
		if !norm.NFC.IsNormalString(got) {
			t.Fatalf("extractVersionField(%q) = %q, not NFC", text, got)
		}
		if n := utf8.RuneCountInString(got); n > versionCap {
			t.Fatalf("extractVersionField(%q) = %q, %d runes over the %d cap", text, got, n, versionCap)
		}
		// The plain-text branch is the one that can pass a body through
		// unparsed, so a value that came from there must be one line: the
		// engine does not get to add rows to the version readout.
		if !strings.HasPrefix(strings.TrimSpace(text), "{") && strings.ContainsAny(got, "\n\r") {
			t.Fatalf("extractVersionField(%q) = %q, carries a line break", text, got)
		}
		// Prose is not a version. A body past the cap is refused outright, and
		// so is anything carrying JSON or a newline, so the plain-text branch
		// can only ever answer with a body no longer than the cap.
		if !strings.HasPrefix(strings.TrimSpace(text), "{") {
			trimmed := strings.TrimSpace(text)
			if len(trimmed) > versionCap {
				t.Fatalf("extractVersionField(%q) = %q, but a %d-byte body is past the %d cap", text, got, len(trimmed), versionCap)
			}
		}
	})
}
