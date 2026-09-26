//go:build windows

package main

import (
	"errors"
	"io"
	"syscall"

	"golang.org/x/sys/windows"
)

// isBrokenPipe reports whether err represents a broken stdout pipe.
// On Windows, writing to a closed pipe yields ERROR_BROKEN_PIPE or ERROR_NO_DATA
// rather than POSIX EPIPE.
func isBrokenPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, windows.ERROR_BROKEN_PIPE) ||
		errors.Is(err, windows.ERROR_NO_DATA)
}
