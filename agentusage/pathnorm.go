// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

// How one recorded path is spelled, and how two spellings are compared, is a
// per-platform question; foldSpelling and spellingEqual in the pathnorm_<goos>
// files answer it, and dirVariants lists the forms a watcher looks a directory
// up under.

// sameSpelling reports whether two recorded paths denote the same directory
// on this platform, which is the comparison spellingEqual makes once both
// sides are in the one form foldSpelling normalizes them to.
func sameSpelling(a, b string) bool {
	return spellingEqual(foldSpelling(a), foldSpelling(b))
}
