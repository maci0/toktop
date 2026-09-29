// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build windows

package core

// SyncDir reports a flush that Windows has no call for.
//
// os.Open on a directory there succeeds with a read-only handle
// (FILE_FLAG_BACKUP_SEMANTICS), and File.Sync on it bottoms out in
// FlushFileBuffers, which needs write access on the handle: every call
// returns "Access is denied." A caller reading that as "the rename is not
// durable" fails a write that landed, which is how the ssh host-key pin store
// aborted the first connect to every host with the pin already on disk.
//
// The rename is durable without the call: the NTFS journal writes its metadata
// through, so a crash cannot leave the previous name in place. Reporting
// success is therefore the truth here, not a swallowed failure.
func SyncDir(string) error { return nil }
