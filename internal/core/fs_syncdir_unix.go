// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build !windows

package core

import "os"

// SyncDir flushes the directory entry a rename into place created. The file's
// contents are already fsynced by the writer, but a rename is a change to the
// directory: without this the rename itself can be lost to a crash, leaving
// the previous file in place after the write reported success.
//
// It lives here because two packages write by rename and must both make the
// rename durable (the ssh host-key pin store and the self-update install);
// one helper is the only way those two cannot drift apart.
//
// The reason it could not be flushed is returned, not dropped. A caller that
// reported its write as successful while the flush failed reported a durability
// it never had: the rename is what a crash loses, and the file the writer
// fsynced is not the name the reader opens. Deciding what an unflushable
// directory means belongs to the caller, because it differs per write: a
// renamed binary that does not survive a boot leaves nothing to run, while a
// store a peer can rebuild is a different problem.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
