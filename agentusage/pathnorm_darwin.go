// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build darwin

package agentusage

import (
	"slices"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// macOS file systems (HFS+, APFS) look names up normalization-insensitively
// and, in the default configuration, case-insensitively, while storing
// whichever form was created: "café" spelled NFC and NFD address one and the
// same directory yet differ byte for byte, and so do "Users" and "users". An
// agent records the spelling its own argv carried, so directory identity
// cannot be decided by bytes alone here: a session started from an NFC
// spelling would silently vanish from a watcher resolved through NFD paths.

// dirVariants lists the spellings p can be recorded under: itself, then its
// NFC and NFD forms when they differ. The given spelling stays first so
// callers preferring it keep seeing it first.
func dirVariants(p string) []string {
	out := []string{p}
	for _, v := range []string{norm.NFC.String(p), norm.NFD.String(p)} {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// foldSpelling brings a recorded path to the one form sameSpelling compares
// in, and spellingEqual compares two such forms. Split out so a caller
// comparing a path against a set can fold each one once instead of on every
// comparison: normalizing is a scan and an allocation, and the set a
// candidate is looked up in is walked once per entry.
func foldSpelling(p string) string { return norm.NFC.String(p) }

func spellingEqual(a, b string) bool { return strings.EqualFold(a, b) }
