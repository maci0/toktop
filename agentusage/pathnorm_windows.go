// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build windows

package agentusage

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/maci0/toktop/internal/core"
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
	for _, v := range []string{cleaned, slash, core.FoldCase(cleaned), core.FoldCase(slash)} {
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

// dirKey is the map key form of a recorded path: cleaned, slash-normalized
// and case-folded, which is how NTFS and the Windows APIs look the name up, so
// one checkout spelled two ways is one key.
//
// The fold is core.FoldCase, the one strings.EqualFold compares by, because
// that is the fold NTFS performs. strings.ToLower is a full case mapping and
// over-folds: it renders U+0130 as a bare "i", so the two paths "C:\Users\i"
// and "C:\Users\İ" share a key although the volume keeps them apart, and the
// session recorded under one is watched as the other.
func dirKey(p string) string { return core.FoldCase(foldSpelling(p)) }
