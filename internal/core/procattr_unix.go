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
// The Cancel covers the deadline: when the parent is still running at the
// deadline, os/exec calls it and the group kill takes the tree with it.
//
// It does not cover the WaitDelay path. When the direct child exits on its
// own while a grandchild still holds the output pipe, os/exec closes the
// pipes on the WaitDelay expiry without calling Cancel ("if pipes are closed
// due to WaitDelay, no Cancel call has occurred"), and the survivor is never
// signalled. A caller that needs the tree taken in that case has to signal the
// group itself once the command returns.
//
// It requires a command built with exec.CommandContext: os/exec refuses to
// start one whose Cancel it did not set.

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
