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
// signalled. KillGroup is that signal, deferred next to this one.
//
// It requires a command built with exec.CommandContext: os/exec refuses to
// start one whose Cancel it did not set.
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

// KillGroup signals the group GroupKill made, and belongs deferred right after
// it: the Wait that has to have happened for a signal to be worth sending is
// the one cmd.Run or cmd.Output returns from, and a defer reaches it on every
// exit path a caller has.
//
// It is the half GroupKill cannot be. exec calls Cancel only on a deadline,
// and never on the WaitDelay path, so a wrapper CLI that exits on its own
// while a grandchild of its own still holds the output pipe is released by
// WaitDelay with the grandchild alive and unsignalled. Every caller here runs
// on a dashboard timer, so that is one survivor per poll for the life of the
// process.
//
// The signal is unconditional, including after a clean exit: a wrapper that
// exits 0 having spawned a child is the case, not an exception to it. Only
// this command's own group is addressed, since GroupKill is what put it in
// one, so a survivor is always a descendant of the command that was stopped.
func KillGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return // never started, or failed to start: nothing to signal
	}
	// ESRCH is the outcome the kill wanted, so no error is returned to read:
	// a command whose whole tree exited leaves no group, on every poll.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
