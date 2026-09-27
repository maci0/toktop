// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build !darwin

package core

// Every other platform compares path names by their bytes: Linux stores and
// matches UTF-8 exactly, and a Windows volume holds UTF-16 where the composed
// and decomposed spellings of a name are different names. Rewriting a message
// through a normalization form there would change text no comparison
// considered equal.

// normalizeSpelling returns s unchanged: this platform's file systems do not
// look names up normalization-insensitively.
func normalizeSpelling(s string) string {
	return s
}
