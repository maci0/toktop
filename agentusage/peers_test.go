// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"net"
	"net/netip"
	"os"
	"runtime"
	"testing"
	"time"
)

func TestParseLsofPeers(t *testing.T) {
	out := "p4242\n" +
		"n127.0.0.1:52154->127.0.0.1:11434\n" +
		"n*:8000\n" + // listening socket: no remote end
		"n192.168.1.7:53000->10.0.0.5:8000\n" +
		"n*:*->*:*?\n" + // unconnected UDP: remote does not parse as an endpoint
		"fcwd\nn/some/path\n" // non-socket field lines

	got := parseLsofPeers(out)
	want := []netip.AddrPort{
		netip.MustParseAddrPort("127.0.0.1:11434"),
		netip.MustParseAddrPort("10.0.0.5:8000"),
	}
	if len(got) != len(want) {
		t.Fatalf("peers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("peer[%d] = %v, want %v", i, got[i], want[i])
		}
	}

	// A loopback spelling must survive parsing so sameEndpoint can collapse
	// it against an engine advertised on ::1.
	if peers := parseLsofPeers("p1\nn[::1]:40000->[::1]:11434\n"); len(peers) != 1 ||
		peers[0].String() != "[::1]:11434" {
		t.Errorf("IPv6 peer = %v, want [::1]:11434", peers)
	}
	if peers := parseLsofPeers(""); len(peers) != 0 {
		t.Errorf("empty output gave %v", peers)
	}
}

func TestSameEndpointTreatsLoopbackSpellingsAsOne(t *testing.T) {
	v4 := netip.MustParseAddrPort("127.0.0.1:11434")
	v6 := netip.MustParseAddrPort("[::1]:11434")
	other := netip.MustParseAddrPort("192.168.1.5:11434")
	wrongPort := netip.MustParseAddrPort("127.0.0.1:8080")

	if !sameEndpoint(v4, v6) {
		t.Error("loopback spellings are the same engine")
	}
	if sameEndpoint(v4, other) {
		t.Error("a remote host is not loopback")
	}
	if sameEndpoint(v4, wrongPort) {
		t.Error("a different port is a different engine")
	}
}

// TestConnectedToSeesARealConnection is the whole point of the file: an agent
// talking to an engine must be recognizable, so its tokens are not counted
// twice.
func TestConnectedToSeesARealConnection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no connection table without native APIs")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	engine := netip.MustParseAddrPort(ln.Addr().String())
	pid := os.Getpid()
	if !ConnectedTo(pid, []netip.AddrPort{engine}) {
		t.Fatalf("a live connection to %s was not detected", engine)
	}

	// An endpoint nobody is talking to must not match, or every agent would
	// look engine-backed and report nothing.
	elsewhere := netip.MustParseAddrPort("127.0.0.1:1")
	if ConnectedTo(pid, []netip.AddrPort{elsewhere}) {
		t.Error("matched an endpoint with no connection")
	}
	if ConnectedTo(pid, nil) {
		t.Error("no endpoints means nothing to match")
	}

	got := MatchingEndpoints([]int{pid, -1}, []netip.AddrPort{engine, elsewhere})
	if got[pid] != engine {
		t.Errorf("MatchingEndpoints = %v, want pid %d -> %s", got, pid, engine)
	}
	if _, ok := got[-1]; ok {
		t.Error("dead pid matched an endpoint")
	}
}

func TestPeersExcludesUDP(t *testing.T) {
	ln, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	conn, err := net.DialTimeout("udp4", ln.LocalAddr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	endpoint := netip.MustParseAddrPort(ln.LocalAddr().String())
	pid := os.Getpid()
	for _, peer := range Peers(pid) {
		if peer == endpoint {
			t.Errorf("Peers returned a UDP endpoint: %s", peer)
		}
	}
	if ConnectedTo(pid, []netip.AddrPort{endpoint}) {
		t.Error("a UDP connection was reported as a TCP connection")
	}
	if got := MatchingEndpoints([]int{pid}, []netip.AddrPort{endpoint}); len(got) != 0 {
		t.Errorf("a UDP connection matched a TCP endpoint: %v", got)
	}
}

func TestPeersOnAnImpossiblePID(t *testing.T) {
	// Cannot tell is not the same as connected: a dead process must not be
	// reported as talking to anything.
	if got := Peers(-1); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	if ConnectedTo(-1, []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:1")}) {
		t.Fatal("a dead process cannot hold a connection")
	}
}
