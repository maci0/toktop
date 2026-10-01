// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
)

// walkUpDirs calls read at each directory from path's own directory up to
// depth levels above it, nearest first, and returns what the first read that
// succeeds produced. It stops at the filesystem root. Every adapter whose
// session directory is recorded in a file somewhere above the transcript walks
// up to that file rather than assuming a fixed path; this is that walk, and
// read is where each one reads its own file or looks for its own marker, so a
// directory is read once and the walk costs one read per level it visits.
func walkUpDirs[T any](path string, depth int, read func(dir string) (T, bool)) (T, bool) {
	var zero T
	dir := filepath.Dir(path)
	for range depth {
		if v, ok := read(dir); ok {
			return v, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return zero, false
}

// readRootedCapped reads name out of dir through a root opened at dir, so a
// symlink beside the store cannot redirect the read elsewhere, and refuses
// anything longer than limit. These files are written by the agent and are
// read on every rescan: an unbounded read turns a padded file into memory
// held for the length of a poll, and a file past the limit is not one toktop
// can use in any case.
//
// The returned bytes have the UTF-8 BOM stripped.
func readRootedCapped(dir, name string, limit int64) ([]byte, bool) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, false
	}
	defer r.Close()
	f, err := r.Open(name)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, false
	}
	return bytes.TrimPrefix(b, utf8BOM), true
}
