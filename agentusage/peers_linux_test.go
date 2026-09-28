// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build linux

package agentusage

import (
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

func TestPeersExcludesListeningSockets(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	pid := os.Getpid()
	for _, peer := range Peers(pid) {
		if peer.Port() == 0 || peer.Addr().IsUnspecified() {
			t.Errorf("Peers returned a listening socket's remote endpoint: %s", peer)
		}
	}
	endpoints := []netip.AddrPort{netip.MustParseAddrPort("0.0.0.0:0")}
	if ConnectedTo(pid, endpoints) {
		t.Error("a listening socket was reported as connected")
	}
	if got := MatchingEndpoints([]int{pid}, endpoints); len(got) != 0 {
		t.Errorf("a listening socket matched an endpoint: %v", got)
	}
}

func TestParseHexAddrPort(t *testing.T) {
	cases := map[string]string{
		// The kernel writes each 32-bit word little endian: 0100007F is
		// 127.0.0.1, and 1F90 is port 8080.
		"0100007F:1F90":                         "127.0.0.1:8080",
		"0100007F:2CAA":                         "127.0.0.1:11434",
		"00000000:0050":                         "0.0.0.0:80",
		"00000000000000000000000001000000:1F90": "[::1]:8080",
	}
	for in, want := range cases {
		got, ok := parseHexAddrPort(in)
		if !ok {
			t.Errorf("%s: not parsed", in)
			continue
		}
		if got.String() != want {
			t.Errorf("%s = %s, want %s", in, got, want)
		}
	}
	for _, bad := range []string{"", "nocolon", "ZZ:1F90", "0100007F:ZZZZ", "01:1F90"} {
		if _, ok := parseHexAddrPort(bad); ok {
			t.Errorf("%q should not parse", bad)
		}
	}
}

// TestReadTCPTableMatchesWantedInodes drives the table reader over the kernel's
// own line shape. readTCPTable splits a line in place instead of into a field
// slice, so what has to hold is that the two fields it reads are the ones the
// old split picked: the remote address at index 2 and the inode at index 9.
func TestReadTCPTableMatchesWantedInodes(t *testing.T) {
	// A tcp6 line, since that table carries the 16-byte address form and the
	// widest columns the reader will meet.
	const line = "  17: 00000000000000000000000001000000:1F90 " +
		"0000000000000000FFFF00000100007F:C1B4 01 00000000:00000000 00:00000000 " +
		"00000000  1000        0 4823917 4 000000 100 0 0 10 0"
	path := filepath.Join(t.TempDir(), "tcp6")
	body := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" + line + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	into := map[uint64]netip.AddrPort{}
	if err := readTCPTable(path, map[uint64]bool{4823917: true}, into); err != nil {
		t.Fatal(err)
	}
	want := netip.MustParseAddrPort("127.0.0.1:49588")
	if got := into[4823917]; got != want {
		t.Errorf("remote endpoint = %v, want %v", got, want)
	}
	// An inode nobody asked for contributes nothing, whatever it names.
	other := map[uint64]netip.AddrPort{}
	if err := readTCPTable(path, map[uint64]bool{1: true}, other); err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Errorf("unwanted inode was recorded: %v", other)
	}
}

func TestTCPInode(t *testing.T) {
	for in, want := range map[string]uint64{
		"0":                    0,
		"4823917":              4823917,
		"0000048":              48,
		"18446744073709551615": 1<<64 - 1,
	} {
		got, ok := tcpInode([]byte(in))
		if !ok || got != want {
			t.Errorf("tcpInode(%q) = %d,%v want %d,true", in, got, ok, want)
		}
	}
	// Anything that is not a plain decimal number is refused rather than
	// wrapped into a value that could name a wanted inode.
	for _, bad := range []string{"", "-1", "+1", " 1", "1 ", "0x10", "1.0", "1a", "18446744073709551616", "99999999999999999999"} {
		if got, ok := tcpInode([]byte(bad)); ok {
			t.Errorf("tcpInode(%q) = %d,true, want refusal", bad, got)
		}
	}
}

func TestTCPField(t *testing.T) {
	line := []byte("  0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 12345 1 x")
	for n, want := range map[int]string{0: "0:", 1: "0100007F:1F90", 2: "00000000:0000", 9: "12345", 10: "1"} {
		if got := string(tcpField(line, n)); got != want {
			t.Errorf("tcpField(%d) = %q, want %q", n, got, want)
		}
	}
	// A line that stops short yields nothing rather than a partial field.
	if got := tcpField(line, 12); got != nil {
		t.Errorf("tcpField past the end = %q, want nil", got)
	}
	if got := tcpField([]byte("   "), 0); got != nil {
		t.Errorf("tcpField of blank line = %q, want nil", got)
	}
	if got := tcpField(nil, 0); got != nil {
		t.Errorf("tcpField of empty line = %q, want nil", got)
	}
}
