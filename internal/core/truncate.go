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

// Snippet collapses raw bytes to at most SnippetCap characters (grapheme
// clusters) on one line, cutting between characters so a trailing emoji or
// accented letter from an engine's body is never sliced in half.
func Snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	s = SanitizeText(s)
	return TruncateClusters(s, SnippetCap)
}
