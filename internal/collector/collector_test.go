package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/procs"
	"github.com/maci0/toktop/internal/provider"
)

type fakeProvider struct {
	label string
	addr  string // defaults to "fake://<label>"
	kind  string // defaults to core.KindOllama
	m     *provider.Metrics
	err   error
}

func (f *fakeProvider) asProvider() provider.Provider {
	addr, kind := f.addr, f.kind
	if addr == "" {
		addr = "fake://" + f.label
	}
	if kind == "" {
		kind = core.KindOllama
	}
	return provider.Provider{Label: f.label, Addr: addr, Kind: kind, Poll: func(context.Context) (*provider.Metrics, error) {
		return f.m, f.err
	}}
}

func TestURLPort(t *testing.T) {
	cases := []struct {
		addr string
		want int
	}{
		{"http://127.0.0.1:11434", 11434},
		{"http://127.0.0.1:8081", 8081},
		{"https://example:8443", 8443},
		{"https://example.com:8443", 8443},
		{"http://127.0.0.1", 80},
		{"http://example.com", 80},
		{"https://example.com", 443},
		{"http://[::1]:8080", 8080},
		{"http://[::1]:11434", 11434},
		{"https://[::1]", 443},
		{"", 0},
		{"fake://x", 0},
		{"http://127.0.0.1:abc", 0},
		{"http://127.0.0.1:0", 0},
		{"http://127.0.0.1:65536", 0},
	}
	for _, tc := range cases {
		if got := urlPort(tc.addr); got != tc.want {
			t.Errorf("urlPort(%q) = %d, want %d", tc.addr, got, tc.want)
		}
	}
}

func TestRatesDeriveAndSmooth(t *testing.T) {
	c := New(nil, time.Second)
	now := time.Now()

	out, in := c.rates("p", &provider.Metrics{OutTotal: 100}, now)
	if out != 0 || in != 0 {
		t.Fatalf("first sample must seed baseline, got %v/%v", out, in)
	}
	out, _ = c.rates("p", &provider.Metrics{OutTotal: 400}, now.Add(time.Second))
	if out < 104 || out > 106 { // raw rate 300 tok/s smoothed with alpha .35 from 0
		t.Fatalf("smoothed rate = %v", out)
	}
	out, _ = c.rates("p", &provider.Metrics{OutTotal: 700}, now.Add(2*time.Second))
	if out <= 105 || out >= 300 {
		t.Fatalf("second smoothing step = %v", out)
	}
}

func TestRateCounterResetClampsToZero(t *testing.T) {
	c := New(nil, time.Second)
	c.rates("p", &provider.Metrics{OutTotal: 1000}, time.Now())
	out, _ := c.rates("p", &provider.Metrics{OutTotal: 5}, time.Now().Add(time.Second))
	if out != 0 {
		t.Fatalf("counter reset produced %v, want 0", out)
	}
}

// A sample arriving with zero elapsed time cannot produce a rate: 0/0 is NaN
// and n/0 is +Inf, and the EMA would carry either into every later sample.
// The rate must hold its prior value and the baseline must stay put so the
// next real interval still accounts for the tokens seen at the dup timestamp.
func TestRatesZeroElapsedHoldsPriorRate(t *testing.T) {
	c := New(nil, time.Second)
	now := time.Now()
	c.rates("p", &provider.Metrics{OutTotal: 100}, now)
	out, _ := c.rates("p", &provider.Metrics{OutTotal: 400}, now.Add(time.Second))

	held, heldIn := c.rates("p", &provider.Metrics{OutTotal: 900}, now.Add(time.Second))
	if math.IsNaN(held) || math.IsInf(held, 0) {
		t.Fatalf("zero-elapsed sample produced %v", held)
	}
	if held != out || heldIn != 0 {
		t.Fatalf("zero-elapsed sample moved rates to %v/%v, want %v/0", held, heldIn, out)
	}

	next, _ := c.rates("p", &provider.Metrics{OutTotal: 1000}, now.Add(2*time.Second))
	if next <= held { // delta 100 over the full 1s window, not 700
		t.Fatalf("rate after held sample = %v, want > %v", next, held)
	}
}

// Engines that publish instantaneous tok/s gauges (SGLang, TRT-LLM) must feed
// the rate directly instead of counter deltas.
func TestRatesHonorDirectThroughput(t *testing.T) {
	c := New(nil, time.Second)
	c.rates("p", &provider.Metrics{OutTotal: 10}, time.Now())
	out, _ := c.rates("p", &provider.Metrics{OutTotal: 10, DirectOutPS: 300, HasDirectOutPS: true}, time.Now().Add(time.Second))
	if out < 104 || out > 106 { // ema(0, 300)
		t.Fatalf("direct rate = %v", out)
	}
}

func TestEmitFirstThroughputGaugeSeedsRate(t *testing.T) {
	for _, tc := range []struct {
		name string
		has  bool
		want float64
	}{
		{"present", true, 300},
		{"absent", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fakeProvider{label: "engine", m: &provider.Metrics{
				OutTotal: 1000, InTotal: 2000,
				HasDirectOutPS: tc.has, DirectOutPS: 300,
			}}
			c := New([]provider.Provider{f.asProvider()}, time.Second)
			c.SetSysFn(nil)
			now := time.Unix(1000, 0)
			c.SetNow(func() time.Time { return now })
			ch := make(chan core.Snapshot, 1)
			c.emit(context.Background(), ch)
			first := (<-ch).Providers[0]
			if first.OutTokPS != tc.want || first.InTokPS != 0 {
				t.Fatalf("first rates = %v/%v, want %v/0", first.OutTokPS, first.InTokPS, tc.want)
			}
			if len(first.OutHist) != 1 || first.OutHist[0] != tc.want {
				t.Fatalf("first history = %v, want [%v]", first.OutHist, tc.want)
			}
			f.m.HasDirectOutPS = false
			f.m.OutTotal += 300
			f.m.InTotal += 100
			now = now.Add(time.Second)
			c.emit(context.Background(), ch)
			next := (<-ch).Providers[0]
			wantNext := tc.want*(1-emaAlpha) + 300*emaAlpha
			if next.OutTokPS != wantNext || next.InTokPS != 35 {
				t.Fatalf("next rates = %v/%v, want %v/35", next.OutTokPS, next.InTokPS, wantNext)
			}
		})
	}
}

func TestEmitHonorsZeroThroughputGauge(t *testing.T) {
	for _, tc := range []struct {
		name  string
		gauge string
		want  float64
	}{
		{"zero", "sglang:gen_throughput 0\n", 0},
		{"absent", "", 105},
		{"positive", "sglang:gen_throughput 200\n", 200},
		{"negative", "sglang:gen_throughput -1\n", 105},
		{"nan", "sglang:gen_throughput NaN\n", 105},
		{"infinite", "sglang:gen_throughput +Inf\n", 105},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var total atomic.Int64
			total.Store(100)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/metrics":
					fmt.Fprintf(w, "sglang:generation_tokens_total %d\n%s", total.Load(), tc.gauge)
				case "/v1/models":
					io.WriteString(w, `{"data":[]}`)
				case "/api/version":
					io.WriteString(w, `{"version":"test"}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			p := provider.NewOpenAICompat(srv.URL, "engine", core.KindSGLang)
			c := New([]provider.Provider{p}, time.Second)
			c.SetSysFn(nil)
			now := time.Unix(1000, 0)
			c.SetNow(func() time.Time { return now })
			out := make(chan core.Snapshot, 1)
			c.emit(context.Background(), out)
			<-out
			total.Store(400)
			now = now.Add(time.Second)
			c.emit(context.Background(), out)
			snap := <-out
			if got := snap.Providers[0].OutTokPS; got != tc.want {
				t.Fatalf("output rate = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHistoryRingCap(t *testing.T) {
	r := &timedRing{}
	t0 := time.Now()
	for i := range core.HistoryLen + 10 {
		r.push(float64(i), t0.Add(time.Duration(i)*time.Second), time.Second)
	}
	vals := r.copy()
	if len(vals) != core.HistoryLen {
		t.Fatalf("ring len = %d, want %d", len(vals), core.HistoryLen)
	}
	if vals[len(vals)-1] != float64(core.HistoryLen+9) {
		t.Fatal("newest sample lost")
	}
	if vals[0] != 10 { // oldest slid off in order: 0..9 evicted, 10 is now first
		t.Fatal("ring order broken after wrap")
	}
	// oldest timestamp must slide forward one cadence per evicted sample
	wantT0 := t0.Add(10 * time.Second)
	if !r.t0.Equal(wantT0) {
		t.Fatalf("t0 = %v, want %v", r.t0, wantT0)
	}
}

// Once warm, push must stop allocating: the ring reuses one fixed buffer for
// its lifetime instead of sliding a slice (which reallocs on every push).
func TestHistoryRingDoesNotGrowBuffer(t *testing.T) {
	r := &timedRing{}
	for i := range core.HistoryLen * 3 {
		r.push(float64(i), time.Unix(int64(i), 0), time.Second)
	}
	if cap(r.buf) != core.HistoryLen {
		t.Fatalf("buffer capacity = %d, want exactly %d", cap(r.buf), core.HistoryLen)
	}
	vals := r.copy()
	if want := float64(core.HistoryLen * 2); vals[0] != want {
		t.Fatalf("oldest sample = %v, want %v", vals[0], want)
	}
}

// Pushes are not guaranteed one cadence apart: a scrape can take up to
// PollTimeout and coalesced ticks widen gaps further. t0 must re-anchor to
// the real push time (newest sample at now) or the chart's absolute time
// axis drifts into the past by every lost interval, permanently.
func TestHistoryRingRetimesAfterGap(t *testing.T) {
	r := &timedRing{}
	base := time.Unix(1_000_000, 0)
	for i := range 5 {
		r.push(float64(i), base.Add(time.Duration(i)*time.Second), time.Second)
	}
	r.push(5, base.Add(9*time.Second), time.Second) // 4s stall, tick coalesced
	want := base.Add(9 * time.Second).Add(-5 * time.Second)
	if !r.t0.Equal(want) {
		t.Fatalf("t0 after gap = %v, want %v", r.t0, want)
	}
}

func TestEmitCopiesProviderKind(t *testing.T) {
	fp := fakeProvider{label: "v", kind: core.KindVLLM, m: &provider.Metrics{}}
	ch := make(chan core.Snapshot, 1)
	c := New([]provider.Provider{fp.asProvider()}, time.Hour)
	c.sysFn = func() core.SysSample { return core.SysSample{} }
	go c.emit(context.Background(), ch)

	select {
	case snap := <-ch:
		if len(snap.Providers) != 1 {
			t.Fatalf("providers = %d, want 1", len(snap.Providers))
		}
		if snap.Providers[0].Kind != core.KindVLLM {
			t.Fatalf("Kind = %q, want %q", snap.Providers[0].Kind, core.KindVLLM)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("emit did not produce a snapshot")
	}
}

func TestEmitSnapshotShape(t *testing.T) {
	fp := fakeProvider{label: "testprov", m: &provider.Metrics{
		Models: []core.ModelInfo{{Name: "m1"}}, Running: 2,
	}}
	ch := make(chan core.Snapshot, 1)
	c := New([]provider.Provider{fp.asProvider()}, time.Hour)
	c.sysFn = func() core.SysSample {
		return core.SysSample{MemTotal: 100, MemUsed: 50, Load1: 0.5,
			Temps: []core.TempReading{{Label: "package", MilliC: 45000}}}
	}
	done := make(chan struct{})
	go func() { c.emit(context.Background(), ch); close(done) }()

	select {
	case snap := <-ch:
		if snap.Sys == nil || snap.Sys.MemUsed != 50 || snap.Sys.MemTotal != 100 ||
			snap.Sys.Load1 != 0.5 || len(snap.Sys.Temps) != 1 {
			t.Fatalf("sys sample missing from snapshot: %+v", snap.Sys)
		}
		if len(snap.Providers) != 1 {
			t.Fatalf("providers = %d, want 1", len(snap.Providers))
		}
		p := snap.Providers[0]
		if p.Label != "testprov" || p.Kind != core.KindOllama || !p.OK || p.Running != 2 {
			t.Fatalf("provider = %+v, want testprov/ollama ok running=2", p)
		}
		if len(p.Models) != 1 || p.Models[0].Name != "m1" {
			t.Fatalf("models = %+v, want [m1]", p.Models)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("emit did not produce a snapshot")
	}
	<-done
}

// Host-vitals sampling happens off the emit path: the background poller
// fills the cache, and a warm emit must never invoke the (potentially
// seconds-slow) sampler itself.
func TestEmitUsesCachedSysSample(t *testing.T) {
	c := New(nil, time.Hour)
	var calls atomic.Int32
	c.SetSysFn(func() core.SysSample {
		calls.Add(1)
		return core.SysSample{MemTotal: 7}
	})
	ctx := t.Context()
	c.startSysPoller(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for c.sysSnapshot() == nil {
		if time.Now().After(deadline) {
			t.Fatal("background poller never cached a sample")
		}
		time.Sleep(time.Millisecond)
	}
	before := calls.Load()

	ch := make(chan core.Snapshot, 1)
	done := make(chan struct{})
	go func() { defer close(done); c.emit(context.Background(), ch) }()
	var snap core.Snapshot
	select {
	case snap = <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("emit did not produce a snapshot")
	}
	<-done
	if snap.Sys == nil || snap.Sys.MemTotal != 7 {
		t.Fatalf("cached sys sample missing from snapshot: %+v", snap.Sys)
	}
	if got := calls.Load(); got != before {
		t.Fatalf("emit invoked the sampler inline (%d -> %d calls)", before, got)
	}
}

func frozenCollector(t *testing.T, frozen time.Time, providers []provider.Provider) *Collector {
	t.Helper()
	c := New(providers, time.Second)
	c.SetNow(func() time.Time { return frozen })
	c.SetSysFn(func() core.SysSample { return core.SysSample{MemTotal: 8, MemUsed: 3} })
	c.procFn = func() []procs.Info { return nil }
	return c
}

// Snapshot timestamps and uptime come from the injected clock, not a
// wall-clock read inside emit, so a replay from the same instant matches.
func TestEmitFollowsInjectedClock(t *testing.T) {
	frozen := time.Unix(1_700_000_000, 0).UTC()
	c := frozenCollector(t, frozen, []provider.Provider{(&fakeProvider{label: "x", m: &provider.Metrics{OutTotal: 10}}).asProvider()})
	ch := make(chan core.Snapshot, 1)
	c.emit(context.Background(), ch)
	snap := <-ch
	if !snap.At.Equal(frozen) {
		t.Fatalf("At = %v, want %v", snap.At, frozen)
	}
	if snap.Uptime != 0 {
		t.Fatalf("Uptime = %v, want 0 with a frozen clock", snap.Uptime)
	}
}

func TestEmitDeterministicUnderSameClock(t *testing.T) {
	frozen := time.Unix(1_700_000_000, 0).UTC()
	fp := fakeProvider{label: "ollama", m: &provider.Metrics{OutTotal: 100, InTotal: 20, Running: 1}}
	chA, chB := make(chan core.Snapshot, 1), make(chan core.Snapshot, 1)
	frozenCollector(t, frozen, []provider.Provider{fp.asProvider()}).emit(context.Background(), chA)
	frozenCollector(t, frozen, []provider.Provider{fp.asProvider()}).emit(context.Background(), chB)
	sa, sb := <-chA, <-chB
	if !reflect.DeepEqual(sa, sb) {
		t.Fatalf("same clock, same providers diverged:\n%+v\n%+v", sa, sb)
	}
}

func TestRecordAgentZeroAtUsesClock(t *testing.T) {
	frozen := time.Unix(1_700_000_000, 0).UTC()
	c := frozenCollector(t, frozen, nil)
	c.RecordAgent(core.AgentEvent{Agent: "x", OutputTokens: 1})
	if len(c.agents) != 1 || !c.agents[0].At.Equal(frozen) {
		t.Fatalf("At = %v, want injected %v", c.agents, frozen)
	}
}

func TestRunWaitsForPollers(t *testing.T) {
	for _, first := range []string{"sys", "proc"} {
		t.Run(first, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				c := New(nil, time.Second)
				sysRelease, procRelease := make(chan struct{}), make(chan struct{})
				procStarted, sysStarted := make(chan struct{}), make(chan struct{})
				var procOnce sync.Once
				c.procFn = func() []procs.Info {
					procOnce.Do(func() { close(procStarted) })
					<-procRelease
					return nil
				}
				calls := 0
				c.SetSysFn(func() core.SysSample {
					calls++
					if calls == 2 {
						close(sysStarted)
						<-sysRelease
					}
					return core.SysSample{}
				})
				done := make(chan struct{})
				go func() {
					defer close(done)
					c.Run(ctx, make(chan core.Snapshot, 4))
				}()
				<-procStarted
				<-sysStarted
				cancel()
				synctest.Wait()
				select {
				case <-done:
					t.Error("Run returned with both pollers still sampling")
				default:
				}
				firstRelease, lastRelease := sysRelease, procRelease
				if first == "proc" {
					firstRelease, lastRelease = procRelease, sysRelease
				}
				close(firstRelease)
				synctest.Wait()
				select {
				case <-done:
					t.Error("Run returned with one poller still sampling")
				default:
				}
				close(lastRelease)
				<-done
			})
		})
	}
}

// Run is the production loop: it must start the background proc+sys pollers,
// emit one snapshot per interval (including the immediate first frame) and
// stop promptly on ctx cancellation without stranding the emit goroutine.
func TestRunEmitsUntilCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fp := fakeProvider{label: "run", m: &provider.Metrics{
			OutTotal: 1, Models: []core.ModelInfo{{Name: "m"}},
		}}
		ch := make(chan core.Snapshot)
		ctx, cancel := context.WithCancel(t.Context())
		c := New([]provider.Provider{fp.asProvider()}, 5*time.Millisecond)
		c.SetSysFn(func() core.SysSample { return core.SysSample{MemTotal: 9} })
		c.procFn = func() []procs.Info { return nil }

		done := make(chan struct{})
		go func() { defer close(done); c.Run(ctx, ch) }()

		for i := range 2 {
			select {
			case snap := <-ch:
				if len(snap.Providers) != 1 || snap.Providers[0].Label != "run" {
					t.Fatalf("snapshot %d = %+v", i, snap.Providers)
				}
				if snap.Sys == nil || snap.Sys.MemTotal != 9 {
					t.Fatalf("snapshot %d missing sys vitals: %+v", i, snap.Sys)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Run stopped emitting snapshots")
			}
		}
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return after cancel")
		}
	})
}

// Run warms the vitals cache before its first emit, so the (potentially
// seconds-slow) sampler - GPU vendor CLIs especially - never runs inside
// emit's c.mu critical section: ingest handlers and probe launches must stay
// live throughout startup.
func TestRunWarmsSysCacheBeforeFirstEmit(t *testing.T) {
	c := New(nil, time.Hour)
	release := make(chan struct{})
	var calls atomic.Int32
	c.SetSysFn(func() core.SysSample {
		calls.Add(1)
		<-release // hold the sampler as long as a hung vendor CLI would
		return core.SysSample{MemTotal: 5}
	})
	// Run waits for the proc poller after cancel. The live Windows CIM
	// listing takes seconds, which is longer than this test's shutdown wait.
	c.procFn = func() []procs.Info { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan core.Snapshot)
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx, ch) }()

	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 { // warm-up sampling is now in flight
		if time.Now().After(deadline) {
			t.Fatal("Run never invoked the sys sampler")
		}
		time.Sleep(time.Millisecond)
	}

	rec := make(chan struct{})
	go func() { c.RecordAgent(core.AgentEvent{At: time.Now(), Agent: "liveness"}); close(rec) }()
	select {
	case <-rec:
	case <-time.After(2 * time.Second):
		t.Fatal("RecordAgent blocked behind Run's warm-up sampling")
	}

	close(release) // let warm-up finish so Run can reach its first emit
	select {
	case snap := <-ch:
		if snap.Sys == nil || snap.Sys.MemTotal != 5 {
			t.Fatalf("first snapshot missing warmed vitals: %+v", snap.Sys)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not emit after warm-up")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestAgentEventRing(t *testing.T) {
	c := New(nil, time.Second)
	base := time.Unix(1_700_000_000, 0)
	n := core.AgentHistoryLen + 5
	for i := range n {
		c.RecordAgent(core.AgentEvent{
			At:           base.Add(time.Duration(i) * time.Second),
			Agent:        "a",
			OutputTokens: int64(i),
		})
	}
	if len(c.agents) != core.AgentHistoryLen {
		t.Fatalf("agent ring = %d, want %d", len(c.agents), core.AgentHistoryLen)
	}
	if c.agents[0].OutputTokens != 5 {
		t.Fatalf("oldest kept = %d, want 5 (first 5 evicted)", c.agents[0].OutputTokens)
	}
	if want := int64(n - 1); c.agents[len(c.agents)-1].OutputTokens != want {
		t.Fatalf("newest kept = %d, want %d", c.agents[len(c.agents)-1].OutputTokens, want)
	}
	for i := 1; i < len(c.agents); i++ {
		if !c.agents[i].At.After(c.agents[i-1].At) {
			t.Fatalf("ring order broken at %d: %v", i, c.agents)
		}
	}
}

// The probe ring evicts the oldest sample once full, the same way the agent
// ring does: charts and the "last probe" readout assume newest-last and a
// bounded window.
func TestProbeRingCap(t *testing.T) {
	c := New(nil, time.Second)
	base := time.Unix(1_700_000_000, 0)
	n := core.ProbeHistoryLen + 5
	for i := range n {
		c.RecordProbe(core.ProbeSample{
			At:    base.Add(time.Duration(i) * time.Second),
			TokPS: float64(i),
		})
	}
	if len(c.probes) != core.ProbeHistoryLen {
		t.Fatalf("probe ring = %d, want %d", len(c.probes), core.ProbeHistoryLen)
	}
	if c.probes[0].TokPS != 5 {
		t.Fatalf("oldest kept = %v, want 5 (first 5 evicted)", c.probes[0].TokPS)
	}
	if want := float64(n - 1); c.probes[len(c.probes)-1].TokPS != want {
		t.Fatalf("newest kept = %v, want %v", c.probes[len(c.probes)-1].TokPS, want)
	}
	for i := 1; i < len(c.probes); i++ {
		if !c.probes[i].At.After(c.probes[i-1].At) {
			t.Fatalf("ring order broken at %d: %v", i, c.probes)
		}
	}
}

func TestProbeAllSkipsWhenNoModelKnown(t *testing.T) {
	c := New([]provider.Provider{(&fakeProvider{label: "x"}).asProvider()}, time.Second)
	c.ProbeAll() // must not panic or block; no model known yet
	if len(c.probes) != 0 {
		t.Fatalf("unexpected probes: %d", len(c.probes))
	}
}

// Probes complete concurrently and can finish out of launch order; the
// retained ring must still be chronological or the probe chart anchors its
// window on a stale sample and drops newer ones.
func TestRecordProbeKeepsChronologicalOrder(t *testing.T) {
	c := New(nil, time.Second)
	base := time.Now()
	order := []time.Duration{time.Second, 5 * time.Second, 2 * time.Second, 0, 9 * time.Second}
	for _, d := range order {
		c.RecordProbe(core.ProbeSample{At: base.Add(d), TokPS: 1})
	}
	for i := 1; i < len(c.probes); i++ {
		if c.probes[i].At.Before(c.probes[i-1].At) {
			t.Fatalf("probe ring not sorted at %d: %v", i, c.probes)
		}
	}
	if !c.probes[len(c.probes)-1].At.Equal(base.Add(9 * time.Second)) {
		t.Fatal("newest probe is not last")
	}
}

// Equal timestamps must not fall back to arrival order: concurrent probe
// completions would then shuffle the ring across replays of the same clock.
func TestRecordProbeEqualTimestampOrdersByAddr(t *testing.T) {
	c := New(nil, time.Second)
	at := time.Unix(1_700_000_000, 0).UTC()
	c.RecordProbe(core.ProbeSample{At: at, Addr: "http://127.0.0.1:8000", Model: "b"})
	c.RecordProbe(core.ProbeSample{At: at, Addr: "http://127.0.0.1:11434", Model: "a"})
	c.RecordProbe(core.ProbeSample{At: at, Addr: "http://127.0.0.1:8000", Model: "a"})
	want := [][2]string{
		{"http://127.0.0.1:11434", "a"},
		{"http://127.0.0.1:8000", "a"},
		{"http://127.0.0.1:8000", "b"},
	}
	if len(c.probes) != len(want) {
		t.Fatalf("probes = %d, want %d", len(c.probes), len(want))
	}
	for i, p := range c.probes {
		if p.Addr != want[i][0] || p.Model != want[i][1] {
			t.Fatalf("probe %d = %s %s, want %s %s", i, p.Addr, p.Model, want[i][0], want[i][1])
		}
	}
}

func TestRecordAgentEqualTimestampOrdersByAgent(t *testing.T) {
	c := New(nil, time.Second)
	at := time.Unix(1_700_000_000, 0).UTC()
	c.RecordAgent(core.AgentEvent{At: at, Agent: "codex", ID: "2"})
	c.RecordAgent(core.AgentEvent{At: at, Agent: "claude", ID: "1"})
	c.RecordAgent(core.AgentEvent{At: at, Agent: "claude", ID: "0"})
	want := [][2]string{{"claude", "0"}, {"claude", "1"}, {"codex", "2"}}
	if len(c.agents) != len(want) {
		t.Fatalf("agents = %d, want %d", len(c.agents), len(want))
	}
	for i, ev := range c.agents {
		if ev.Agent != want[i][0] || ev.ID != want[i][1] {
			t.Fatalf("agent %d = %s %s, want %s %s", i, ev.Agent, ev.ID, want[i][0], want[i][1])
		}
	}
}

// Agent events arrive over the ingest endpoint from senders whose clocks
// disagree, so arrival order is not time order; the retained slice must
// still be chronological or the agent feed renders a stale event last and
// eviction drops the wrong end.
func TestRecordAgentKeepsChronologicalOrder(t *testing.T) {
	c := New(nil, time.Second)
	base := time.Now()
	order := []time.Duration{3 * time.Second, 7 * time.Second, 0, 5 * time.Second}
	for _, d := range order {
		c.RecordAgent(core.AgentEvent{At: base.Add(d), Agent: "a", OutputTokens: 1})
	}
	for i := 1; i < len(c.agents); i++ {
		if c.agents[i].At.Before(c.agents[i-1].At) {
			t.Fatalf("agent ring not sorted at %d: %v", i, c.agents)
		}
	}
	if !c.agents[len(c.agents)-1].At.Equal(base.Add(7 * time.Second)) {
		t.Fatal("newest agent is not last")
	}
}

// A retried ingest POST (lost 202, replayed NDJSON) carries the same id;
// the retained feed must stay a single event so rates do not double-count.
func TestRecordAgentSameIDKeptOnce(t *testing.T) {
	c := New(nil, time.Second)
	ev := core.AgentEvent{At: time.Now(), ID: "turn-1", Agent: "coder", OutputTokens: 50}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			c.RecordAgent(ev)
		})
	}
	wg.Wait()
	if len(c.agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(c.agents))
	}

	c.RecordAgent(core.AgentEvent{At: time.Now(), ID: "turn-2", Agent: "coder", OutputTokens: 10})
	if len(c.agents) != 2 {
		t.Fatalf("distinct ids = %d, want 2", len(c.agents))
	}

	c.RecordAgent(core.AgentEvent{At: time.Now(), Agent: "coder", OutputTokens: 1})
	c.RecordAgent(core.AgentEvent{At: time.Now(), Agent: "coder", OutputTokens: 1})
	if len(c.agents) != 4 {
		t.Fatalf("events without id = %d, want 4 total", len(c.agents))
	}
}

// emit launches one goroutine per provider and waits; they must all return
// so the leak profile stays empty. A leaked poll goroutine would show up
// here after GC, the same way a production dashboard would accumulate them.
func TestEmitDoesNotLeakGoroutines(t *testing.T) {
	fp := fakeProvider{label: "x", m: &provider.Metrics{
		OutTotal: 1, Models: []core.ModelInfo{{Name: "m"}},
	}}
	c := New([]provider.Provider{fp.asProvider()}, time.Hour)
	c.SetSysFn(func() core.SysSample { return core.SysSample{MemTotal: 1} })
	ch := make(chan core.Snapshot, 1)
	c.emit(context.Background(), ch)
	<-ch
	runtime.GC()
	runtime.GC()
	p := pprof.Lookup("goroutineleak")
	if p == nil {
		t.Fatal("goroutineleak profile not available")
	}
	var buf strings.Builder
	if err := p.WriteTo(&buf, 1); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "goroutine ") {
		t.Fatalf("leaked goroutines after emit:\n%s", buf.String())
	}
}

// The id window is the retained ring: once an event is evicted, its id may
// be reused rather than pinning a key forever.
func TestRecordAgentReusesEvictedID(t *testing.T) {
	// Explicit timestamps, not time.Now(): a coarse clock (Windows ticks at
	// milliseconds) hands out the same instant to the whole run, and equal
	// timestamps order by id, which would sort "old" newest and pin it.
	base := time.Unix(1_700_000_000, 0).UTC()
	c := New(nil, time.Second)
	c.RecordAgent(core.AgentEvent{At: base, ID: "old", Agent: "a"})
	for i := range core.AgentHistoryLen {
		c.RecordAgent(core.AgentEvent{
			At:    base.Add(time.Duration(i+1) * time.Millisecond),
			ID:    fmt.Sprintf("n%d", i),
			Agent: "a",
		})
	}
	if core.HasAgentID(c.agents, "old") {
		t.Fatal("old id still retained after eviction")
	}
	c.RecordAgent(core.AgentEvent{
		At:           base.Add((core.AgentHistoryLen + 1) * time.Millisecond),
		ID:           "old",
		Agent:        "a",
		OutputTokens: 7,
	})
	if !core.HasAgentID(c.agents, "old") {
		t.Fatal("evicted id must be reusable")
	}
}

// Two instances of the same engine kind share a display label ("llama.cpp"
// for every llama.cpp server); their rate baselines and histories must be
// keyed by endpoint so counters never mix across engines.
func TestPerProviderStateKeyedByEndpoint(t *testing.T) {
	m1 := &provider.Metrics{OutTotal: 100, Models: []core.ModelInfo{{Name: "m"}}}
	m2 := &provider.Metrics{OutTotal: 500, Models: []core.ModelInfo{{Name: "m"}}}
	ch := make(chan core.Snapshot, 1)
	now := time.Unix(1_700_000_000, 0).UTC()
	c := New([]provider.Provider{(&fakeProvider{label: core.KindLlamaCPP, addr: "http://127.0.0.1:8080", m: m1}).asProvider(), (&fakeProvider{label: core.KindLlamaCPP, addr: "http://127.0.0.1:8081", m: m2}).asProvider()}, time.Second)
	c.SetNow(func() time.Time { return now })

	get := func() map[string]float64 {
		c.emit(context.Background(), ch)
		snap := <-ch
		out := map[string]float64{}
		for _, p := range snap.Providers {
			out[p.Addr] = p.OutTokPS
		}
		return out
	}

	get() // seed both baselines
	now = now.Add(time.Second)
	m1.OutTotal = 200 // only engine :8080 generated tokens since emit #1

	rates := get()
	// 100 tok over 1s, EMA from 0 with alpha 0.35.
	if rates["http://127.0.0.1:8080"] != 35 {
		t.Fatalf(":8080 rate = %v, want 35 (its own counter moved)", rates)
	}
	if rates["http://127.0.0.1:8081"] != 0 {
		t.Fatalf(":8081 rate = %v, want 0 (its counter did not move)", rates)
	}
}

// A duplicate endpoint (the same --add URL twice, or discovery plus an
// explicit attach of one engine) must collapse to one provider: rate
// baselines, histories and probe state are keyed by endpoint, so a second
// entry with the same key would share its baseline, double the UI aggregate
// and push two history samples per emission.
func TestDuplicateEndpointsCollapsed(t *testing.T) {
	m := &provider.Metrics{OutTotal: 100, Models: []core.ModelInfo{{Name: "m"}}}
	ch := make(chan core.Snapshot, 1)
	now := time.Unix(1_700_000_000, 0).UTC()
	dup := (&fakeProvider{label: core.KindLlamaCPP, addr: "http://127.0.0.1:8080", m: m}).asProvider()
	other := (&fakeProvider{label: core.KindLlamaCPP, addr: "http://127.0.0.1:8081", m: m}).asProvider()
	c := New([]provider.Provider{dup, dup, other}, time.Second)
	c.SetNow(func() time.Time { return now })

	c.emit(context.Background(), ch) // seed baseline
	snap := <-ch
	if len(snap.Providers) != 2 {
		t.Fatalf("providers = %d, want 2 (duplicate endpoint collapsed)", len(snap.Providers))
	}
	now = now.Add(time.Second)
	m.OutTotal = 200 // one engine generated tokens since emit #1

	c.emit(context.Background(), ch)
	snap = <-ch
	rates := map[string]float64{}
	for _, p := range snap.Providers {
		rates[p.Addr] += p.OutTokPS
	}
	if rates["http://127.0.0.1:8080"] != 35 { // 100 tok over 1s, EMA alpha .35 from 0
		t.Fatalf(":8080 aggregate rate = %v, want 35 (single poll, no double count)", rates)
	}
	if len(snap.Providers[0].OutHist) != 2 {
		t.Fatalf("history samples = %d, want 2 (one per emission)", len(snap.Providers[0].OutHist))
	}
}

func TestEmitProcessAttributionRequiresLocalHost(t *testing.T) {
	for _, tc := range []struct {
		addr string
		pid  int
	}{
		{"http://127.0.0.1:11434", 42},
		{"http://127.0.0.2:11434", 42},
		{"http://[::1]:11434", 42},
		{"http://localhost:11434", 42},
		{"http://192.0.2.1:11434", 0},
		{"http://[2001:db8::1]:11434", 0},
		{"http://engine.example:11434", 0},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			p := fakeProvider{label: "engine", addr: tc.addr, m: &provider.Metrics{}}
			c := New([]provider.Provider{p.asProvider()}, time.Second)
			c.SetSysFn(func() core.SysSample { return core.SysSample{} })
			c.procCache = []procs.Info{{PID: 42, RSS: 1000, CPUPct: 12.5, PortHint: 11434}}
			ch := make(chan core.Snapshot, 1)
			c.emit(context.Background(), ch)
			got := (<-ch).Providers[0]
			if got.PID != tc.pid {
				t.Errorf("PID = %d, want %d", got.PID, tc.pid)
			}
			if tc.pid == 0 && (got.ProcRSS != 0 || got.ProcCPU != 0) {
				t.Errorf("remote provider inherited local process metrics: RSS %d, CPU %v", got.ProcRSS, got.ProcCPU)
			}
		})
	}
}

func TestEmitAttachesProcessByListenPort(t *testing.T) {
	ollama := fakeProvider{label: "ollama", addr: "http://127.0.0.1:11434", m: &provider.Metrics{
		Models: []core.ModelInfo{{Name: "m"}},
	}}
	vllm := fakeProvider{label: "vllm", addr: "http://127.0.0.1:8000", m: &provider.Metrics{
		Models: []core.ModelInfo{{Name: "m"}},
	}}
	ch := make(chan core.Snapshot, 1)
	c := New([]provider.Provider{ollama.asProvider(), vllm.asProvider()}, time.Hour)
	c.procCache = []procs.Info{
		{PID: 42, RSS: 1000, CPUPct: 12.5, PortHint: 11434},
		{PID: 43, RSS: 999, CPUPct: 1, PortHint: 11434}, // same port: first wins
		{PID: 99, RSS: 2000, CPUPct: 3, PortHint: 9999}, // unmatched
	}
	c.emit(context.Background(), ch)
	snap := <-ch
	if len(snap.Providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(snap.Providers))
	}
	got := map[string]core.ProviderSnapshot{}
	for _, p := range snap.Providers {
		got[p.Addr] = p
	}
	o := got["http://127.0.0.1:11434"]
	if o.PID != 42 || o.ProcRSS != 1000 || o.ProcCPU != 12.5 {
		t.Errorf("ollama process = pid %d rss %d cpu %v, want 42/1000/12.5 (first sample)",
			o.PID, o.ProcRSS, o.ProcCPU)
	}
	v := got["http://127.0.0.1:8000"]
	if v.PID != 0 || v.ProcRSS != 0 || v.ProcCPU != 0 {
		t.Errorf("unmatched vllm process = pid %d rss %d cpu %v, want zeros",
			v.PID, v.ProcRSS, v.ProcCPU)
	}
}

// Snapshots must not alias the vitals cache: the UI goroutine reads Sys while
// the poller replaces it, and Drivers/Temps/GPUs/NPUs are reference fields.
func TestEmitSysSampleDetachedFromCache(t *testing.T) {
	c := New(nil, time.Hour)
	c.SetSysFn(func() core.SysSample {
		return core.SysSample{
			MemTotal: 10,
			Drivers:  map[string]string{"nvidia": "1"},
			Temps:    []core.TempReading{{Label: "cpu", MilliC: 40000}},
			GPUs:     []core.GPUDevice{{Name: "a", UtilPct: 1}},
			NPUs:     []string{"npu"},
		}
	})
	c.sampleSys(true)
	ch := make(chan core.Snapshot, 1)
	c.emit(context.Background(), ch)
	snap := <-ch
	if snap.Sys == nil {
		t.Fatal("missing sys sample")
	}
	c.sysMu.Lock()
	c.sysCache.MemTotal = 99
	c.sysCache.Drivers["nvidia"] = "mutated"
	c.sysCache.Temps[0].MilliC = 1
	c.sysCache.GPUs[0].UtilPct = 99
	c.sysCache.NPUs[0] = "x"
	c.sysMu.Unlock()
	if snap.Sys.MemTotal != 10 || snap.Sys.Drivers["nvidia"] != "1" ||
		snap.Sys.Temps[0].MilliC != 40000 || snap.Sys.GPUs[0].UtilPct != 1 ||
		snap.Sys.NPUs[0] != "npu" {
		t.Fatalf("published sys aliased the cache: %+v", snap.Sys)
	}
}

func TestProcSnapshotDetachedFromCache(t *testing.T) {
	c := New(nil, time.Hour)
	c.procCache = []procs.Info{{PID: 1, RSS: 10}}
	got := c.procSnapshot()
	got[0].PID = 99
	if c.procCache[0].PID != 1 {
		t.Fatalf("procSnapshot aliased the cache: %+v", c.procCache)
	}
}

// A provider failing its very first poll has no history rings yet; emit must
// still include it (with the error surfaced) instead of dereferencing nil.
func TestEmitSurvivesFirstPollError(t *testing.T) {
	fp := fakeProvider{label: "dead", err: errors.New("connection refused")}
	ch := make(chan core.Snapshot, 1)
	c := New([]provider.Provider{fp.asProvider()}, time.Hour)
	done := make(chan struct{})
	go func() { defer close(done); c.emit(context.Background(), ch) }()
	select {
	case snap := <-ch:
		if len(snap.Providers) != 1 {
			t.Fatalf("providers = %d, want 1", len(snap.Providers))
		}
		p := snap.Providers[0]
		if p.OK || p.Err == "" {
			t.Fatalf("expected failed provider with error, got ok=%v err=%q", p.OK, p.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("emit did not produce a snapshot")
	}
	<-done
}

// Poll returning (nil, nil) used to dereference Metrics and panic, killing
// the dashboard process. Treat it as a failed poll instead.
func TestEmitSurvivesNilMetrics(t *testing.T) {
	fp := fakeProvider{label: "empty"}
	ch := make(chan core.Snapshot, 1)
	c := New([]provider.Provider{fp.asProvider()}, time.Hour)
	c.emit(context.Background(), ch)
	snap := <-ch
	if len(snap.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(snap.Providers))
	}
	p := snap.Providers[0]
	if p.OK || p.Err == "" {
		t.Fatalf("nil metrics without error: ok=%v err=%q", p.OK, p.Err)
	}
}

// emit's per-provider timeout must be a child of the Run context: a stalled
// scrape must not hold shutdown for PollTimeout after cancel.
func TestEmitCancelsPollsOnContext(t *testing.T) {
	started := make(chan struct{})
	fp := &blockingProvider{started: started}
	ch := make(chan core.Snapshot, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New([]provider.Provider{fp.asProvider()}, time.Hour)
	c.SetSysFn(func() core.SysSample {
		t.Error("host sampling started after cancellation")
		return core.SysSample{}
	})
	done := make(chan struct{})
	go func() { defer close(done); c.emit(ctx, ch) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("poll never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("emit did not return after ctx cancel")
	}
}

type blockingProvider struct {
	started chan struct{}
}

func (b *blockingProvider) asProvider() provider.Provider {
	return provider.Provider{Label: "block", Addr: "fake://block", Kind: core.KindOllama, Poll: func(ctx context.Context) (*provider.Metrics, error) {
		close(b.started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
}

// The ingest handlers, the UI prober and the emit loop all touch the
// collector's shared maps concurrently in production; hammer them together so
// -race can prove the locking holds.
func TestConcurrentRecordProbeEmit(t *testing.T) {
	fp := fakeProvider{label: "c", m: &provider.Metrics{
		OutTotal: 5, Models: []core.ModelInfo{{Name: "m"}},
	}}
	ch := make(chan core.Snapshot, 1)
	c := New([]provider.Provider{fp.asProvider()}, time.Hour)
	c.SetSysFn(func() core.SysSample { return core.SysSample{MemTotal: 1} })

	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Go(func() { // ingest server handlers appending events and probes
		for {
			select {
			case <-stop:
				return
			default:
				c.RecordAgent(core.AgentEvent{At: time.Now(), Agent: "a"})
				c.RecordProbe(core.ProbeSample{At: time.Now(), OK: true})
				time.Sleep(time.Millisecond)
			}
		}
	})
	wg.Go(func() { // UI 'p' keypresses; ProbeAll reads lastModel under mu
		for {
			select {
			case <-stop:
				return
			default:
				c.ProbeAll()
				time.Sleep(time.Millisecond)
			}
		}
	})

	emitDone := make(chan struct{})
	wg.Go(func() { // poll loop emitting snapshots
		defer close(emitDone)
		for {
			select {
			case <-stop:
				return
			default:
				c.emit(context.Background(), ch)
			}
		}
	})

	time.Sleep(200 * time.Millisecond)
	close(stop)
	var nSnap int
	for { // drain until the emit goroutine has exited its final send
		select {
		case <-ch:
			nSnap++
		case <-emitDone:
			for len(ch) > 0 {
				<-ch
				nSnap++
			}
			wg.Wait()
			if nSnap == 0 {
				t.Fatal("emit produced no snapshots")
			}
			c.mu.Lock()
			nAgents, nProbes := len(c.agents), len(c.probes)
			c.mu.Unlock()
			if nAgents == 0 {
				t.Fatal("RecordAgent produced no events")
			}
			if nProbes == 0 {
				t.Fatal("RecordProbe produced no samples")
			}
			return
		}
	}
}

// A consumer stalled on a full channel must not pin c.mu: agent-event
// recording (ingest HTTP handlers) and probe recording stay live while emit
// waits to deliver a snapshot. Regression for sending under the lock.
func TestEmitBlockedSendDoesNotPinMu(t *testing.T) {
	col := New(nil, time.Hour) // no providers: emit parks on the send at once
	ctx := t.Context()
	ch := make(chan core.Snapshot) // unbuffered: the send blocks until consumed
	go col.emit(ctx, ch)
	waitUntilEmitParked(t)

	done := make(chan struct{})
	go func() {
		col.RecordAgent(core.AgentEvent{Agent: "liveness-probe"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RecordAgent blocked while emit was parked on a channel send")
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("emit was not parked: no snapshot pending")
	}
}

// A cold vitals sample (GPU vendor CLIs) must not hold c.mu. Regression for
// sampling inside emit's critical section.
func TestEmitSysSampleDoesNotPinMu(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	col := New(nil, time.Hour)
	col.SetSysFn(func() core.SysSample {
		close(started)
		<-release
		return core.SysSample{MemTotal: 1}
	})
	ch := make(chan core.Snapshot, 1)
	go col.emit(context.Background(), ch)

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("sysFn never started")
	}

	done := make(chan struct{})
	go func() {
		col.RecordAgent(core.AgentEvent{Agent: "liveness-probe"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RecordAgent blocked while emit was sampling vitals")
	}
	close(release)
}

// waitFor polls cond until it holds or the deadline passes; probe completion
// is asynchronous, so tests must wait rather than sleep-and-hope.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

func waitUntilEmitParked(t *testing.T) {
	t.Helper()
	waitFor(t, func() bool {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		s := string(buf[:n])
		return strings.Contains(s, "/collector.") && strings.Contains(s, "emit")
	}, "emit never parked on the snapshot send")
}

// waitStay fails if cond drops during window: a "must not happen" check
// that would otherwise be a single sleep-and-sample.
func waitStay(t *testing.T, window time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if !cond() {
			t.Fatal(msg)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Holding 'p' or stacking --probe on top of it must not pile concurrent
// generations onto one backend: a probing engine distorts the metrics being
// watched, and gateway backends bill per generated token. At most one probe
// runs per backend, and a finished backend re-arms once its sample lands.
func TestProbeAllSingleFlightPerBackend(t *testing.T) {
	release := make(chan struct{})
	releaseProbes := sync.OnceFunc(func() { close(release) })
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release // hold the generation open until the test lets it finish
		w.Header().Set("Content-Type", "application/x-ndjson")
		io.WriteString(w, "{\"response\":\"one\",\"done\":false}\n"+
			"{\"response\":\"two\",\"done\":true,\"eval_count\":2,\"eval_duration\":1000000}\n")
	}))
	defer srv.Close()
	defer releaseProbes()

	oldGap := probeWaveGap
	probeWaveGap = 0 // isolate single-flight from the wave gate
	defer func() { probeWaveGap = oldGap }()

	c := New([]provider.Provider{(&fakeProvider{label: "p", addr: srv.URL}).asProvider()}, time.Second)
	c.lastModel[srv.URL] = "m"

	c.ProbeAll()
	waitFor(t, func() bool { return hits.Load() == 1 }, "first wave never reached the engine")

	c.ProbeAll() // first still in flight: this wave must be dropped
	waitStay(t, 50*time.Millisecond, func() bool { return hits.Load() == 1 },
		"second wave started while the first was in flight")

	c.mu.Lock()
	c.lastModel[srv.URL] = "replacement"
	c.mu.Unlock()
	c.ProbeAll()
	waitStay(t, 50*time.Millisecond, func() bool { return hits.Load() == 1 },
		"model change started a second probe on the same backend")

	releaseProbes()
	waitFor(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.probes) == 1
	}, "in-flight probe never recorded")
	waitFor(t, func() bool {
		c.probeMu.Lock()
		defer c.probeMu.Unlock()
		return len(c.probeInflight) == 0
	}, "in-flight probe never cleared")

	c.ProbeAll() // completed: a fresh generation is allowed
	waitFor(t, func() bool { return hits.Load() == 2 }, "re-armed backend was never probed again")
	waitFor(t, func() bool {
		c.probeMu.Lock()
		defer c.probeMu.Unlock()
		return len(c.probeInflight) == 0
	}, "replacement probe never cleared")
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.probes) != 2 || c.probes[1].Model != "replacement" {
		t.Fatalf("probes = %+v, want the replacement model after re-arming", c.probes)
	}
}

// Rapid-fire triggers (a held 'p', a fast --probe ticker) must not launch
// waves continuously; within one gap only the first wave runs.
func TestProbeAllWaveGap(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, "{\"response\":\"one\",\"done\":true,\"eval_count\":1,\"eval_duration\":1000000}\n")
	}))
	defer srv.Close()

	oldGap := probeWaveGap
	probeWaveGap = time.Hour // longer than this test can run: only wave #1 passes
	defer func() { probeWaveGap = oldGap }()

	c := New([]provider.Provider{(&fakeProvider{label: "g", addr: srv.URL}).asProvider()}, time.Second)
	c.lastModel[srv.URL] = "m"

	c.ProbeAll()
	c.ProbeAll()
	c.ProbeAll()
	waitFor(t, func() bool { return hits.Load() == 1 }, "first wave never reached the engine")
	waitStay(t, 50*time.Millisecond, func() bool { return hits.Load() == 1 },
		"gap gate let extra waves reach the engine")
}

func emitOnce(t *testing.T, c *Collector) {
	t.Helper()
	ch := make(chan core.Snapshot, 1)
	c.emit(context.Background(), ch)
	<-ch
}

// A successful poll with no loaded models must forget the previous id:
// probing it would JIT-load (or bill) a cold weight.
func TestProbeAllDropsModelOnceUnloaded(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		io.WriteString(w, "{\"response\":\"one\",\"done\":true,\"eval_count\":1,\"eval_duration\":1000000}\n")
	}))
	defer srv.Close()

	oldGap := probeWaveGap
	probeWaveGap = 0
	defer func() { probeWaveGap = oldGap }()

	fp := fakeProvider{
		label: "p", addr: srv.URL,
		m: &provider.Metrics{Models: []core.ModelInfo{{Name: "m"}}},
	}
	c := New([]provider.Provider{fp.asProvider()}, time.Second)
	emitOnce(t, c)

	fp.m = &provider.Metrics{} // still up, nothing loaded
	emitOnce(t, c)

	c.ProbeAll()
	waitStay(t, 50*time.Millisecond, func() bool { return hits.Load() == 0 },
		"probe ran against an unloaded model")
}

// Catalog entries without VRAM must lose to a loaded model, otherwise 'p'
// JIT-loads whatever /v1/models listed first.
func TestProbeAllPrefersLoadedModel(t *testing.T) {
	var got atomic.Value
	got.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if m, _ := body["model"].(string); m != "" {
			got.Store(m)
		}
		io.WriteString(w, "{\"response\":\"one\",\"done\":true,\"eval_count\":1,\"eval_duration\":1000000}\n")
	}))
	defer srv.Close()

	oldGap := probeWaveGap
	probeWaveGap = 0
	defer func() { probeWaveGap = oldGap }()

	fp := fakeProvider{
		label: "p", addr: srv.URL,
		m: &provider.Metrics{Models: []core.ModelInfo{
			{Name: "catalog-only"},
			{Name: "loaded", SizeVRAM: 1 << 30},
		}},
	}
	c := New([]provider.Provider{fp.asProvider()}, time.Second)
	emitOnce(t, c)
	c.ProbeAll()
	waitFor(t, func() bool { return got.Load().(string) == "loaded" },
		"probe did not target the loaded model")
}

func TestProbeAllSkipsBlankModelName(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	oldGap := probeWaveGap
	probeWaveGap = 0
	defer func() { probeWaveGap = oldGap }()

	fp := fakeProvider{
		label: "p", addr: srv.URL,
		m: &provider.Metrics{Models: []core.ModelInfo{{Name: "   "}}},
	}
	c := New([]provider.Provider{fp.asProvider()}, time.Second)
	emitOnce(t, c)
	c.ProbeAll()
	waitStay(t, 50*time.Millisecond, func() bool { return hits.Load() == 0 },
		"probe ran with a blank model id")
}

// Probe samples ride the collector clock, not probe.Run's wall-clock start,
// so a frozen now produces the same At on every backend in the wave.
func TestProbeAllStampsWithInjectedClock(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "{\"response\":\"one\",\"done\":true,\"eval_count\":1,\"eval_duration\":1000000}\n")
	})
	srvA := httptest.NewServer(handler)
	defer srvA.Close()
	srvB := httptest.NewServer(handler)
	defer srvB.Close()

	frozen := time.Unix(1_700_000_000, 0).UTC()
	c := frozenCollector(t, frozen, []provider.Provider{(&fakeProvider{label: "b", addr: srvB.URL}).asProvider(), (&fakeProvider{label: "a", addr: srvA.URL}).asProvider()})
	c.lastModel[srvA.URL] = "ma"
	c.lastModel[srvB.URL] = "mb"
	c.ProbeAll()
	waitFor(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.probes) == 2
	}, "probes never recorded")

	c.mu.Lock()
	probes := append([]core.ProbeSample(nil), c.probes...)
	c.mu.Unlock()
	for _, p := range probes {
		if !p.At.Equal(frozen) {
			t.Fatalf("At = %v, want injected %v (%+v)", p.At, frozen, p)
		}
	}
	if probes[0].Addr > probes[1].Addr {
		t.Fatalf("equal-timestamp probes not ordered by addr: %q then %q",
			probes[0].Addr, probes[1].Addr)
	}
}

// A chat completion against an embedding weight is a wasted billed request.
func TestProbeAllSkipsEmbeddingModel(t *testing.T) {
	var got atomic.Value
	got.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if m, _ := body["model"].(string); m != "" {
			got.Store(m)
		}
		io.WriteString(w, "{\"response\":\"one\",\"done\":true,\"eval_count\":1,\"eval_duration\":1000000}\n")
	}))
	defer srv.Close()

	oldGap := probeWaveGap
	probeWaveGap = 0
	defer func() { probeWaveGap = oldGap }()

	fp := fakeProvider{
		label: "p", addr: srv.URL,
		m: &provider.Metrics{Models: []core.ModelInfo{
			{Name: "text-embedding-3-small", SizeVRAM: 1 << 30},
			{Name: "llama3"},
		}},
	}
	c := New([]provider.Provider{fp.asProvider()}, time.Second)
	emitOnce(t, c)
	c.ProbeAll()
	waitFor(t, func() bool { return got.Load().(string) == "llama3" },
		"probe targeted the embedding model instead of the chat id")
}

func TestProbeAllSkipsEmbedOnlyInventory(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	oldGap := probeWaveGap
	probeWaveGap = 0
	defer func() { probeWaveGap = oldGap }()

	fp := fakeProvider{
		label: "p", addr: srv.URL,
		m: &provider.Metrics{Models: []core.ModelInfo{{Name: "nomic-embed-text"}}},
	}
	c := New([]provider.Provider{fp.asProvider()}, time.Second)
	emitOnce(t, c)
	c.ProbeAll()
	waitStay(t, 50*time.Millisecond, func() bool { return hits.Load() == 0 },
		"probe ran against an embed-only inventory")
}

// 429/503 must keep ProbeAll from POSTing that backend until Retry-After
// elapses; otherwise --probe 1 becomes a spend amplifier.
func TestProbeAllHonorsRetryAfter(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"rate limited"}`)
	}))
	defer srv.Close()

	oldGap := probeWaveGap
	probeWaveGap = 0
	defer func() { probeWaveGap = oldGap }()

	var clockMu sync.Mutex
	now := time.Unix(1_700_000_000, 0).UTC()
	c := New([]provider.Provider{(&fakeProvider{label: "p", addr: srv.URL}).asProvider()}, time.Second)
	c.SetNow(func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	})
	c.lastModel[srv.URL] = "m"

	c.ProbeAll()
	waitFor(t, func() bool { return hits.Load() == 1 }, "first 429 never reached the engine")
	waitFor(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.probes) == 1 && c.probes[0].RetryAfter == 30*time.Second
	}, "429 sample never recorded")
	waitFor(t, func() bool {
		c.probeMu.Lock()
		defer c.probeMu.Unlock()
		return len(c.probeInflight) == 0
	}, "in-flight 429 never cleared")

	c.ProbeAll()
	waitStay(t, 50*time.Millisecond, func() bool { return hits.Load() == 1 },
		"backoff let another POST through")

	c.mu.Lock()
	c.lastModel[srv.URL] = "replacement"
	c.mu.Unlock()
	c.ProbeAll()
	waitStay(t, 50*time.Millisecond, func() bool { return hits.Load() == 1 },
		"model change bypassed the backend backoff")

	clockMu.Lock()
	now = now.Add(31 * time.Second)
	clockMu.Unlock()
	c.ProbeAll()
	waitFor(t, func() bool { return hits.Load() == 2 }, "expired backoff never re-armed")
	waitFor(t, func() bool {
		c.probeMu.Lock()
		defer c.probeMu.Unlock()
		return len(c.probeInflight) == 0
	}, "replacement probe never cleared")
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.probes) != 2 || c.probes[1].Model != "replacement" {
		t.Fatalf("probes = %+v, want the replacement model after backoff", c.probes)
	}
}

func TestAgentCmp(t *testing.T) {
	t0 := time.Unix(100, 0)
	t1 := time.Unix(200, 0)

	cases := []struct {
		name string
		a, b core.AgentEvent
		want int
	}{
		{"time before", core.AgentEvent{At: t0}, core.AgentEvent{At: t1}, -1},
		{"time after", core.AgentEvent{At: t1}, core.AgentEvent{At: t0}, 1},
		{"agent name", core.AgentEvent{At: t0, Agent: "a"}, core.AgentEvent{At: t0, Agent: "b"}, -1},
		{"id", core.AgentEvent{At: t0, Agent: "a", ID: "1"}, core.AgentEvent{At: t0, Agent: "a", ID: "2"}, -1},
		{"note", core.AgentEvent{At: t0, Agent: "a", ID: "1", Note: "alpha"}, core.AgentEvent{At: t0, Agent: "a", ID: "1", Note: "beta"}, -1},
		{"equal", core.AgentEvent{At: t0, Agent: "a", ID: "1", Note: "alpha"}, core.AgentEvent{At: t0, Agent: "a", ID: "1", Note: "alpha"}, 0},
	}
	for _, tc := range cases {
		c := agentCmp(tc.a, tc.b)
		switch {
		case tc.want < 0 && c >= 0:
			t.Errorf("%s: agentCmp = %d, want < 0", tc.name, c)
		case tc.want > 0 && c <= 0:
			t.Errorf("%s: agentCmp = %d, want > 0", tc.name, c)
		case tc.want == 0 && c != 0:
			t.Errorf("%s: agentCmp = %d, want 0", tc.name, c)
		}
	}
}

func TestRecordAgentEqualTimestampOrdersByNote(t *testing.T) {
	c := New(nil, time.Second)
	at := time.Unix(1_700_000_000, 0).UTC()
	c.RecordAgent(core.AgentEvent{At: at, Agent: "claude", Note: "z-note"})
	c.RecordAgent(core.AgentEvent{At: at, Agent: "claude", Note: "a-note"})
	if len(c.agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(c.agents))
	}
	if c.agents[0].Note != "a-note" || c.agents[1].Note != "z-note" {
		t.Fatalf("agents = %+v, want sorted by Note", c.agents)
	}
}

func TestProviderKey(t *testing.T) {
	pWithAddr := provider.Provider{
		Label: "ollama",
		Addr:  "http://127.0.0.1:11434",
	}
	if got := providerKey(pWithAddr); got != "http://127.0.0.1:11434" {
		t.Errorf("providerKey with Addr = %q, want http://127.0.0.1:11434", got)
	}

	pNoAddr := provider.Provider{
		Label: "gpu-monitor",
		Addr:  "",
	}
	if got := providerKey(pNoAddr); got != "gpu-monitor" {
		t.Errorf("providerKey without Addr = %q, want gpu-monitor", got)
	}
}
