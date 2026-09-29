// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"strings"
	"unicode/utf8"

	"github.com/rivo/uniseg"
	"golang.org/x/text/unicode/norm"
)

// TruncateClusters caps s at n grapheme clusters, cutting only between
// clusters. A grapheme cluster is one user-perceived character: a base letter
// with its combining marks, an emoji ZWJ sequence, a flag built from two
// regional indicators. Cutting inside one renders garbage (a dangling
// zero-width joiner, half a flag), so length caps on retained text must move
// in whole clusters even though they are counted in code points.
//
// The result therefore never holds more than n characters and never ends
// mid-character. n <= 0 yields "".
func TruncateClusters(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if uniseg.GraphemeClusterCount(s) <= n {
		return s
	}
	var b strings.Builder
	state := -1
	for remaining := n; remaining > 0 && s != ""; remaining-- {
		var cluster string
		cluster, s, _, state = uniseg.FirstGraphemeClusterInString(s, state)
		b.WriteString(cluster)
	}
	return b.String()
}

// TailClusters keeps the last n grapheme clusters of s, dropping whole
// clusters from the front. It is TruncateClusters for the ends of a string
// worth reading rather than its start: a peer's stderr tail is where the
// error is, and the reason it must not be a byte slice is the same. A cut at
// a byte offset lands inside a multi-byte sequence, and trimming the partial
// rune that leaves still lands inside a grapheme: an "e" whose U+0301
// combining acute fell on the wrong side prints as an unaccented letter, and
// a family emoji cut before its last element prints as a bare person. n <= 0
// yields "".
func TailClusters(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if uniseg.GraphemeClusterCount(s) <= n {
		return s
	}
	drop := uniseg.GraphemeClusterCount(s) - n
	state := -1
	for ; drop > 0 && s != ""; drop-- {
		_, s, _, state = uniseg.FirstGraphemeClusterInString(s, state)
	}
	return s
}

// ClampField composes s to NFC and caps it at n grapheme clusters, cutting
// only between clusters. Identity fields (agent names, event ids) use this
// so "café" spelled NFD (e + combining acute) and NFC (precomposed) stay one
// name, and a retained emoji is never sliced in half. n <= 0 yields "".
func ClampField(s string, n int) string {
	s = norm.NFC.String(s)
	if n <= 0 {
		return ""
	}
	if len(s) <= n { // fast path: ASCII within cap, no scan
		return s
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return TruncateClusters(s, n)
}

// SnippetCap bounds how much of an error response body is quoted into
// failure messages.
const SnippetCap = 256

// ModelNameMax caps an engine-supplied model id. The id is server-chosen
// data: a /v1/models listing on a misbehaving or hostile engine answers with
// megabyte strings, and one of those would otherwise ride every snapshot, the
// probe request and the --json report at full length. HuggingFace ids fit in
// well under this.
const ModelNameMax = 256

// ModelName is the one shape an engine-supplied model id takes in this
// program: trimmed, terminal-sanitized, folded to one line, capped at ModelNameMax. Every
// ModelInfo built from a listing or a health endpoint goes through it, so an
// engine cannot put a control character or an unbounded string into the
// dashboard, into a probe body, or into the machine-readable report. The
// probe's own cap is the same bound, so a name that survives here is one the
// probe will send unchanged. A GPU's reported name takes the same shape: it is
// driver- and vendor-chosen text, two of its four sources are JSON, and
// SanitizeText keeps newlines, which is right for a block of text and wrong
// for a name the system panel measures with lipgloss.Width and splits on
// newlines.
func ModelName(s string) string {
	return SingleLine(TruncateClusters(strings.TrimSpace(s), ModelNameMax))
}

// HTTPStatus folds an http.Response.Status line into text a single cell can
// carry. The status line is the one part of a hostile engine's answer that
// reaches a message unfolded: Go reads the reason phrase off the wire and only
// trims the leading space, so a listener squatting a discovered port can put a
// tab or a second line in it, and widthOf counts a tab as one cell while the
// terminal runs it to the next stop. The same fold a body gets through Snippet
// is what keeps the reason phrase one cell and one row.
func HTTPStatus(status string) string {
	return SingleLine(Snippet([]byte(status)))
}

// Snippet collapses raw bytes to at most SnippetCap characters (grapheme
// clusters) on one line, cutting between characters so a trailing emoji or
// accented letter from an engine's body is never sliced in half.
func Snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	s = SanitizeText(s)
	return TruncateClusters(s, SnippetCap)
}
