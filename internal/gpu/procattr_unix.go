//go:build unix

package gpu

import (
	"errors"
	"os/exec"
	"syscall"
)

// groupKill puts cmd in its own process group and replaces CommandContext's
// deadline kill with a group kill. The default Cancel signals only the direct
// child, so a grandchild of a wrapper CLI is reparented to init and keeps
// running past the poll that started it; the sampler runs this every tick, so
// those survivors accumulate for the life of the dashboard. A new group also
// keeps the group signal off toktop itself, which is not in it.
//
// The same Cancel covers the case pipeGrace was added for: when the direct
// child exits but a grandchild still holds the output pipe, WaitDelay fires
// Cancel, and the group kill takes the surviving tree with it.
func groupKill(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid addresses the whole group. ESRCH means it is already
		// gone, which is the outcome the kill wanted.
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
}
