//go:build linux || darwin

package sysmon

import "bytes"

// utsField converts a NUL-padded Utsname char array to a string.
func utsField(b []byte) string {
	if before, _, ok := bytes.Cut(b, []byte{0}); ok {
		return string(before)
	}
	return string(b)
}
