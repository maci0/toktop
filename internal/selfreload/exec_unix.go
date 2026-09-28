//go:build !windows

package selfreload

import (
	"fmt"
	"os"
	"syscall"

	"github.com/maci0/toktop/internal/core"
)

// Restart replaces the current process with a fresh copy of itself, keeping
// arguments and environment. The terminal is restored by the caller before
// this is attempted. Either the process image is replaced, so this call
// never returns, or the reason is reported and toktop exits nonzero.
//
// The reason names the binary it could not exec, and that path is under the
// account's home on an asdf, a rustup or a `go install` prefix. The line is
// the last thing a failing reload prints and the first thing a bug report
// carries, so the home folds to "~" as it does everywhere else toktop
// reports a path.
func Restart(selfPath string, argv, env []string) {
	if err := syscall.Exec(selfPath, argv, env); err != nil {
		fmt.Fprintln(os.Stderr, "toktop:", core.RedactHome(err.Error()))
		os.Exit(1)
	}
}
