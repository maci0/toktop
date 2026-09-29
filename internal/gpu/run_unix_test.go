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
	if out, ok := run(context.Background(), "sh", "sh", nil, "-c", "echo hello"); !ok || !strings.Contains(string(out), "hello") {
		t.Fatalf("control run = %q, %v; want hello, true", out, ok)
	}
	start := time.Now()
	const deadline = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	run(ctx, "sh", "sh", nil, "-c", "sleep 5 & echo hello")
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
	run(ctx, "sh", "sh", nil, "-c", "sleep 30 & echo $! > "+pidFile+"; sleep 30")
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
	out, ok := run(ctx, "sh", "sh", nil, "-c", "yes x | head -c 8000000")
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
	out, ok := run(ctx, "sh", "sh", nil, "-c", "head -c 1024 /dev/zero | tr '\\0' 'x'")
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
		if _, ok := run(ctx, tool, tool, nil, "--query"); ok {
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
	noteRunOK(tool, tool)
	if !strings.Contains(lines.String(), "gpu vendor tool answering again") {
		t.Fatalf("recovery wrote no line:\n%s", lines.String())
	}
	// A recovery clears the latch, so a tool that breaks again is reported
	// afresh instead of being silenced by the outage it just recovered from.
	lines.Reset()
	if _, ok := run(ctx, tool, tool, nil, "--query"); ok {
		t.Fatal("run reported success for a tool that does not exist")
	}
	if !strings.Contains(lines.String(), "gpu vendor tool failed") {
		t.Fatalf("a second outage after a recovery wrote no line:\n%s", lines.String())
	}
}

// A driver reinstall or a flipped symlink moves the binary, and lookup
// re-resolves it after toolHitTTL, so the path a tool is run from is not fixed
// for the life of the process. The outage latch is keyed by the tool's name:
// the same tool failing from a new path is the same outage and is not audited
// a second time, and the table holds one entry per vendor rather than one per
// path that binary has ever had.
func TestOutageLatchIsKeyedByToolName(t *testing.T) {
	var lines bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&lines, &slog.HandlerOptions{Level: slog.LevelDebug}))
	old := audit
	audit = func() *slog.Logger { return lg }
	defer func() { audit = old }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const tool = "toktop-vendor-cli-name-latch"
	if _, ok := run(ctx, tool, "/nonexistent/vendor-cli-one", nil, "--query"); ok {
		t.Fatal("run reported success for a tool that does not exist")
	}
	if _, ok := run(ctx, tool, "/nonexistent/vendor-cli-two", nil, "--query"); ok {
		t.Fatal("run reported success for a tool that does not exist")
	}
	if n := strings.Count(lines.String(), "gpu vendor tool failed"); n != 1 {
		t.Fatalf("audited %d outage lines for one tool failing from two paths, want 1:\n%s", n, lines.String())
	}
	s, ok := runState.Load(tool)
	if !ok {
		t.Fatal("the latch is not keyed by the tool name")
	}
	if tr := s.(*toolRun); !tr.failed {
		t.Fatal("the latch cleared on a failure from a new path")
	}
}

// down_for is a duration, and the clock that stamps it is a wall clock on a
// real run. An NTP correction or a laptop resuming from sleep moves it
// backwards between the failure and the recovery, and the raw subtraction then
// audits "down_for=-2h0m0s": a negative outage, which reads as a broken clock
// rather than as a tool that was down for the length it was down.
func TestOutageDownForIsNotNegativeAfterAClockStep(t *testing.T) {
	var lines bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&lines, &slog.HandlerOptions{Level: slog.LevelDebug}))
	old := audit
	audit = func() *slog.Logger { return lg }
	t.Cleanup(func() { audit = old })

	now := time.Unix(1_700_000_000, 0).UTC()
	SetNow(func() time.Time { return now })
	t.Cleanup(func() { SetNow(nil) })

	const tool = "toktop-vendor-cli-clock-step"
	runState.Delete(tool)
	noteRunFailure(tool, tool, errors.New("exec failed"))

	// The clock steps back two hours while the tool stays down.
	now = now.Add(-2 * time.Hour)
	lines.Reset()
	noteRunOK(tool, tool)
	if !strings.Contains(lines.String(), "down_for=0s") {
		t.Fatalf("recovery after a backward step audited a negative down_for:\n%s", lines.String())
	}
}

// A driver upgrade can change a vendor CLI's output shape without making it
// fail: nvidia-smi exits 0 with rows this build cannot split, xpu-smi exits 0
// with something that is not JSON. The GPU row then goes blank and stays
// blank for the rest of the session, which on screen is indistinguishable from
// a host with no GPU of that kind. run must judge the output and audit the
// tool as failing, so a blank row has a line naming the reason behind it.
func TestRunAuditsUnreadableOutput(t *testing.T) {
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

	cases := []struct {
		tool   string
		shout  string
		decode func([]byte) error
	}{
		{"toktop-vendor-cli-nvidia", "echo 'not,a,device,row,at,all'", nvidiaUsable},
		{"toktop-vendor-cli-rocm", "echo '<not json>'", rocmUsable},
		{"toktop-vendor-cli-xpu", "echo '<not json>'", xpuUsable},
	}
	for _, c := range cases {
		lines.Reset()
		runState.Delete(c.tool)
		out, ok := run(ctx, c.tool, "sh", c.decode, "-c", c.shout)
		if ok {
			t.Fatalf("%s: run reported success for output it cannot read: %q", c.tool, out)
		}
		if !strings.Contains(lines.String(), "gpu vendor tool failed") {
			t.Fatalf("%s: unreadable output wrote no outage line:\n%s", c.tool, lines.String())
		}
		if s, stored := runState.Load(c.tool); !stored || !s.(*toolRun).failed {
			t.Fatalf("%s: unreadable output left the tool latched healthy", c.tool)
		}
	}

	// Output the build can read clears nothing and reports no outage, so the
	// gate does not latch a working tool.
	lines.Reset()
	const good = "toktop-vendor-cli-good"
	runState.Delete(good)
	if out, ok := run(ctx, good, "sh", nvidiaUsable, "-c", "echo '0,NVIDIA A,40,1,8192,10,50,550.00'"); !ok {
		t.Fatalf("run rejected readable nvidia-smi output: %q", out)
	}
	if strings.Contains(lines.String(), "gpu vendor tool failed") {
		t.Fatalf("readable output wrote an outage line:\n%s", lines.String())
	}

	// xpu-smi discovery legitimately reports no devices, so the check judges
	// the encoding and not the emptiness.
	if _, ok := run(ctx, good, "sh", xpuUsable, "-c", "echo '[]'"); !ok {
		t.Fatal("xpu-smi was audited as failing for an empty device list")
	}
}
