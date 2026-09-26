//go:build !windows

package main

import (
	"errors"
	"io"
	"syscall"
)

// isBrokenPipe reports whether err represents a broken stdout pipe.
func isBrokenPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, io.ErrClosedPipe)
}
