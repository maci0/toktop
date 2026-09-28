package collector

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/provider"
)

// The collector's shared state is reached from five goroutines at once in a
// real run: Run's emit loop, the sys and process pollers, the probe fan-out
// ProbeAll spawns, the ingest handlers calling RecordAgent, and SetNow.
// -race over this is the check that the locks around them actually hold.
func TestConcurrentEmitRecordProbeClock(t *testing.T) {
	provs := []provider.Provider{}
	for i := range 3 {
		label := fmt.Sprintf("p%d", i)
		provs = append(provs, provider.Provider{Label: label, Addr: "http://127.0.0.1:" + fmt.Sprint(9000+i), Kind: core.KindOllama,
			Poll: func(ctx context.Context) (*provider.Metrics, error) {
				return &provider.Metrics{OutTotal: 10, InTotal: 5, Models: []core.ModelInfo{{Name: "m"}}}, nil
			}})
	}
	c := New(provs, time.Millisecond)
	t.Cleanup(func() { c.SetNow(nil) })
	c.SetSysFn(func() core.SysSample { return core.SysSample{CPUModel: "x"} })
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan core.Snapshot, 4)
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx, out) }()

	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 500 {
				switch w % 4 {
				case 0:
					c.RecordAgent(core.AgentEvent{ID: fmt.Sprintf("id-%d-%d", w, i), At: time.Now(), Agent: "a", OutputTokens: 1})
				case 1:
					c.RecordProbe(core.ProbeSample{At: time.Now(), Addr: "x", TokPS: float64(i)})
				case 2:
					c.ProbeAll()
				case 3:
					c.SetNow(func() time.Time { return time.Now() })
				}
			}
		})
	}
	wg.Wait()
	cancel()
	go func() {
		for range out {
		}
	}()
	<-done
}

// freeNow reports whether mu can be taken right now, taking and releasing it
// to find out. It is how a callback asserts that the lock protecting its
// owner is not held while it runs.
func freeNow(mu *sync.Mutex) bool {
	if !mu.TryLock() {
		return false
	}
	mu.Unlock()
	return true
}

// The injected clock is caller-supplied, so it is called with clockMu released
// at every site that reads it: the two accessors, the stamp SetNow takes, and
// drainWindowRefusals, which is the one call site that reads the clock from
// under c.mu. clockMu is what the accessor owns, so holding it across the call
// is the defect: a clock that reaches back for it to swap itself deadlocks
// there, and a clock that needs c.mu deadlocks the poll loop. Every goroutine
// the collector runs reads this clock, from emit through the ingest handlers
// to the UI's probe wave, so the cost of getting it wrong is a stalled process
// rather than a stalled reader.
func TestInjectedClockRunsWithNoCollectorLockHeld(t *testing.T) {
	c := New(nil, time.Millisecond)
	t.Cleanup(func() { c.SetNow(nil) })
	base := time.Unix(1_700_000_000, 0).UTC()
	var reads, held int
	var reenter, underCMu bool
	c.SetNow(func() time.Time {
		reads++
		if !freeNow(&c.clockMu) || (!underCMu && !freeNow(&c.mu)) {
			held++
		}
		// Armed only for the read below that runs with no collector lock held,
		// since a re-entry from under c.mu is a deadlock by construction: that
		// lock is the caller's, not the clock's. RecordAgent takes c.mu and
		// reads the clock again, which is the re-entrancy the old accessor
		// could not survive.
		if reenter {
			reenter = false
			c.RecordAgent(core.AgentEvent{Agent: "clock", At: base})
		}
		return base.Add(time.Duration(reads) * time.Millisecond)
	})

	reenter = true
	c.instant()
	c.clock()
	// The call site that runs under c.mu: emit drains a window-refusal run
	// while holding it, and the recovery line ages the run off the clock. c.mu
	// is held by design there; clockMu is not.
	c.mu.Lock()
	c.windowLost, c.windowLogged, c.windowRecovered = base, true, "a"
	underCMu = true
	run := c.drainWindowRefusals()
	underCMu = false
	c.mu.Unlock()
	if !run.recovered {
		t.Fatal("the seeded refusal run did not drain, so the clock was never read under c.mu")
	}

	if reads < 3 {
		t.Fatalf("the injected clock was read %d times, too few to cover the accessors and the drain", reads)
	}
	if held != 0 {
		t.Errorf("the injected clock ran with a collector lock held, %d of %d reads", held, reads)
	}
}
