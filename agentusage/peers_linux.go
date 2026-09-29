// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build linux

package agentusage

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/maci0/toktop/internal/core"
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
		if err := readTCPTable(table, allInodes, byInode); err != nil {
			// A table that could not be read yields no peers for its
			// inodes, and the caller reads that as "not the same engine".
			// Silent, it would attribute a running agent's traffic to
			// nothing while showing it as talking to the engine directly.
			auditLogger().Warn("agent usage: kernel TCP table unreadable; agent peers are unknown",
				"path", table,
				"error", redactStorePath(core.Snippet([]byte(err.Error()))))
		}
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
//
// An error means into holds an incomplete picture, not that the process has
// no peers: the entries decoded before the failure stay, and the caller
// reports the gap rather than presenting the partial table as the truth.
func readTCPTable(path string, want map[uint64]bool, into map[uint64]netip.AddrPort) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Scan() // header
	// The tables are walked on every discovery tick and hold every socket on
	// the host, while a matched line is one or two. Splitting every line into
	// its fields allocated a slice and a dozen strings per connection to throw
	// all but two away, and sc.Text() allocated the line again on top: tcpField
	// walks in place, and only a line whose inode is wanted pays the string the
	// address parser takes.
	//
	// sl local rem st tx:rx retr uid timeout inode
	const (
		remField   = 2
		inodeField = 9
	)
	for sc.Scan() {
		line := sc.Bytes()
		inode, ok := tcpInode(tcpField(line, inodeField))
		if !ok || !want[inode] {
			continue
		}
		rem := tcpField(line, remField)
		if ap, ok := parseHexAddrPort(string(rem)); ok && ap.Port() != 0 && !ap.Addr().IsUnspecified() {
			into[inode] = ap
		}
	}
	// An I/O error or an over-long line stops the scan early; without this
	// the truncated table is indistinguishable from a table in which the
	// missing connections simply do not exist.
	if err := sc.Err(); err != nil {
		return fmt.Errorf("scan %s: %w", path, err)
	}
	return nil
}

// tcpField returns the n-th space-separated field of a /proc/net/tcp line, or
// nil when the line holds fewer than n+1. The bytes are a window into line, so
// the result is only valid while line is.
func tcpField(line []byte, n int) []byte {
	i := 0
	for ; n >= 0; n-- {
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i >= len(line) {
			return nil
		}
		start := i
		for i < len(line) && line[i] != ' ' && line[i] != '\t' {
			i++
		}
		if n == 0 {
			return line[start:i]
		}
	}
	return nil
}

// tcpInode parses a socket inode out of a table field, refusing anything that
// is not a plain decimal number. The kernel writes the inode as one, and a
// field that is anything else belongs to a line this build does not read, so
// the accumulator stops rather than wrapping a malformed field into a value
// that could match a wanted inode.
func tcpInode(f []byte) (uint64, bool) {
	if len(f) == 0 {
		return 0, false
	}
	var n uint64
	for _, c := range f {
		if c < '0' || c > '9' {
			return 0, false
		}
		if n > (math.MaxUint64-uint64(c-'0'))/10 {
			return 0, false
		}
		n = n*10 + uint64(c-'0')
	}
	return n, true
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
