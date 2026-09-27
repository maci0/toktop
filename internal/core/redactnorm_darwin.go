// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build darwin

package core

import "golang.org/x/text/unicode/norm"

// macOS file systems (HFS+, APFS) look names up normalization-insensitively
// while storing whichever form created them, so "rène" spelled NFC and NFD
// name one directory and differ byte for byte. A home directory the file
// system stored decomposed is the same account as the composed spelling a
// process carries in a diagnostic, and a byte comparison misses it.

// normalizeSpelling brings a path spelling to the one form the file system
// compares in. The message and the home directory are both normalized, so the
// offsets replaceFold works with still line up with the text it rewrites.
func normalizeSpelling(s string) string {
	return norm.NFC.String(s)
}
