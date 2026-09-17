// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build linux

package agentusage

import (
	"bufio"
	"encoding/hex"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func init() { peersByPID = linuxPeersByPID }

// Peers lists the TCP endpoints a process is connected to.
//
// An empty result means "cannot tell", which a caller should treat as
// "assume it is not the same engine" rather than as a statement about the
// process. On Linux this walks the process's open descriptors for socket
// inodes and looks each up in the kernel's TCP tables, all through procfs.
func Peers(pid int) []netip.AddrPort {
	return linuxPeersByPID([]int{pid})[pid]
}

// linuxPeersByPID reads the kernel TCP tables once and matches every pid's
// socket inodes against them.
func linuxPeersByPID(pids []int) map[int][]netip.AddrPort {
	out := make(map[int][]netip.AddrPort, len(pids))
	pidInodes := make(map[int]map[uint64]bool, len(pids))
	allInodes := map[uint64]bool{}
	for _, pid := range pids {
		inodes := socketInodes(pid)
		if len(inodes) == 0 {
			continue
		}
		pidInodes[pid] = inodes
		for ino := range inodes {
			allInodes[ino] = true
		}
	}
	if len(allInodes) == 0 {
		return out
	}
	byInode := map[uint64]netip.AddrPort{}
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		readTCPTable(table, allInodes, byInode)
	}
	for pid, inodes := range pidInodes {
		seen := map[netip.AddrPort]bool{}
		var peers []netip.AddrPort
		for ino := range inodes {
			ap, ok := byInode[ino]
			if !ok || seen[ap] {
				continue
			}
			seen[ap] = true
			peers = append(peers, ap)
		}
		if len(peers) > 0 {
			out[pid] = peers
		}
	}
	return out
}

// socketInodes collects the socket inodes a process holds open.
func socketInodes(pid int) map[uint64]bool {
	dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // exited, or another user's process
	}
	out := make(map[uint64]bool, len(entries))
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		// "socket:[12345]" is the only shape that matters here.
		rest, ok := strings.CutPrefix(target, "socket:[")
		if !ok {
			continue
		}
		num, ok := strings.CutSuffix(rest, "]")
		if !ok {
			continue
		}
		if inode, err := strconv.ParseUint(num, 10, 64); err == nil {
			out[inode] = true
		}
	}
	return out
}

// readTCPTable pulls the remote address of every connection whose inode the
// caller cares about. The kernel's table is fixed-width text: local address,
// remote address, state, and further along, the inode.
func readTCPTable(path string, want map[uint64]bool, into map[uint64]netip.AddrPort) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// sl local rem st tx:rx retr uid timeout inode
		if len(fields) < 10 {
			continue
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil || !want[inode] {
			continue
		}
		if ap, ok := parseHexAddrPort(fields[2]); ok && ap.Port() != 0 && !ap.Addr().IsUnspecified() {
			into[inode] = ap
		}
	}
}

// parseHexAddrPort decodes the kernel's "0100007F:1F90" spelling, which is the
// address in native byte order followed by the port.
func parseHexAddrPort(s string) (netip.AddrPort, bool) {
	host, port, ok := strings.Cut(s, ":")
	if !ok {
		return netip.AddrPort{}, false
	}
	p, err := strconv.ParseUint(port, 16, 16)
	if err != nil {
		return netip.AddrPort{}, false
	}
	raw, err := hex.DecodeString(host)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.AddrPort{}, false
	}
	// Each 32-bit word is little endian on the platforms this runs on, so the
	// bytes of every word are reversed to get network order.
	for i := 0; i+4 <= len(raw); i += 4 {
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	addr, ok := netip.AddrFromSlice(raw)
	if !ok {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(addr.Unmap(), uint16(p)), true
}
