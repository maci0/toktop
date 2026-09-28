// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build !darwin && !windows

package agentusage

// Outside macOS and Windows no file system in play applies Unicode
// normalization or case-insensitive lookup: on Linux two spellings of one
// accented or differently-cased name are genuinely different directories, so
// equating them would attribute another project's tokens to this watcher.
// Paths are therefore compared exactly as recorded, and each has exactly one
// spelling.

// dirVariants lists the spellings p can be recorded under: just itself.
func dirVariants(p string) []string { return []string{p} }

// foldSpelling brings a recorded path to the one form sameSpelling compares
// in, and spellingEqual compares two such forms. Split out so a caller
// comparing a path against a set can fold each one once instead of on every
// comparison. Outside macOS and Windows nothing folds: a path is its own
// spelling, and one plain comparison stands in for the whole set walk.
func foldSpelling(p string) string { return p }

func spellingEqual(a, b string) bool { return a == b }

// dirKey is the map key form of a recorded path. Nothing folds outside macOS
// and Windows, so the key is the path itself and two spellings that name
// different directories stay different keys.
func dirKey(p string) string { return p }
