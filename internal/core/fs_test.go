// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import "testing"

// SyncDir is best effort by contract: a write that already renamed its file
// into place must not be reported as failed because the directory could not
// be flushed, or the caller would trade a durable write for none. A directory
// that cannot be opened at all has to be a no-op rather than a panic.
func TestSyncDirNeverFails(t *testing.T) {
	SyncDir(t.TempDir())
	SyncDir("")
	SyncDir(t.TempDir() + "/absent")
}
