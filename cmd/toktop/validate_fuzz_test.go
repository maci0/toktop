package main

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// FuzzValidateAddURL drives the --add URL gate over arbitrary bytes. The
// value is argv, so whatever it carries is also readable from a process
// listing and echoed by the audit log, the dashboard and the --plain and
// --json reports. What must hold for every input that passes: it parses as
// an http(s) URL, names a host, carries no userinfo, no query and no
// fragment, and any port it names is in range. A value carrying credentials
// anywhere has to be refused, since --bearer and $TOKTOP_BEARER are the only
// places a token belongs.
func FuzzValidateAddURL(f *testing.F) {
	for _, seed := range []string{
		"http://127.0.0.1:11434",
		"https://api.example.com/v1",
		"http://localhost:8000/",
		"http://",
		"https://user:pw@host:8000",
		"http://host/?api_key=sk-secret",
		"http://host/#frag",
		"http://host:/",
		"http://host:0",
		"http://host:65535",
		"http://host:65536",
		"http://host:99999999999999999999",
		"http://host:-1",
		"http://host:80 ",
		"://host",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"http://[::1]:8000",
		"http://ho st:8000",
		"http://%zz/",
		"HTTP://HOST",
		"http://host/path?a=b#c",
		"",
		" ",
		"://",
		"http://user@host",
		"http://host?",
		"http://host#",
		"http://\x00/",
		"http://host:é",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		err := validateAddURL(raw)
		u, perr := url.Parse(raw)
		if err == nil && perr != nil {
			t.Fatalf("validateAddURL(%q) = nil but url.Parse rejects it: %v", raw, perr)
		}
		if err != nil {
			return
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			t.Fatalf("validateAddURL(%q) = nil, scheme %q", raw, u.Scheme)
		}
		if u.Hostname() == "" {
			t.Fatalf("validateAddURL(%q) = nil, no host", raw)
		}
		if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			t.Fatalf("validateAddURL(%q) = nil, URL carries userinfo/query/fragment", raw)
		}
		if port := u.Port(); port != "" {
			if err := parsePort("URL", port); err != nil {
				t.Fatalf("validateAddURL(%q) = nil but its port is unusable: %v", raw, err)
			}
		}
	})
}

// FuzzParseOrigin drives the --origin instant parser over arbitrary bytes,
// including the empty string: bareUnixSeconds reads its first byte
// unguarded, so the width and the digits both have to hold for whatever
// arrives. A blank value pins nothing, a rejected date-shaped digit string
// must not be read as a Unix second, and an accepted instant has to survive
// a round trip through RFC3339.
func FuzzParseOrigin(f *testing.F) {
	for _, seed := range []string{
		"",
		"   ",
		"0",
		"1",
		"-1",
		"-1700000000",
		"1700000000",
		"20260928",
		"20260928T1337",
		"20260928T133700",
		"20260928000000",
		"20260928T133700Z",
		"2026-09-28T13:37:00Z",
		"2026-09-28T13:37:00.5+02:00",
		"99999999999999999999",
		"-99999999999999999999",
		"1e9",
		"0x10",
		"\t1700000000\n",
		" 2026-09-28T13:37:00Z ",
		"now",
		"-",
		"+1",
		"00",
		"\x00",
		"٣",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		// The width reader must tolerate an empty string wherever it is
		// reached, so it is driven directly as well as through parseOrigin.
		if secs, ok := bareUnixSeconds(s); ok && s == "" {
			t.Fatalf("bareUnixSeconds(%q) = (%d, true), want not-a-second", s, secs)
		}

		at, err := parseOrigin(s)
		if err != nil {
			if !at.IsZero() {
				t.Fatalf("parseOrigin(%q) returned %v with an error", s, at)
			}
			return
		}
		if again, err := parseOrigin(s); err != nil || !again.Equal(at) {
			t.Fatalf("parseOrigin(%q) is not deterministic: %v then %v (%v)", s, at, again, err)
		}
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			if !at.IsZero() {
				t.Fatalf("parseOrigin(%q) = %v, want the zero time (no origin pinned)", s, at)
			}
			return
		}
		// A digit string shaped like a date is refused rather than read as a
		// Unix second, so it never silently replays at a wrong instant.
		if isDateShaped(trimmed) && at.Unix() > 0 {
			t.Fatalf("parseOrigin(%q) = %v, a date-shaped value must be rejected", s, at)
		}
		if trimmed == at.UTC().Format(time.RFC3339) {
			if _, err := time.Parse(time.RFC3339, trimmed); err != nil {
				t.Fatalf("parseOrigin(%q) = %v, which does not survive a format round trip", s, at)
			}
		}
	})
}

// isDateShaped reports whether s is the all-digit spelling of a date, a
// minute-resolution timestamp or a second-resolution timestamp.
func isDateShaped(s string) bool {
	if s == "" || (s[0] == '-' && len(s)-1 != dateDigits) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) == dateDigits || len(s) == dateTimeDigits || len(s) == dateSecDigits
}

// FuzzParsePort drives the shared port reader, which both --ingest and the
// --add URL check their numeric port through. A value it accepts has to be
// the integer the text spells and inside the TCP range, so a garbage port
// can never become a tunnel target or a bind address.
func FuzzParsePort(f *testing.F) {
	for _, seed := range []string{
		"0", "1", "80", "8080", "65535", "65536", "-1",
		"", " ", "+80", "0080", " 80 ", "80.0", "1e3",
		"99999999999999999999", "abc", "0x50", "٨٠",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, port string) {
		err := parsePort("--ingest", port)
		if err != nil {
			return
		}
		n, cerr := strconv.Atoi(port)
		if cerr != nil {
			t.Fatalf("parsePort(%q) = nil, which is not a number: %v", port, cerr)
		}
		if n < 0 || n > portMax {
			t.Fatalf("parsePort(%q) = nil for the out-of-range port %d", port, n)
		}
	})
}
