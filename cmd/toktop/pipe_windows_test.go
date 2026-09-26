//go:build windows

package main

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestOutputStatusWindowsBrokenPipe(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"error_broken_pipe", windows.ERROR_BROKEN_PIPE},
		{"error_no_data", windows.ERROR_NO_DATA},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var code int
			stderr := captureStderr(t, func() { code = outputStatus(tt.err) })
			if code != 0 || stderr != "" {
				t.Fatalf("code = %d, stderr = %q; want 0 and no stderr", code, stderr)
			}
		})
	}
}
