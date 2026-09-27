//go:build unix

package gpu

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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
	// A control run first. Without it a sh that cannot execute at all would
	// make the timed run below return instantly and pass, measuring an exec
	// failure rather than the pipe reclaim.
	if out, ok := run(context.Background(), "sh", "-c", "echo hello"); !ok || !strings.Contains(string(out), "hello") {
		t.Fatalf("control run = %q, %v; want hello, true", out, ok)
	}
	start := time.Now()
	const deadline = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	run(ctx, "sh", "-c", "sleep 5 & echo hello")
	elapsed := time.Since(start)
	if elapsed < deadline {
		t.Fatalf("run returned after %s: the command was not run to its deadline", elapsed)
	}
	if elapsed > 4*time.Second {
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
	// The deadline has to outlast the shell's own startup: it writes the
	// grandchild's pid as its first act, and a deadline landing before that
	// leaves nothing to check. A surviving grandchild sleeps 30s, so a
	// generous deadline still tells a killed group from an unkilled one.
	const runDeadline = 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), runDeadline)
	defer cancel()
	run(ctx, "sh", "-c", "sleep 30 & echo $! > "+pidFile+"; sleep 30")
	pid, err := readPIDFile(pidFile)
	if err != nil {
		// sh was found and the script always writes the pid before it
		// sleeps, so a missing file is a failure. Skipping here would turn
		// a regression that kills the shell before its first write into a
		// green run.
		t.Fatalf("sh did not report a grandchild pid: %v", err)
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

// A vendor CLI has no reason to stop printing, and Output's buffer would grow
// for whatever it emitted inside runTimeout. run caps the read instead, so an
// endless tool is reported as a miss and its output is not retained; the
// sampler runs this several times per tick, so an uncapped read is a memory
// exhaustion the dashboard cannot defend against.
func TestRunCapsUnboundedVendorOutput(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	out, ok := run(ctx, "sh", "-c", "yes x | head -c 8000000")
	if ok {
		t.Fatalf("run accepted %d bytes of unbounded vendor output, want a miss", len(out))
	}
	if len(out) != 0 {
		t.Fatalf("run returned %d bytes with ok=false, want nothing", len(out))
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("run took %s to hit the cap: the write was not refused at the cap", elapsed)
	}
}

// Output that fits must still come through whole, or the cap costs a vendor
// its readings.
func TestRunReturnsOutputUnderTheCap(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, ok := run(ctx, "sh", "-c", "head -c 1024 /dev/zero | tr '\\0' 'x'")
	if !ok {
		t.Fatal("run reported a miss for 1 KiB of vendor output")
	}
	if len(out) != 1024 {
		t.Fatalf("run returned %d bytes, want 1024", len(out))
	}
}

// A vendor CLI that fails is indistinguishable on screen from a host with no
// GPU of that kind: the row is simply empty, and the sampler retries on the
// same spacing for the rest of the session. run must record the failure once,
// name the tool and the reason, and say so again when the tool recovers, so an
// operator debugging a blank GPU row has something to read.
func TestRunAuditsOutageOnceAndRecovery(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	var lines bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&lines, &slog.HandlerOptions{Level: slog.LevelDebug}))
	old := audit
	audit = func() *slog.Logger { return lg }
	defer func() { audit = old }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const tool = "/nonexistent/vendor-cli"
	for range 3 {
		if _, ok := run(ctx, tool, "--query"); ok {
			t.Fatalf("run reported success for a tool that does not exist")
		}
	}
	if n := strings.Count(lines.String(), "gpu vendor tool failed"); n != 1 {
		t.Fatalf("audited %d outage lines for three failing polls, want 1:\n%s", n, lines.String())
	}
	if !strings.Contains(lines.String(), "vendor-cli") {
		t.Fatalf("the outage line does not name the tool:\n%s", lines.String())
	}

	lines.Reset()
	noteRunOK(tool)
	if !strings.Contains(lines.String(), "gpu vendor tool answering again") {
		t.Fatalf("recovery wrote no line:\n%s", lines.String())
	}
	// A recovery clears the latch, so a tool that breaks again is reported
	// afresh instead of being silenced by the outage it just recovered from.
	lines.Reset()
	if _, ok := run(ctx, tool, "--query"); ok {
		t.Fatal("run reported success for a tool that does not exist")
	}
	if !strings.Contains(lines.String(), "gpu vendor tool failed") {
		t.Fatalf("a second outage after a recovery wrote no line:\n%s", lines.String())
	}
}
