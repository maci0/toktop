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

// SameDir reports whether two recorded paths name the same directory on this
// platform. A caller outside this package that has to decide directory
// identity (is this the process already being followed, is this store already
// claimed) asks here rather than comparing bytes: on macOS and Windows two
// spellings of one directory differ byte for byte and still name one
// directory, and on Linux they are two directories.
func SameDir(a, b string) bool { return sameSpelling(a, b) }

// DirKey brings a recorded path to the one form SameDir compares in, for a
// caller keying a map by directory. It folds case as well as normalization
// wherever the file system does, so two spellings SameDir calls equal also
// produce one key and a map keyed this way holds a single entry for them.
func DirKey(p string) string { return dirKey(p) }
