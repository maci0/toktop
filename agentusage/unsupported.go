// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build !linux && !darwin

package agentusage

import "net/netip"

// Platforms without a process-table implementation share this one stub file.
// Their two answers have the same shape and the same reason: an empty result
// means "cannot tell", never "nothing is running".

// Discover lists the agent CLIs running on this machine.
//
// An empty result means "cannot tell" rather than "nothing is running", which
// callers should surface as no local agents rather than an error. Windows
// reports nothing: Win32_Process exposes a command line but not a working
// directory, and the directory is what attributes a transcript to a process
// (see Process.Dir). Reading it would mean undocumented
// NtQueryInformationProcess PEB walks. Platforms with neither procfs nor
// ps(1) have no implementation at all for the same reason.
func Discover() []Process { return nil }

// Peers lists the TCP endpoints a process is connected to.
//
// An empty result means "cannot tell", which a caller should treat as
// "assume it is not the same engine" rather than as a statement about the
// process. Windows would need GetExtendedTcpTable plus owner-PID rows, which
// this package has no native binding for.
func Peers(int) []netip.AddrPort { return nil }
