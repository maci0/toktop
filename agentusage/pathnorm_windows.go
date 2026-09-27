// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build windows

package agentusage

import (
	"path/filepath"
	"slices"
	"strings"
)

// NTFS and the Windows APIs look names up case-insensitively and accept both
// separators. An agent records whichever spelling its runtime produced (Node
// likes '/', cmd.exe likes '\'), so directory identity cannot be decided by
// bytes the way it is on Linux: a session started as C:/Users/Foo would
// silently vanish from a watcher resolved as c:\users\foo.

// dirVariants lists the spellings p can be recorded under: itself, then the
// cleaned, slash-normalized, and lowercased forms when they differ. The given
// spelling stays first so callers preferring it keep seeing it first.
func dirVariants(p string) []string {
	cleaned := filepath.Clean(p)
	slash := filepath.ToSlash(cleaned)
	out := []string{p}
	for _, v := range []string{cleaned, slash, strings.ToLower(cleaned), strings.ToLower(slash)} {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// foldSpelling brings a recorded path to the one form sameSpelling compares
// in, and spellingEqual compares two such forms. Split out so a caller
// comparing a path against a set can fold each one once instead of on every
// comparison.
func foldSpelling(p string) string { return filepath.ToSlash(filepath.Clean(p)) }

func spellingEqual(a, b string) bool { return strings.EqualFold(a, b) }
