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

// userHost is what goes into the ssh argv, so an absent user must produce the
// bare host and ssh applies its own default.
func TestTargetUserHost(t *testing.T) {
	if got := (Target{Host: "box"}).userHost(); got != "box" {
		t.Errorf("userHost with no user = %q, want %q", got, "box")
	}
	if got := (Target{User: "root", Host: "box"}).userHost(); got != "root@box" {
		t.Errorf("userHost = %q, want %q", got, "root@box")
	}
	if got := (Target{}).userHost(); got != "" {
		t.Errorf("userHost with neither = %q, want empty", got)
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
