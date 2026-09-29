package collector

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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
	// The drainer runs from the start, not after the writers, so the count is
	// the frames the emit loop actually produced. Without it the test is a
	// deadlock detector: every call below could be a no-op and it would still
	// pass, because nothing here asserts that the collector did any work.
	// Run does not close out, so the counter is read, not waited on.
	var frames atomic.Int64
	go func() {
		for range out {
			frames.Add(1)
		}
	}()

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
	<-done
	if frames.Load() == 0 {
		t.Fatal("the emit loop produced no frame: the concurrent calls above shared no state with the run they were racing")
	}
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

// A run that is shutting down joins the probe fan-out while a --probe ticker
// or a held 'p' can still be calling ProbeAll. The Add and the Wait have to be
// ordered, not merely unlikely to meet: an Add landing after the Wait is a
// WaitGroup misuse panic, and a generation that starts past the join records
// into a collector a later run already owns.
func TestProbeAllRacingRunShutdown(t *testing.T) {
	oldGap := probeWaveGap
	probeWaveGap = 0
	defer func() { probeWaveGap = oldGap }()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		io.WriteString(w, "{\"response\":\"one\",\"done\":true,\"eval_count\":1,\"eval_duration\":1000000}\n")
	}))
	defer srv.Close()

	c := New([]provider.Provider{(&fakeProvider{label: "p", addr: srv.URL}).asProvider()}, time.Millisecond)
	c.SetSysFn(func() core.SysSample { return core.SysSample{CPUModel: "x"} })
	c.mu.Lock()
	c.lastModel[srv.URL] = "m"
	c.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan core.Snapshot, 8)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for ctx.Err() == nil {
				c.ProbeAll()
			}
		})
	}
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx, out) }()

	cancel()
	wg.Wait()
	<-done // Run's join is the point: it must not race the ProbeAll calls above

	// Every generation the fan-out launched has been joined by Run, so the
	// inflight set is empty rather than naming a backend whose goroutine is
	// still recording into this collector.
	waitFor(t, func() bool {
		c.probeMu.Lock()
		defer c.probeMu.Unlock()
		return len(c.probeInflight) == 0
	}, "Run returned with a probe generation still in flight")

	// A trigger that lands after the join is refused rather than launched:
	// the generation it would start is one Run did not wait for.
	// The join latch is what makes that join sound, so it is asserted rather
	// than inferred: a probe launched past it is one Run never waited for,
	// and the canceled baseCtx hides it (the generation returns before it
	// makes a request), so the request count cannot see it.
	c.probeMu.Lock()
	latched := c.probeClosing
	c.probeMu.Unlock()
	if !latched {
		t.Error("Run returned without latching the probe fan-out closed")
	}
	c.mu.Lock()
	c.lastModel[srv.URL] = "m"
	c.mu.Unlock()
	before := hits.Load()
	for range 5 {
		c.ProbeAll()
	}
	waitStay(t, 100*time.Millisecond, func() bool { return hits.Load() == before },
		"ProbeAll started a generation after Run had returned")

	// The gate itself, on a live context where a launched generation would
	// reach the engine: with the fan-out latched closed the wave is dropped,
	// and with a model known to the collector, so a no-op is the gate and
	// not an empty target list.
	live := New([]provider.Provider{(&fakeProvider{label: "p", addr: srv.URL}).asProvider()}, time.Second)
	live.SetSysFn(func() core.SysSample { return core.SysSample{CPUModel: "x"} })
	live.mu.Lock()
	live.lastModel[srv.URL] = "m"
	live.probeMu.Lock()
	live.probeClosing = true
	live.probeMu.Unlock()
	live.mu.Unlock()
	gate := hits.Load()
	live.ProbeAll()
	waitStay(t, 100*time.Millisecond, func() bool { return hits.Load() == gate },
		"ProbeAll launched a wave with the fan-out latched closed")

	// A later run is a fresh join: the latch does not outlive the run that
	// set it, or probing would be dead for the rest of the process.
	ctx2, cancel2 := context.WithCancel(context.Background())
	run2 := make(chan struct{})
	go func() { defer close(run2); _ = c.Run(ctx2, out) }()
	waitFor(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.running
	}, "the restarted run never started")
	waitFor(t, func() bool {
		c.ProbeAll()
		return hits.Load() > before
	}, "the restarted run never probed again")
	cancel2()
	<-run2
}
