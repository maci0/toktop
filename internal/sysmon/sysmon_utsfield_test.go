//go:build linux || darwin

package sysmon

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestKernelTextMarksUndecodableBytes(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"ascii", []byte("6.1.0-generic"), "6.1.0-generic"},
		{"utf8", []byte("Raspberry Pi 4 Model B"), "Raspberry Pi 4 Model B"},
		{"latin1", []byte("caf\xe9"), "caf\uFFFD"},
		{"truncated", []byte("caf\xc3"), "caf\uFFFD"},
		{"lone continuation", []byte("\x80"), "\uFFFD"},
		{"stray continuation mid-string", []byte("a\x80b"), "a\uFFFDb"},
		{"empty", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := kernelText(c.in)
			if got != c.want {
				t.Errorf("kernelText(%q) = %q, want %q", c.in, got, c.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("kernelText(%q) = %q, not valid UTF-8", c.in, got)
			}
		})
	}
}

func TestUTsFieldCutsAtNULAndValidates(t *testing.T) {
	var b [65]byte
	copy(b[:], "caf\xe9")
	if got := utsField(b[:]); got != "caf\uFFFD" {
		t.Errorf("utsField = %q, want %q", got, "caf\uFFFD")
	}
	// A field with no NUL is still handed back whole.
	if got := utsField([]byte("caf\xe9")); got != "caf\uFFFD" {
		t.Errorf("utsField(unterminated) = %q, want %q", got, "caf\uFFFD")
	}
}

// The system panel measures these strings with lipgloss.Width, so the string
// has to be well-formed text by the time it gets there, and an undecodable
// region has to still be visible. A run of ill-formed bytes is one maximal
// subpart and becomes one replacement character, so the field reads as
// "unreadable here" rather than as a name the hardware never reported.
func TestKernelTextMarksARunOnce(t *testing.T) {
	got := kernelText([]byte("a\x80\x80b"))
	if want := "a\uFFFDb"; got != want {
		t.Errorf("kernelText(%q) = %q, want %q", "a\x80\x80b", got, want)
	}
	if n := utf8.RuneCountInString(got); n != 3 {
		t.Errorf("kernelText produced %d runes (%q), want 3", n, got)
	}
	if strings.Count(got, "\uFFFD") != 1 {
		t.Errorf("kernelText(%q) should mark the run exactly once", got)
	}
}
