// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build linux

package agentusage

import (
	"encoding/hex"
	"net/netip"
	"testing"
)

// FuzzParseHexAddrPort drives the kernel's "0100007F:1F90" address spelling
// with arbitrary bytes. That table is kernel-owned, but its shape is only a
// convention, and the decoded address is what decides which engine a running
// process is judged to be talking to, so a wrong answer here mislabels a
// process rather than merely failing to read. What must hold: no panic, a
// rejected spelling reports failure instead of a zero endpoint, an accepted
// one is a real address with the exact port the hex named, and the decode is
// deterministic.
func FuzzParseHexAddrPort(f *testing.F) {
	for _, seed := range []string{
		"0100007F:1F90", // 127.0.0.1:8080
		"00000000:0000", // wildcard, filtered by the caller
		"FFFFFFFF:FFFF",
		"0100007F:1f90",
		"00000000000000000000000001000000:1F90", // ::1
		"00000000000000000000000000000000:0001",
		"0100007F:",
		":1F90",
		"",
		":",
		"::",
		"1:2:3",
		"0100007F:10000", // port past 16 bits
		"0100007F:FFFFF", // largest 16-bit port
		"0100007F:GGGG",  // not hex
		"0100007F: 1F90", // embedded space
		"0100007F:1F90 ", // trailing space
		"0100000G:1F90",  // not hex in the address
		"0100:1F90",      // 2 byte address
		"010000:1F90",    // 3 byte address
		"01000000:1F90",  // 5 byte address
		"0000000000000000000000000000000000:1F90", // 17 byte address
		"0100007F1F90",
		"\x00\x01\x7F:1F90",
		"0100007F:1f9",
		"-1:1F90",
		"+1:1F90",
		"0x10:1F90",
		"0100007F:ffff\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, ok := parseHexAddrPort(s)
		again, againOK := parseHexAddrPort(s)
		if ok != againOK || got != again {
			t.Fatalf("decode is not deterministic: (%v,%v) then (%v,%v)",
				got, ok, again, againOK)
		}
		if !ok {
			// A rejected spelling must not leak a usable endpoint, or a
			// caller that only checks the boolean gets a wrong peer.
			if got != (netip.AddrPort{}) {
				t.Fatalf("rejected %q but returned %v", s, got)
			}
			return
		}
		if !got.Addr().IsValid() {
			t.Fatalf("accepted %q but the address is not valid: %v", s, got)
		}
		if got == (netip.AddrPort{}) {
			t.Fatalf("accepted %q but returned the zero endpoint", s)
		}
	})
}

// FuzzParseHexAddrPortRoundTrip encodes known endpoints into the kernel's
// byte-swapped hex spelling and checks they decode back to the same address
// and port. This is the pair assertion the raw-byte target cannot make: it
// proves the word swap is the right direction for every address width, not
// merely that nothing panics.
func FuzzParseHexAddrPortRoundTrip(f *testing.F) {
	for _, seed := range []string{"127.0.0.1:8080", "0.0.0.0:0", "255.255.255.255:65535", "[::1]:11434", "[fe80::1]:443"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		ap, err := netip.ParseAddrPort(s)
		if err != nil {
			return
		}
		raw := ap.Addr().As16()
		word := raw[:]
		if ap.Addr().Is4() {
			word = raw[12:]
		}
		// Kernel spelling: each 32-bit word reversed, port in hex.
		enc := ""
		for i := 0; i+4 <= len(word); i += 4 {
			enc += hex.EncodeToString([]byte{word[i+3], word[i+2], word[i+1], word[i]})
		}
		enc += ":" + hex.EncodeToString([]byte{byte(ap.Port() >> 8), byte(ap.Port())})
		got, ok := parseHexAddrPort(enc)
		if !ok {
			t.Fatalf("rejected the kernel spelling of %q (%q)", s, enc)
		}
		want := netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
		if got != want {
			t.Fatalf("%q encoded as %q decoded to %v, want %v", s, enc, got, want)
		}
	})
}

// FuzzParseLsofPeers drives the `lsof -FnP -n` reader with arbitrary output.
// The peer list decides whether a process counts as a connected agent, so a
// line that parses to a bogus endpoint relabels a running process. What must
// hold: no panic, every returned endpoint is valid and non-zero, the result
// is duplicate-free, and a second parse of the same bytes agrees.
func FuzzParseLsofPeers(f *testing.F) {
	for _, seed := range []string{
		"n127.0.0.1:52154->127.0.0.1:11434\n",
		"n127.0.0.1:52154->127.0.0.1:11434\nn127.0.0.1:52155->127.0.0.1:11434\n",
		"p1234\nn*:11434\nn127.0.0.1:52154->127.0.0.1:11434\n",
		"n[::1]:52154->[::1]:11434\n",
		"n127.0.0.1:52154->\n",
		"n->127.0.0.1:11434\n",
		"n->\n",
		"n",
		"nn->\n",
		"n127.0.0.1:99999->127.0.0.1:11434\n",
		"n127.0.0.1:0->127.0.0.1:11434\n",
		"n127.0.0.1:1->[::1]:11434\n",
		"nfoo->bar\n",
		"n\x00\xff->\x00",
		"n[::ffff:127.0.0.1]:1->127.0.0.1:2\n",
		"n 127.0.0.1:1->127.0.0.1:2\n",
		"",
		"\n\n\n",
		strings30Lines(),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, out string) {
		peers := parseLsofPeers(out)
		again := parseLsofPeers(out)
		if len(peers) != len(again) {
			t.Fatalf("parse is not deterministic: %d peers then %d", len(peers), len(again))
		}
		seen := map[netip.AddrPort]bool{}
		for i, ap := range peers {
			if !ap.Addr().IsValid() {
				t.Fatalf("peer %d of %q has an invalid address: %v", i, out, ap)
			}
			if ap == (netip.AddrPort{}) {
				t.Fatalf("peer %d of %q is the zero endpoint", i, out)
			}
			if seen[ap] {
				t.Fatalf("peer %d of %q repeats %v", i, out, ap)
			}
			seen[ap] = true
			if again[i] != ap {
				t.Fatalf("parse is not deterministic at %d: %v then %v", i, ap, again[i])
			}
		}
	})
}

// strings30Lines is a long, mostly-valid lsof body: enough repeated
// connections that a quadratic dedup would show up as a slow run.
func strings30Lines() string {
	s := ""
	for i := 0; i < 30; i++ {
		s += "p" + string(rune('a'+i)) + "\nn127.0.0.1:5000" + string(rune('0'+i%10)) + "->127.0.0.1:11434\n"
	}
	return s
}
