//go:build unix

package gpu

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

// The context deadline kills the direct child, but Output also waits for the
// command's stdout to reach EOF. A grandchild inheriting that pipe (here: a
// backgrounded sleep) would hold it open for its whole lifetime and pin
// run(), plus whatever lock the caller holds, far past the deadline.
// WaitDelay must reclaim the pipes instead.
func TestRunReclaimsPipesHeldByGrandchild(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	run(ctx, "sh", "-c", "sleep 5 & echo hello")
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("run returned after %s: pipes held by the backgrounded child were not reclaimed at the deadline", elapsed)
	}
}

// A deadline that signals only the direct child leaves the child sh forked
// orphaned and reparented to init, still running its full sleep. Because the
// vendor CLIs are spawned on every poll, those survivors would accumulate one
// per tick for the life of the dashboard. run must put the command in its own
// process group and signal the group, so nothing it started outlives the call.
//
// The parent sleeps too, so the deadline lands while it is still running:
// that is the hang the vendor CLIs actually present, and the only path on
// which exec calls Cancel. When the parent exits first, WaitDelay elapses
// instead and exec kills the direct child itself without asking Cancel
// (os/exec calls Process.Kill on that path), which is what the pipe-reclaim
// test above covers.
func TestRunKillsGrandchildOnDeadline(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	run(ctx, "sh", "-c", "sleep 30 & echo $! > "+pidFile+"; sleep 30")
	pid, err := readPIDFile(pidFile)
	if err != nil {
		t.Skipf("sh did not report a grandchild pid: %v", err)
	}
	// The kill is asynchronous, so poll briefly rather than asserting on the
	// first syscall. A surviving sleep 30 would still be alive here.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("grandchild pid %d survived the deadline: the command's process group was not killed", pid)
}

func readPIDFile(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
