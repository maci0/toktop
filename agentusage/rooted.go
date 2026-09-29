// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"bytes"
	"io"
	"os"
)

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
