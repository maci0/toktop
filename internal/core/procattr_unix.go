//go:build unix

package core

import (
	"errors"
	"os/exec"
	"syscall"
)

// GroupKill puts cmd in its own process group and replaces
// CommandContext's deadline kill with a group kill. The default Cancel
// signals only the direct child, so a grandchild of a wrapper CLI is
// reparented to init and keeps running past the poll that started it; every
// caller here runs on a dashboard timer, so those survivors accumulate one
// per call for the life of the process. A new group also keeps the group
// signal off toktop itself, which is not in it.
//
// The Cancel covers the other half too: when the direct child exits but a
// grandchild still holds the output pipe, a caller that set WaitDelay has it
// fire Cancel, and the group kill takes the surviving tree with it. Without
// that, Output waits on stdout for EOF and the call pins far past the
// deadline.
//
// It lives here because the agent store discovery, the process listing and
// the GPU sampler each spawn a wrapper that outlives its deadline, and none
// of them should own the fix.
func GroupKill(cmd *exec.Cmd) {
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
