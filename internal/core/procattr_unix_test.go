//go:build unix

package core

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// Every caller here runs on a dashboard timer, so a command that outlives its
// deadline outlives it by minutes, and one survivor per call is one survivor
// per tick for the life of the process. GroupKill is the fix all of them
// share, so it is pinned here rather than only through whichever caller
// happens to run on this platform.

// The group has to be a new one: a negative pid addresses the group, and
// signalling toktop's own group would take the dashboard down with the vendor
// CLI it was trying to stop.
func TestGroupKillPutsTheCommandInItsOwnGroup(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	// `sh -c exit` returns at once, but Getpgid is read while the process is
	// still there: a reaped child has no pgid left to read, which would turn
	// a wrong group into a skip.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 5")
	GroupKill(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()

	got, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Getpgid on the started command: %v", err)
	}
	if mine, err := syscall.Getpgid(os.Getpid()); err != nil {
		t.Fatalf("Getpgid on this process: %v", err)
	} else if got == mine {
		t.Errorf("the command shares toktop's process group %d, so the group kill would signal the dashboard too", got)
	}
}

// A child that has already exited and been reaped leaves no group to signal.
// ESRCH is the outcome the kill wanted, not a failure to report: a caller
// that treated it as an error would log a failed cancellation every time a
// vendor CLI finished normally.
func TestGroupKillToleratesAnAlreadyDeadGroup(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "exit 0")
	GroupKill(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("the control command did not run: %v", err)
	}
	if cmd.Cancel == nil {
		t.Fatal("GroupKill left Cancel unset, so the deadline kill is the default one")
	}
	if err := cmd.Cancel(); err != nil {
		t.Errorf("Cancel on a group that is already gone returned %v, want nil: the kill wanted exactly this", err)
	}
}

// exec calls Cancel only after a deadline, and it can reach it with no process
// started at all, so the nil guard is load-bearing: a nil dereference there
// panics the goroutine that ran the command rather than reporting anything.
func TestGroupKillCancelWithoutAProcess(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	GroupKill(cmd)
	if cmd.Cancel == nil {
		t.Fatal("GroupKill left Cancel unset, so the deadline kill is the default one")
	}
	if err := cmd.Cancel(); err != nil {
		t.Errorf("Cancel with no process started returned %v, want nil", err)
	}
}
