//go:build linux || darwin

package sysmon

import (
	"bytes"
	"strings"
)

// kernelText decodes a byte field the kernel or a device tree supplied into
// text. Neither is required to hold valid UTF-8: a release string comes from
// the kernel build, a board model from firmware, and a firmware or build
// tool written in another encoding puts a Latin-1 or Shift-JIS byte in
// either. A Go string carries those bytes happily, so nothing downstream
// would catch them.
//
// Each byte that is not part of a well-formed UTF-8 sequence becomes U+FFFD
// here, once, at the boundary. The alternative is worse than a wrong
// character: core.SanitizeText, which every renderer downstream applies,
// drops an ill-formed byte outright, so a Latin-1 "café" would reach the
// operator as "caf" with the letter gone and no sign anything was dropped.
// A replacement character marks the spot the hardware wrote something this
// program cannot read.
func kernelText(b []byte) string { return strings.ToValidUTF8(string(b), "\uFFFD") }

// utsField converts a NUL-padded Utsname char array to a string.
func utsField(b []byte) string {
	if before, _, ok := bytes.Cut(b, []byte{0}); ok {
		return kernelText(before)
	}
	return kernelText(b)
}
