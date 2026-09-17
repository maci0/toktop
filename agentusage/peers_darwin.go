// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build darwin

package agentusage

import (
	"context"
	"net/netip"
	"strconv"
	"time"
)

// Peers lists the TCP endpoints a process is connected to.
//
// An empty result means "cannot tell", which a caller should treat as
// "assume it is not the same engine" rather than as a statement about the
// process. On macOS this asks lsof(8) for the process's internet sockets;
// there is no procfs-equivalent connection table for unprivileged readers.
// -n and -P keep every field numeric so parsing never depends on resolver or
// services databases.
func Peers(pid int) []netip.AddrPort {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := commandOutput(ctx, "lsof", "-a", "-w", "-p", strconv.Itoa(pid),
		"-iTCP", "-FnP", "-n")
	if err != nil {
		return nil // exited, or another user's process
	}
	return parseLsofPeers(string(out))
}
