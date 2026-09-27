// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package remote

import (
	"strings"
	"testing"
)

// ParseTarget's table only reaches validTargetField's whitespace branch: every
// control character it rejects also has to survive url.Parse to get there. A
// host carrying ESC or DEL does, and it is the one that matters, because the
// first-use, forwarding-failure, and connection-lost messages print it to the
// operator's terminal.
func TestValidTargetField(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr string
	}{
		{"empty user is legal", "", ""},
		{"an ordinary host", "gpu-box.lab", ""},
		{"punycode and dots", "xn--bcher-kva.example.com", ""},
		{"an ipv6 literal", "[2001:db8::1]", ""},
		{"a space", "bad host", "whitespace"},
		{"a tab", "bad\thost", "whitespace"},
		{"a carriage return", "bad\rhost", "whitespace"},
		{"a newline", "bad\nhost", "whitespace"},
		{"a NUL", "bad\x00host", "whitespace"},
		{"an ESC", "bad\x1bhost", "control"},
		{"a CSI title-set", "bad\x1b]0;pwned\x07host", "control"},
		{"BEL on its own", "bad\x07host", "control"},
		{"the C0 boundary below newline", "bad\x1fhost", "control"},
		{"DEL", "bad\x7fhost", "control"},
		{"a control character only after a long host", strings.Repeat("a", 200) + "\x1b", "control"},
		{"a right-to-left override", "bad\u202ehost", "invisible"},
		{"a left-to-right mark", "bad\u200fhost", "invisible"},
		{"a zero-width space", "bad\u200bhost", "invisible"},
		{"a soft hyphen", "bad\u00adhost", "invisible"},
		{"a variation selector", "bad\ufe0fhost", "invisible"},
		{"a tag character", "bad\U000e0061host", "invisible"},
		{"an accented host stays legal", "équipe.example.com", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validTargetField(c.in)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("validTargetField(%q) = %v, want nil", c.in, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validTargetField(%q) = nil, want an error mentioning %q", c.in, c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("validTargetField(%q) = %q, want it to mention %q", c.in, err, c.wantErr)
			}
		})
	}
}

// The end-to-end proof that the control branch is reached, not just the
// unit-level one: an ssh URL is the only way an operator supplies a host, and
// the escape must not survive the round trip to a dial.
func TestParseTargetRejectsTerminalEscapesInHostAndUser(t *testing.T) {
	for _, raw := range []string{
		"ssh://bad\x1b]0;pwned\x07host",
		"ssh://bad\x7fhost",
		"ssh://bad\x1bhost",
		"ssh://bad\x1buser@host",
	} {
		if got, err := ParseTarget(raw); err == nil {
			t.Errorf("ParseTarget(%q) = %+v, want an error", raw, got)
		}
	}
}

// UserHost is what goes into the ssh argv, so an absent user must produce the
// bare host and ssh applies its own default.
func TestTargetUserHost(t *testing.T) {
	if got := (Target{Host: "box"}).UserHost(); got != "box" {
		t.Errorf("UserHost with no user = %q, want %q", got, "box")
	}
	if got := (Target{User: "root", Host: "box"}).UserHost(); got != "root@box" {
		t.Errorf("UserHost = %q, want %q", got, "root@box")
	}
	if got := (Target{}).UserHost(); got != "" {
		t.Errorf("UserHost with neither = %q, want empty", got)
	}
}

func TestTargetUserOr(t *testing.T) {
	if got := (Target{User: "root", Host: "box"}).userOr("fallback"); got != "root" {
		t.Errorf("userOr = %q, want the explicit user", got)
	}
	if got := (Target{Host: "box"}).userOr("fallback"); got != "fallback" {
		t.Errorf("userOr with no user = %q, want the default", got)
	}
}

func TestParseTargets(t *testing.T) {
	got, _, err := ParseTargets(nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("ParseTargets(nil) = %v, %v; want no targets and no error", got, err)
	}
	got, _, err = ParseTargets([]string{"ssh://user@box:2222", "ssh://other"})
	if err != nil {
		t.Fatalf("ParseTargets(valid) = %v, want nil", err)
	}
	if len(got) != 2 || got[0].Host != "box" || got[0].Port != 2222 {
		t.Fatalf("ParseTargets(valid) = %+v, want box:2222 and other", got)
	}
	for _, bad := range []string{"ssh://", "ssh://user:secret@box", "ssh://a b"} {
		if _, _, err := ParseTargets([]string{bad}); err == nil {
			t.Fatalf("ParseTargets(%q) = nil error, want rejection", bad)
		}
	}
}

// One host, two spellings. The NFD spelling is what a macOS terminal hands
// the operator for an accented host, and it is the same host: attaching it
// twice forwards the same remote ports twice and lists the same engines
// under two local addresses, so their tokens land in the totals twice.
func TestParseTargetsCollapsesNormalizationVariants(t *testing.T) {
	const composed = "caf\u00e9.example"    // é as one code point
	const decomposed = "cafe\u0301.example" // e + combining acute
	got, dupes, err := ParseTargets([]string{
		"ssh://user@" + composed + ":2222",
		"ssh://user@" + decomposed + ":2222",
	})
	if err != nil {
		t.Fatalf("ParseTargets(nfd) = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("ParseTargets(nfd) = %+v, want one target", got)
	}
	if len(dupes) != 1 {
		t.Fatalf("ParseTargets(nfd) duplicates = %+v, want the NFD spelling dropped", dupes)
	}
	if foldHost(composed) != foldHost(decomposed) {
		t.Fatalf("foldHost(%q) = %q, foldHost(%q) = %q; want one key",
			composed, foldHost(composed), decomposed, foldHost(decomposed))
	}
}

// A target named twice must attach once. Attaching it twice opens a second
// ssh connection, forwards the same remote ports onto a second set of local
// listeners, and lists that host's engines again under different local
// addresses, so its tokens land in the totals twice.
func TestParseTargetsCollapsesRepeats(t *testing.T) {
	got, dupes, err := ParseTargets([]string{"ssh://user@dupbox:2222", "ssh://user@dupbox:2222", "ssh://user@DUPBOX:2222"})
	if err != nil {
		t.Fatalf("ParseTargets(repeat) = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("ParseTargets(repeat) = %+v, want one target", got)
	}
	if len(dupes) != 2 || dupes[0].Host != "dupbox" {
		t.Fatalf("ParseTargets(repeat) duplicates = %+v, want the two dropped targets", dupes)
	}
	// A different port or account on the same host is a different target.
	got, dupes, err = ParseTargets([]string{"ssh://user@dupbox:2222", "ssh://user@dupbox:2223", "ssh://other@dupbox:2222"})
	if err != nil {
		t.Fatalf("ParseTargets(distinct) = %v, want nil", err)
	}
	if len(got) != 3 || len(dupes) != 0 {
		t.Fatalf("ParseTargets(distinct) = %+v with %d duplicates, want three targets", got, len(dupes))
	}
}
