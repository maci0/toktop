//go:build unix

package core

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

// The gap GroupKill cannot close is a wrapper that exits on its own with a
// child of its own still running: exec never signals the group on that path,
// so the survivor is reparented to init and, on a dashboard timer, is one
// more process per poll. The test is the fixture this whole file exists for,
// so it asserts the grandchild is alive before the deferred call and gone
// after it: a fixture whose grandchild died on its own would pass the second
// half and prove nothing.
func TestKillGroupTakesAGrandchildThatOutlivedTheChild(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The shell exits at once, so the deadline and the WaitDelay path are
	// both out of it: this is the case where only the caller's own signal
	// reaches the tree. The pid file is the handle on the grandchild, which
	// is otherwise a nameless process in a group the test cannot join.
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & echo $! > "+pidFile+"; exit 0")
	GroupKill(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("the wrapper did not run: %v", err)
	}
	pid := readPIDFile(t, pidFile)
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the grandchild %d was already gone, so this fixture cannot show a survivor being released: %v", pid, err)
	}

	KillGroup(cmd) // what every caller defers next to GroupKill

	for range 100 {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("grandchild %d survived the group kill, so it is reparented to init and one more per poll", pid)
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the grandchild pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("the grandchild pid file holds %q: %v", b, err)
	}
	return pid
}
