package demo

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/maci0/toktop/internal/core"
)

func collectOne(t *testing.T, s *Source) core.Snapshot {
	t.Helper()
	ch := make(chan core.Snapshot, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx, ch)
	select {
	case snap := <-ch:
		return snap
	// A bound keeps a stalled source from hanging the whole suite.
	case <-time.After(5 * time.Second):
		t.Fatal("demo source produced no snapshot")
		return core.Snapshot{}
	}
}

// The seed is the whole run, so a source has to be able to report the one it
// was built with: the dashboard shows it and the operator replays with it.
func TestSeedReportsConstructionSeed(t *testing.T) {
	if got := NewSource(time.Second, 7).Seed(); got != 7 {
		t.Errorf("Seed() = %d, want 7", got)
	}
}

func TestDeterministicPerSeed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		testDeterministicPerSeed(t)
	})
}

func testDeterministicPerSeed(t *testing.T) {
	a := collectOne(t, NewSource(10*time.Millisecond, 7))
	b := collectOne(t, NewSource(10*time.Millisecond, 7))
	if len(a.Providers) == 0 || len(a.Providers) != len(b.Providers) {
		t.Fatalf("provider count mismatch: %d vs %d", len(a.Providers), len(b.Providers))
	}
	for i := range a.Providers {
		pa, pb := a.Providers[i], b.Providers[i]
		// The first frame carries no wall-clock state: everything a seed
		// controls must match exactly, across the whole history not just
		// sample zero.
		if pa.Label != pb.Label || pa.OutTokPS != pb.OutTokPS || pa.InTokPS != pb.InTokPS ||
			pa.KVPct != pb.KVPct || pa.Running != pb.Running || pa.Waiting != pb.Waiting ||
			!reflect.DeepEqual(pa.OutHist, pb.OutHist) || !reflect.DeepEqual(pa.InHist, pb.InHist) {
			t.Fatalf("seeded sources diverged at provider %d:\n%+v\n%+v", i, pa, pb)
		}
	}
	if a.Sys.MemUsed != b.Sys.MemUsed || a.Sys.Load1 != b.Sys.Load1 ||
		!reflect.DeepEqual(a.Sys.GPUs, b.Sys.GPUs) {
		t.Fatalf("seeded vitals diverged:\n%+v\n%+v", a.Sys, b.Sys)
	}
}

// Two sources stepped at the same instants with the same seed must produce
// identical snapshots, timestamps included. Ticker-driven Run still takes
// one wall-clock read to place t0, so this is the byte-for-byte check.
func TestDeterministicFrames(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	a, b := NewSource(time.Second, 7), NewSource(time.Second, 7)
	var last core.Snapshot
	for i := range 24 {
		now := t0.Add(time.Duration(i) * time.Second)
		sa, sb := a.stepAt(now), b.stepAt(now)
		if i == 10 {
			a.ProbeAll()
			b.ProbeAll()
		}
		if !reflect.DeepEqual(sa, sb) {
			t.Fatalf("seeded sources diverged at frame %d:\n%+v\n%+v", i, sa, sb)
		}
		last = sa
	}
	if last.Uptime != 23*time.Second {
		t.Fatalf("uptime = %v, want 23s", last.Uptime)
	}
	if !last.At.Equal(t0.Add(23 * time.Second)) {
		t.Fatalf("At = %v, want %v", last.At, t0.Add(23*time.Second))
	}
	other := NewSource(time.Second, 8).stepAt(t0)
	same := NewSource(time.Second, 7).stepAt(t0)
	if reflect.DeepEqual(same, other) {
		t.Fatal("different seeds produced identical first frames")
	}
}

// Run is the path `toktop --demo` takes, and it places the timeline on the
// wall clock unless the origin is pinned. Two runs of one seed would then
// report equal values on two different axes, so a replay could not be
// diffed against the run it reproduces. Pinned, every frame must match.
func TestRunReplaysIdenticallyFromOneSeed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t0 := time.Unix(1_700_000_000, 0).UTC()
		frames := func() []core.Snapshot {
			s := NewSource(10*time.Millisecond, 7)
			s.SetOrigin(t0)
			ch := make(chan core.Snapshot, 64)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			go s.Run(ctx, ch)
			var out []core.Snapshot
			for len(out) < 20 {
				select {
				case snap := <-ch:
					out = append(out, snap)
				case <-ctx.Done():
					t.Fatalf("source produced %d frames, want 20", len(out))
				}
			}
			return out
		}
		a, b := frames(), frames()
		for i := range a {
			if !reflect.DeepEqual(a[i], b[i]) {
				t.Fatalf("replays diverged at frame %d:\n%+v\n%+v", i, a[i], b[i])
			}
		}
		if !a[0].At.Equal(t0) {
			t.Fatalf("first frame at %v, want the pinned origin %v", a[0].At, t0)
		}
	})
}

// Auto-probe is a simulated schedule, not a wall-clock ticker: two sources
// stepped at the same instants must run the same number of waves and stamp
// them at the same simulated instants. Driven from the real clock the wave
// count and every wave's stamp followed how long the process happened to
// take, and the run stopped replaying from its seed.
func TestProbeEveryIsSimulated(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	replay := func() core.Snapshot {
		s := NewSource(time.Second, 7)
		s.SetOrigin(t0)
		s.ProbeEvery(3 * time.Second)
		var last core.Snapshot
		for i := range 10 {
			last = s.stepAt(t0.Add(time.Duration(i) * time.Second))
		}
		return last
	}
	a, b := replay(), replay()
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("auto-probe replays diverged:\n%+v\n%+v", a.Probes, b.Probes)
	}
	// One wave every 3s over frames at 0s..9s, each covering every backend.
	waves := map[time.Time]int{}
	for _, p := range a.Probes {
		waves[p.At]++
	}
	for _, at := range []time.Time{t0, t0.Add(3 * time.Second), t0.Add(6 * time.Second), t0.Add(9 * time.Second)} {
		if waves[at] == 0 {
			t.Fatalf("no auto-probe wave stamped %v; got %v", at, waves)
		}
	}
	// Every wave stamps a simulated instant, never wall time.
	for _, p := range a.Probes {
		if p.At.Before(t0) || p.At.After(t0.Add(9*time.Second)) {
			t.Fatalf("probe stamped %v, outside the simulated timeline %v..%v", p.At, t0, t0.Add(9*time.Second))
		}
	}
}

// A cadence shorter than the poll interval has to catch up rather than leave
// the schedule behind the timeline, or the later frames would stop probing.
func TestProbeEveryShorterThanInterval(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	s := NewSource(4*time.Second, 7)
	s.SetOrigin(t0)
	s.ProbeEvery(time.Second)
	s.stepAt(t0)
	if got, want := len(s.probes), len(s.backends); got != want {
		t.Fatalf("first frame fired %d samples, want one wave of %d", got, want)
	}
	s.stepAt(t0.Add(4 * time.Second))
	if got, want := len(s.probes), 5*len(s.backends); got != want {
		t.Fatalf("second frame fired %d samples, want four waves of %d", got, len(s.backends))
	}
}

// A timeline that already has stamped history cannot be moved under it, so
// SetOrigin refuses rather than leaving the caller believing it took.
func TestSetOriginAfterFirstFramePanics(t *testing.T) {
	s := NewSource(time.Second, 7)
	s.stepAt(time.Unix(1_700_000_000, 0))
	defer func() {
		if recover() == nil {
			t.Fatal("SetOrigin after the first frame did not panic")
		}
	}()
	s.SetOrigin(time.Unix(0, 0))
}

// The probe cadence arms on the origin, so a source that has already stepped
// a frame cannot take one: the wave it is asking for would sit outside the
// run the stamped history records.
func TestProbeEveryAfterFirstFramePanics(t *testing.T) {
	s := NewSource(time.Second, 7)
	s.stepAt(time.Unix(1_700_000_000, 0))
	defer func() {
		if recover() == nil {
			t.Fatal("ProbeEvery after the first frame did not panic")
		}
	}()
	s.ProbeEvery(time.Second)
}

// ProbeAll that wins the race with the first frame must pin the origin Run
// then uses, so probes and the first snapshot share one instant.
func TestProbeAllPinsOriginForRun(t *testing.T) {
	s := NewSource(time.Second, 3)
	s.ProbeAll()
	pinned := s.Now()
	if pinned.IsZero() {
		t.Fatal("ProbeAll left the origin unpinned")
	}
	if second := s.Now(); !second.Equal(pinned) {
		t.Fatalf("Now moved after pin: %v then %v", pinned, second)
	}
	snap := s.stepAt(pinned)
	if !snap.At.Equal(pinned) {
		t.Fatalf("first frame At = %v, want pinned %v", snap.At, pinned)
	}
	if len(s.probes) == 0 {
		t.Fatal("ProbeAll produced no samples")
	}
	for _, p := range s.probes {
		if !p.At.Equal(pinned) {
			t.Fatalf("probe At = %v, want pinned %v", p.At, pinned)
		}
	}
}

// ProbeAll and RecordAgent stamp the simulated instant, not wall time, so
// injected activity stays on the seeded timeline.
func TestExternalStampsUseSimulatedTime(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	s := NewSource(time.Second, 3)
	s.stepAt(t0)
	s.RecordAgent(core.AgentEvent{Agent: "x"})
	if len(s.agents) != 1 || !s.agents[0].At.Equal(t0) {
		t.Fatalf("agent At = %v, want %v", s.agents, t0)
	}
	if !s.Now().Equal(t0) {
		t.Fatalf("Now = %v, want simulated %v", s.Now(), t0)
	}
	s.ProbeAll()
	if len(s.probes) != len(s.backends) {
		t.Fatalf("probes = %d, want %d", len(s.probes), len(s.backends))
	}
	for _, p := range s.probes {
		if !p.At.Equal(t0) {
			t.Fatalf("probe At = %v, want simulated %v", p.At, t0)
		}
	}
}

func TestSysSamplePresent(t *testing.T) {
	snap := collectOne(t, NewSource(10*time.Millisecond, 5))
	if snap.Sys == nil {
		t.Fatal("demo snapshot missing Sys")
	}
	sys := snap.Sys
	if sys.MemTotal == 0 || sys.MemUsed > sys.MemTotal {
		t.Errorf("implausible memory: used=%d total=%d", sys.MemUsed, sys.MemTotal)
	}
	var haveCPU bool
	for _, tr := range sys.Temps {
		c := float64(tr.MilliC) / 1000
		if c < 20 || c > 120 {
			t.Errorf("implausible temp: %+v", tr)
		}
		haveCPU = haveCPU || !tr.IsGPU
	}
	if !haveCPU {
		t.Errorf("expected a CPU sensor, got %+v", sys.Temps)
	}
	if len(sys.GPUs) == 0 {
		t.Fatal("expected synthesized GPUs")
	}
	for _, g := range sys.GPUs {
		switch {
		case g.MilliC != 0 && (g.MilliC < 20000 || g.MilliC > 120000):
			t.Errorf("implausible GPU temp: %+v", g)
		case g.MemTotal > 0 && g.MemUsed > g.MemTotal:
			t.Errorf("implausible VRAM use: %+v", g)
		case g.UtilPct < 0 || g.UtilPct > 100:
			t.Errorf("implausible util: %+v", g)
		case g.PowerW < 0 || g.PowerW > 1200:
			t.Errorf("implausible power: %+v", g)
		}
	}
}

func TestProbeAllProducesSamples(t *testing.T) {
	s := NewSource(10*time.Millisecond, 3)
	s.ProbeAll()
	if len(s.probes) != len(s.backends) {
		t.Fatalf("probes = %d, want %d", len(s.probes), len(s.backends))
	}
	for _, p := range s.probes {
		if !p.OK || p.TokPS <= 0 || p.TTFTms <= 0 {
			t.Fatalf("implausible probe: %+v", p)
		}
	}
}

func TestSnapshotCarriesAgentsAndProbes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewSource(5*time.Millisecond, 11)
		ch := make(chan core.Snapshot, 32)
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		go s.Run(ctx, ch)

		var got core.Snapshot
		deadline := time.Now().Add(2500 * time.Millisecond)
		for time.Now().Before(deadline) {
			select {
			case got = <-ch:
			default:
				time.Sleep(time.Millisecond)
				continue
			}
			if len(got.Agents) > 0 && len(got.Probes) > 0 {
				return // success
			}
		}
		t.Fatal("no agent/probe activity within deadline")
	})
}

// Every history sample carries the instant it was produced, so a
// sub-second cadence and a coalesced tick both stay on the real axis rather
// than being reconstructed from the sample's position.
func TestRingTimeKeepsRealInstants(t *testing.T) {
	base := time.Now()
	var ts []time.Time
	for i := range 5 {
		ts = ring(ts, base.Add(time.Duration(i)*500*time.Millisecond))
	}
	ts = ring(ts, base.Add(9*time.Second)) // 6.5s stall, ticks coalesced
	if want := base.Add(2 * time.Second); !ts[4].Equal(want) {
		t.Fatalf("stamp before the gap = %v, want %v", ts[4], want)
	}
	if want := base.Add(9 * time.Second); !ts[5].Equal(want) {
		t.Fatalf("stamp after the gap = %v, want %v", ts[5], want)
	}
}

func TestRingTimeSlidesAtHistoryLen(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	var ts []time.Time
	for i := range core.HistoryLen + 3 {
		ts = ring(ts, base.Add(time.Duration(i)*time.Second))
	}
	if len(ts) != core.HistoryLen {
		t.Fatalf("stamps = %d, want %d", len(ts), core.HistoryLen)
	}
	if want := base.Add(3 * time.Second); !ts[0].Equal(want) {
		t.Fatalf("oldest stamp = %v, want %v", ts[0], want)
	}
}

// Run drives the source from one goroutine while RecordAgent and ProbeAll
// arrive from others (ingest handlers, UI prober); hammer that exact mix so
// -race can prove the single s.mu contract holds.
func TestSourceConcurrentAccess(t *testing.T) {
	s := NewSource(2*time.Millisecond, 9)
	ch := make(chan core.Snapshot, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() { defer close(runDone); s.Run(ctx, ch) }()

	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		for range 200 {
			s.RecordAgent(core.AgentEvent{At: time.Now(), Agent: "x"})
			s.ProbeAll()
			time.Sleep(time.Millisecond)
		}
	}()

	<-workerDone
	s.mu.Lock()
	nAgents, nProbes := len(s.agents), len(s.probes)
	s.mu.Unlock()
	if nAgents == 0 {
		t.Fatal("RecordAgent produced no events")
	}
	if nProbes == 0 {
		t.Fatal("ProbeAll produced no samples")
	}
	cancel()
	var nSnap int
	for { // keep draining until Run has noticed cancellation and returned
		select {
		case <-ch:
			nSnap++
		case <-runDone:
			for len(ch) > 0 {
				<-ch
				nSnap++
			}
			if nSnap == 0 {
				t.Fatal("Run produced no snapshots")
			}
			return
		}
	}
}

// A probe wave runs on the UI goroutine, so where it lands among the ticks is
// the OS scheduler's to decide. The frame stream must not notice: a source
// probed on every tick and one never probed report the same values on the
// same tick, or the seed stops determining the run.
func TestProbeAllDoesNotMoveTheFrameStream(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	quiet := NewSource(time.Second, 11)
	noisy := NewSource(time.Second, 11)
	for i := range 24 {
		now := t0.Add(time.Duration(i) * time.Second)
		sq, sn := quiet.stepAt(now), noisy.stepAt(now)
		noisy.ProbeAll()
		if !reflect.DeepEqual(sq.Providers, sn.Providers) {
			t.Fatalf("probe wave moved the provider frame at tick %d:\n%+v\n%+v", i, sq.Providers, sn.Providers)
		}
		if !reflect.DeepEqual(sq.Sys, sn.Sys) {
			t.Fatalf("probe wave moved the vitals at tick %d:\n%+v\n%+v", i, sq.Sys, sn.Sys)
		}
	}
	if len(noisy.probes) == 0 {
		t.Fatal("ProbeAll produced no samples")
	}
}

// Two sources that ran the same number of waves draw the same probe numbers,
// whatever order the waves and the frames arrived in.
func TestProbeStreamIsSeparatePerSeed(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	early := NewSource(time.Second, 11)
	late := NewSource(time.Second, 11)
	for i := range 12 {
		early.stepAt(t0.Add(time.Duration(i) * time.Second))
		late.stepAt(t0.Add(time.Duration(2*i) * time.Second))
	}
	early.ProbeAll()
	for i := range 12 {
		late.stepAt(t0.Add(time.Duration(12+i) * time.Second))
	}
	late.ProbeAll()
	got, want := lastWave(late.probes), lastWave(early.probes)
	if len(want) == 0 {
		t.Fatal("ProbeAll produced no samples")
	}
	for addr, p := range want {
		q, ok := got[addr]
		if !ok {
			t.Fatalf("wave sample for %s missing in the other run", addr)
		}
		if p.TTFTms != q.TTFTms || p.TokPS != q.TokPS || p.Tokens != q.Tokens {
			t.Fatalf("wave sample for %s differs across interleavings: %+v vs %+v", addr, p, q)
		}
	}
}

// lastWave keys the newest samples of one wave by backend. A wave stamps
// every backend at one instant, so the newest At is the wave.
func lastWave(ps []core.ProbeSample) map[string]core.ProbeSample {
	var newest time.Time
	for _, p := range ps {
		if p.At.After(newest) {
			newest = p.At
		}
	}
	out := map[string]core.ProbeSample{}
	for _, p := range ps {
		if p.At.Equal(newest) {
			out[p.Addr] = p
		}
	}
	return out
}

// Demo mode shares the ingest recorder: events must stay newest-last the
// same way the live collector keeps them, or the agent feed renders a
// stale event last and eviction drops the wrong end.
func TestRecordAgentKeepsChronologicalOrder(t *testing.T) {
	s := NewSource(time.Second, 1)
	base := time.Now()
	order := []time.Duration{3 * time.Second, 7 * time.Second, 0, 5 * time.Second}
	for _, d := range order {
		s.RecordAgent(core.AgentEvent{At: base.Add(d), Agent: "a", OutputTokens: 1})
	}
	s.mu.Lock()
	agents := append([]core.AgentEvent(nil), s.agents...)
	s.mu.Unlock()
	for i := 1; i < len(agents); i++ {
		if agents[i].At.Before(agents[i-1].At) {
			t.Fatalf("agent ring not sorted at %d: %v", i, agents)
		}
	}
	if !agents[len(agents)-1].At.Equal(base.Add(7 * time.Second)) {
		t.Fatal("newest agent is not last")
	}
}

func TestRecordAgentEqualTimestampOrdersByAgent(t *testing.T) {
	s := NewSource(time.Second, 1)
	at := time.Unix(1_700_000_000, 0).UTC()
	s.RecordAgent(core.AgentEvent{At: at, Agent: "codex", ID: "2"})
	s.RecordAgent(core.AgentEvent{At: at, Agent: "claude", ID: "1"})
	s.RecordAgent(core.AgentEvent{At: at, Agent: "claude", ID: "0"})
	s.mu.Lock()
	agents := append([]core.AgentEvent(nil), s.agents...)
	s.mu.Unlock()
	want := [][2]string{{"claude", "0"}, {"claude", "1"}, {"codex", "2"}}
	if len(agents) != len(want) {
		t.Fatalf("agents = %d, want %d", len(agents), len(want))
	}
	for i, ev := range agents {
		if ev.Agent != want[i][0] || ev.ID != want[i][1] {
			t.Fatalf("agent %d = %s %s, want %s %s", i, ev.Agent, ev.ID, want[i][0], want[i][1])
		}
	}
}

// Demo mode shares the ingest recorder: a retried POST with the same id
// must not grow the feed, matching the live collector.
func TestRecordAgentSameIDKeptOnce(t *testing.T) {
	s := NewSource(time.Second, 1)
	ev := core.AgentEvent{At: time.Now(), ID: "turn-1", Agent: "coder", OutputTokens: 50}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			s.RecordAgent(ev)
		})
	}
	wg.Wait()
	s.mu.Lock()
	n := len(s.agents)
	s.mu.Unlock()
	if n != 1 {
		t.Fatalf("agents = %d, want 1", n)
	}

	s.RecordAgent(core.AgentEvent{At: time.Now(), ID: "turn-2", Agent: "coder", OutputTokens: 10})
	s.mu.Lock()
	n = len(s.agents)
	s.mu.Unlock()
	if n != 2 {
		t.Fatalf("distinct ids = %d, want 2", n)
	}

	s.RecordAgent(core.AgentEvent{At: time.Now(), Agent: "coder", OutputTokens: 1})
	s.RecordAgent(core.AgentEvent{At: time.Now(), Agent: "coder", OutputTokens: 1})
	s.mu.Lock()
	n = len(s.agents)
	s.mu.Unlock()
	if n != 4 {
		t.Fatalf("events without id = %d, want 4 total", n)
	}
}

// A retried POST arrives long after its first copy, by which time the feed has
// turned over: the id is out of the retained window, so only the ledger can
// answer the replay. Without it the same POST counted twice, which is the
// whole cost a sender pays for a lost 202.
func TestRecordAgentReplaySurvivesTheFeedTurningOver(t *testing.T) {
	s := NewSource(time.Second, 1)
	ev := core.AgentEvent{ID: "turn-1", Agent: "coder", OutputTokens: 50}
	if !s.RecordAgent(ev) {
		t.Fatal("first send was refused")
	}
	at := s.Now()
	for range core.AgentHistoryLen + 8 {
		at = at.Add(time.Second)
		s.RecordAgent(core.AgentEvent{At: at, Agent: "coder", OutputTokens: 1})
	}
	if core.HasAgentID(s.agents, "turn-1") {
		t.Fatal("turn-1 is still in the retained feed; the test proves nothing")
	}
	s.mu.Lock()
	before := len(s.agents)
	s.mu.Unlock()
	if s.RecordAgent(ev) {
		t.Fatal("a replay inside the horizon was retained a second time")
	}
	s.mu.Lock()
	after := len(s.agents)
	s.mu.Unlock()
	if after != before {
		t.Fatalf("feed = %d events after the replay, want %d", after, before)
	}
}

// The ledger is bounded by its horizon, not forever: past it an id is a new
// event again, so a stream posted under one Idempotency-Key hours later is
// counted as what it is.
func TestRecordAgentLedgerAgesOutWithTheHorizon(t *testing.T) {
	s := NewSource(time.Second, 1)
	ev := core.AgentEvent{ID: "turn-1", Agent: "coder", OutputTokens: 50}
	if !s.RecordAgent(ev) {
		t.Fatal("first send was refused")
	}
	s.stepAt(s.Now().Add(core.AgentIDHorizon + time.Second))
	if !s.RecordAgent(ev) {
		t.Fatal("an id past the horizon is still suppressed, so the ledger never ages out")
	}
}
