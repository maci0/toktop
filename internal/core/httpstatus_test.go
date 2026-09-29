package core

import (
	"strings"
	"testing"
)

// An engine squatting a discovered port writes its own reason phrase, and Go
// reads it off the wire with only the leading space trimmed. A tab survives
// SanitizeText and counts as one cell in widthOf while the terminal runs it to
// the next stop, so the one field every other message folds has to fold this
// one too.
func TestHTTPStatusFoldsTheReasonPhrase(t *testing.T) {
	for _, in := range []string{
		"500 Bad\tX",
		"500 Bad\ntoktop: agent stopped",
		"200 OK\x1b]52;c;QUJD\x07",
		"400 \u202egnihsihp",
	} {
		got := HTTPStatus(in)
		if strings.ContainsAny(got, "\t\n\r\x1b\u202e") {
			t.Errorf("HTTPStatus(%q) = %q, want the reason phrase folded to one printable cell", in, got)
		}
	}
}

// A normal status line comes back with its words, since the fold collapses
// spacing rather than dropping text.
func TestHTTPStatusKeepsTheReasonText(t *testing.T) {
	if got := HTTPStatus("404 Not Found"); got != "404 Not Found" {
		t.Errorf("HTTPStatus = %q, want %q", got, "404 Not Found")
	}
}

// A peer that writes a megabyte of reason phrase must not put a megabyte into
// a status line the operator reads.
func TestHTTPStatusCapsLength(t *testing.T) {
	if got := HTTPStatus("500 " + strings.Repeat("x", 10*SnippetCap)); len([]rune(got)) > SnippetCap {
		t.Errorf("HTTPStatus kept %d characters, want at most %d", len([]rune(got)), SnippetCap)
	}
}
