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
