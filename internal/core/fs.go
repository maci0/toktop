// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"os"
	"path/filepath"
	"strings"
)

// ExpandHome expands a leading tilde in a path the way a shell would: "~" to
// the home directory, "~/x" (and "~\x" on a Windows config file) to a path
// under it. Anything else, and any path whose home lookup fails, is returned
// unchanged: expanding to an empty base would silently drop the key or
// transcript root the caller named.
//
// It lives here because two packages expand a leading tilde in paths read from
// user-editable files (an ssh_config IdentityFile and an agent transcript
// root); one helper is the only way those two cannot drift apart.
func ExpandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}

// SyncDir flushes the directory entry a rename into place created. The file's
// contents are already fsynced by the writer, but a rename is a change to the
// directory: without this the rename itself can be lost to a crash, leaving
// the previous file in place after the write reported success.
//
// It lives here because two packages write by rename and must both make the
// rename durable (the ssh host-key pin store and the self-update install);
// one helper is the only way those two cannot drift apart.
//
// Best effort by design. A directory that cannot be opened or synced (Windows,
// some network filesystems) leaves the file whole either way, and failing the
// write over it would cost the operator the write.
func SyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
