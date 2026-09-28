package agentwatch

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maci0/toktop/agentusage"
)

// Discovery rewrites the tracking table (adding, replacing, dropping
// trackers) while the goroutines it started are reporting from it, and the
// table is the only thing the two sides share. Agents appearing and exiting
// on every pass is the interleaving a real --agents run produces. -race over
// this is the check that every path into w.tracked takes w.mu.
func TestConcurrentDiscoveryChurnAndReporting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	work := t.TempDir()
	transcript := filepath.Join(home, ".claude", "projects", "p")
	if err := os.MkdirAll(transcript, 0o755); err != nil {
		t.Fatal(err)
	}

	rec := &recorder{}
	w := New(rec, func() []string { return []string{"http://127.0.0.1:11434"} })
	var live atomic.Int64
	w.listAgents = func() []agentusage.Process {
		var out []agentusage.Process
		for i := int64(0); i < live.Load(); i++ {
			// One store, three processes: the first claims the watcher and
			// the rest are tracked without one, the handover case discovery
			// has to get right while the table churns.
			out = append(out, agentusage.Process{
				PID:     1000 + int(i),
				Tool:    "claude",
				Dir:     work,
				Started: time.Unix(0, 0),
			})
		}
		return out
	}
	w.readEvery = time.Millisecond
	w.discoverEvery = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()

	var wg sync.WaitGroup
	wg.Go(func() {
		out := 100
		for range 40 {
			live.Store(3)
			// Tokens keep landing while the trackers hold the store: the
			// first write of a window is the watcher's baseline, the rest
			// are growth it has to report against a table the next
			// discovery pass rewrites.
			for range 5 {
				writeUsage(transcript, work, out)
				out++
				time.Sleep(time.Millisecond)
			}
			live.Store(0)
			time.Sleep(5 * time.Millisecond)
		}
	})
	wg.Wait()
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	// The goroutines discovery started reported from the table it was
	// rewriting: without an event the whole test is -race and nothing else.
	if got := rec.forPID(1000); len(got) == 0 {
		t.Fatalf("no event from the stub process the churn kept alive; recorded %d events", len(rec.all()))
	}
	w.mu.Lock()
	left := len(w.tracked)
	w.mu.Unlock()
	if left != 0 {
		t.Errorf("%d trackers left after Run returned, want the table drained", left)
	}
}

// writeUsage appends one assistant record to a transcript. It runs on a
// churn goroutine, so a write failure is swallowed rather than t.Fatal'd off
// the test goroutine.
func writeUsage(dir, cwd string, out int) {
	f, err := os.OpenFile(filepath.Join(dir, "s.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(usageLine(cwd, out) + "\n")
}

// SetNow and SetOnError write the two fields every tracker's goroutine reads
// while it stamps an event, and the report path is on a tracker goroutine of
// its own. A caller that installs a clock after Run has started (an embedder
// pinning a timeline when the first event arrives, a demo handing over its
// simulated clock late) therefore writes the clock field under the running
// goroutines' feet. -race over this is the check that both writes take
// clockMu.
func TestSetNowAndOnErrorWhileReporting(t *testing.T) {
	work, transcript := claudeHome(t)
	w, _, tr := followClaude(t, work)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	read := make(chan struct{})
	go func() {
		defer close(read)
		tr.watch.Run(ctx, time.Millisecond, func(s agentusage.Sample) { w.report(tr, s) })
	}()

	var wg sync.WaitGroup
	var writeErr error
	wg.Go(func() {
		for i := range 200 {
			if err := appendLineQuiet(filepath.Join(transcript, "s.jsonl"), usageLine(work, 100+i)); err != nil {
				writeErr = err
				return
			}
			time.Sleep(200 * time.Microsecond)
		}
	})
	wg.Go(func() {
		for i := range 400 {
			w.SetNow(func() time.Time { return time.Unix(0, int64(i)) })
			w.SetOnError(func(error) {})
			time.Sleep(200 * time.Microsecond)
		}
	})
	wg.Wait()
	if writeErr != nil {
		t.Fatalf("appending to the transcript: %v", writeErr)
	}
	cancel()
	select {
	case <-read:
	case <-time.After(30 * time.Second):
		t.Fatal("the tracker goroutine did not return after cancel")
	}
}
