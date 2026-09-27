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
	"github.com/maci0/toktop/internal/core"
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
			out = append(out, agentusage.Process{
				PID:     1000 + int(i),
				Tool:    "claude",
				Dir:     work + string(rune('a'+i)),
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
		for range 400 {
			live.Store(3)
			time.Sleep(200 * time.Microsecond)
			live.Store(0)
			time.Sleep(200 * time.Microsecond)
		}
	})
	wg.Wait()
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if n := len(rec.all()); n < 0 {
		t.Fatal("unreachable")
	}
	_ = core.AgentEvent{}
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
	wg.Go(func() {
		for i := range 200 {
			appendLine(t, filepath.Join(transcript, "s.jsonl"), usageLine(work, 100+i))
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
	cancel()
	select {
	case <-read:
	case <-time.After(30 * time.Second):
		t.Fatal("the tracker goroutine did not return after cancel")
	}
}
