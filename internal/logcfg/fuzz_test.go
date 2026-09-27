// Copyright (C) 2026 Marcel Wysocki
// SPDX-License-Identifier: MIT

package logcfg

import (
	"strings"
	"testing"
)

// FuzzRemote pins the contract every audit line depends on: a peer address
// comes back as "loopback:<port>", "remote" or "unknown", never as the
// address that was handed in. The input is untrusted (net/http interpolates
// conn.RemoteAddr into its panic and handshake lines), so the shapes here are
// the ones a peer can force: bare hosts, zone ids, ports past the range, and
// strings that only look like an address.
func FuzzRemote(f *testing.F) {
	for _, seed := range []string{
		"127.0.0.1:1234",
		"[::1]:80",
		"[fe80::1%eth0]:443",
		"10.0.0.5:65535",
		"8.8.8.8",
		"127.0.0.1",
		":1234",
		"[::1]",
		"host:http",
		"999.999.999.999:1",
		"127.0.0.1:0",
		"127.0.0.1:999999",
		"loopback:1",
		"",
		" \t\n",
		"127.0.0.1\r\n:2",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, addr string) {
		got := Remote(addr)
		if got != addr {
			return
		}
		// Only a string that is already one of the three answers may pass
		// through untouched; anything else would carry the peer address.
		t.Fatalf("Remote(%q) returned the address unchanged", addr)
	})
}

// FuzzRedactAddrs checks the redaction on the text it is actually handed:
// error strings, request ids, header values and body fragments, all of which
// a sender can shape. Every host:port appearance has to become a Remote
// answer, and redacting twice has to be a no-op, so an audit line cannot be
// re-read back into an address by a second pass.
func FuzzRedactAddrs(f *testing.F) {
	for _, seed := range []string{
		"",
		"no addresses here",
		"dial tcp 127.0.0.1:8080: connect: connection refused",
		"[::1]:80 and 10.1.2.3:443",
		"[fe80::1%eth0]:1",
		"127.0.0.1:8080:8080:1",
		"1.2.3.4:5" + strings.Repeat("0", 5000),
		strings.Repeat("1.2.3.4:", 500),
		strings.Repeat("[", 4096),
		strings.Repeat("[::", 2048) + "\n",
		strings.Repeat("0.0.0.0", 2048),
		"999.999.999.999:1",
		"1.2.3.4:99999",
		"a1.2.3.4:1",
		"1.2.3.4:1\r\n5.6.7.8:2",
		"loopback:1 remote unknown",
		"[fe80::1%]:1",
		"é1.2.3.4:1",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := RedactAddrs(s)
		if twice := RedactAddrs(got); twice != got {
			t.Fatalf("RedactAddrs is not idempotent: %q -> %q -> %q", s, got, twice)
		}
		// No host:port appearance may survive a pass: every match in the
		// result has to be one Remote would leave alone, or the peer address
		// is still in the line.
		for _, m := range remoteAddrPat.FindAllString(got, -1) {
			if Remote(m) != m {
				t.Fatalf("RedactAddrs(%q) = %q retains %q", s, got, m)
			}
		}
	})
}
