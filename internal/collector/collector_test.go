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
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
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
	delay time.Duration // how long Poll takes before it answers
}

func (f *fakeProvider) asProvider() provider.Provider {
	addr, kind := f.addr, f.kind
	if addr == "" {
		addr = "fake://" + f.label
	}
	if kind == "" {
		kind = core.KindOllama
	}
	return provider.Provider{Label: f.label, Addr: addr, Kind: kind, Poll: func(ctx context.Context) (*provider.Metrics, error) {
		if f.delay > 0 {
			select {
			case <-time.After(f.delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return f.m, f.err
	}}
}

func TestLoopbackPortExtracts(t *testing.T) {
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
		if got, _ := loopbackPort(tc.addr); got != tc.want {
			t.Errorf("loopbackPort(%q) = %d, want %d", tc.addr, got, tc.want)
		}
	}
}

// Whether a provider's process stats and per-process GPU numbers attach at
// all is this one predicate. A remote engine that passes it has another
// machine's PIDs and RSS attributed to it, so the loopback spellings that
// name the same host all have to agree, and nothing that only looks like one
// may pass.
func TestLoopbackPortLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"http://localhost:11434", true},
		{"http://LOCALHOST:11434", true},
		{"https://LocalHost", true},
		{"http://127.0.0.1:8000", true},
		{"http://127.0.0.5:8000", true},
		{"http://[::1]:8000", true},
		{"http://example.com:8000", false},
		{"http://10.0.0.5:8000", false},
		{"http://169.254.169.254:80", false},
		{"http://notlocalhost:8000", false},
		{"http://localhost.evil.com:8000", false},
		{"http://:8000", false},
		{"http://127.0.0.1:8000/%zz", false},
		{"", false},
		{"localhost:8000", false},
		{"http://[::1", false},
	}
	for _, tc := range cases {
		if _, got := loopbackPort(tc.addr); got != tc.want {
			t.Errorf("loopbackPort(%q) loopback = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// Two processes can share a listen port (a supervisor and the child it
// forked, both holding the socket across a reload). The first one the walk
// found is the one whose stats get attached, and it has to be the same one
// every time, or a frame's per-process numbers come from whichever process
// the table happened to list first.
func TestProcsByPortKeepsTheFirstProcess(t *testing.T) {
	infos := []procs.Info{
		{PID: 10, Name: "supervisor", DefPort: 8000},
		{PID: 11, Name: "child", PortHint: 8000},
		{PID: 12, Name: "other", PortHint: 9000},
		{PID: 13, Name: "no-port"},
	}
	got := procsByPort(infos)
	if len(got) != 2 {
		t.Fatalf("procsByPort holds %d ports, want 2: %v", len(got), got)
	}
	for port, want := range map[int]procs.Info{8000: infos[0], 9000: infos[2]} {
		p, ok := got[port]
		if !ok {
			t.Errorf("port %d missing", port)
			continue
		}
		if p.PID != want.PID {
			t.Errorf("port %d = pid %d, want %d", port, p.PID, want.PID)
		}
	}
	// Repeating the walk has to give the same answer.
	if again := procsByPort(infos); again[8000].PID != got[8000].PID {
		t.Errorf("port 8000 changed between walks: %d then %d", got[8000].PID, again[8000].PID)
	}
	if len(procsByPort(nil)) != 0 {
		t.Error("an empty process list must produce no ports")
	}
}

func TestRatesDeriveAndSmooth(t *testing.T) {
	c := New(nil, time.Second)
	now := time.Now()

	out, in := c.rates("p", &provider.Metrics{OutTotal: 100}, now)
	if out != 0 || in != 0 {
		t.Fatalf("first sample must seed baseline, got %v/%v", out, in)
	}
	// Pin the arithmetic, not a band around it: a wrong alpha or a doubled
	// weight lands inside the old 106..299 window and passes. The literals are
	// raw 300 tok/s smoothed with the production alpha of 0.35 from 0, then
	// from that result.
	const alpha = 0.35
	out, _ = c.rates("p", &provider.Metrics{OutTotal: 400}, now.Add(time.Second))
	first := (0 * (1 - alpha)) + (300 * alpha)
	if out != first {
		t.Fatalf("first smoothing step = %v, want %v", out, first)
	}
	out, _ = c.rates("p", &provider.Metrics{OutTotal: 700}, now.Add(2*time.Second))
	if want := first*(1-alpha) + 300*alpha; out != want {
		t.Fatalf("second smoothing step = %v, want %v", out, want)
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

// A clock stepped backwards makes the interval negative, so no rate is
// defined. Holding the prior one would report the old rate unchanged for as
// long as the step lasts, so the baseline is re-seeded and the interval the
// step made reports no throughput.
func TestRatesBackwardClockReseedsBaseline(t *testing.T) {
	c := New(nil, time.Second)
	now := time.Now()
	c.rates("p", &provider.Metrics{OutTotal: 100}, now)
	out, _ := c.rates("p", &provider.Metrics{OutTotal: 400}, now.Add(time.Second))
	if out <= 0 {
		t.Fatalf("rate before the step = %v, want a positive rate to lose", out)
	}

	stepped, _ := c.rates("p", &provider.Metrics{OutTotal: 700}, now.Add(-5*time.Minute))
	if stepped != 0 {
		t.Fatalf("rate across a backward step = %v, want 0", stepped)
	}

	// The next interval measures from the step, not from the pre-step sample.
	next, _ := c.rates("p", &provider.Metrics{OutTotal: 800}, now.Add(-5*time.Minute).Add(time.Second))
	if next <= 0 {
		t.Fatalf("rate after the step = %v, want a positive rate measured from the new baseline", next)
	}
}

// Engines that publish instantaneous tok/s gauges (SGLang, TRT-LLM) must feed
// the rate directly instead of counter deltas. The expected values are
// literals, not ema() calls: an expectation built from the same helper as the
// code moves with any change to the smoothing and proves nothing.
func TestRatesHonorDirectThroughput(t *testing.T) {
	const emaAlpha = 0.35
	c := New(nil, time.Second)
	c.rates("p", &provider.Metrics{OutTotal: 10}, time.Now())
	// The first gauge seeds the EMA at its own alpha: 0*0.65 + 300*0.35.
	out, _ := c.rates("p", &provider.Metrics{OutTotal: 10, DirectOutPS: 300, HasDirectOutPS: true}, time.Now().Add(time.Second))
	if out != 105 {
		t.Fatalf("direct rate = %v, want 105", out)
	}
	// The next one smooths: 105*0.65 + 200*0.35.
	out, _ = c.rates("p", &provider.Metrics{OutTotal: 10, DirectOutPS: 200, HasDirectOutPS: true}, time.Now().Add(2*time.Second))
	if want := 105*(1-emaAlpha) + 200*emaAlpha; out != want {
		t.Fatalf("smoothed direct rate = %v, want %v", out, want)
	}
	// Without the flag the counter delta path runs instead, and a flat
	// OutTotal over one window means no output rate at all.
	out, _ = c.rates("q", &provider.Metrics{OutTotal: 10, DirectOutPS: 200}, time.Now())
	if out != 0 {
		t.Fatalf("rate without the direct flag = %v, want 0", out)
	}
}

// A rate that is not a finite number cannot be smoothed or drawn: NaN
// compares false against every bound, and both NaN and +Inf reach the JSON
// report, which has no spelling for either and fails the whole frame. A
// derived rate overflowing its divisor, and a direct gauge carrying one, must
// both read as no throughput for that interval.
func TestRatesRejectNonFiniteThroughput(t *testing.T) {
	for _, tc := range []struct {
		name string
		bad  float64
	}{
		{"nan", math.NaN()},
		{"posinf", math.Inf(1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(nil, time.Second)
			now := time.Now()
			c.rates("p", &provider.Metrics{OutTotal: 100}, now)
			// The derived half: a total that overflows the interval.
			out, in := c.rates("p", &provider.Metrics{OutTotal: math.Inf(1)}, now.Add(time.Second))
			if out != 0 || in != 0 {
				t.Fatalf("derived rate from an infinite total = %v/%v, want 0/0", out, in)
			}
			// The direct half: the engine's own gauge, which bypasses the
			// delta entirely.
			out, _ = c.rates("q", &provider.Metrics{OutTotal: 10, DirectOutPS: tc.bad, HasDirectOutPS: true}, now.Add(time.Second))
			if out != 0 {
				t.Fatalf("direct rate %v = %v, want 0", tc.name, out)
			}
			// Neither poisons the next interval: a well-formed sample after a
			// rejected one still measures from the baseline it left.
			out, _ = c.rates("q", &provider.Metrics{OutTotal: 10, DirectOutPS: 200, HasDirectOutPS: true}, now.Add(2*time.Second))
			if want := 200 * 0.35; out != want {
				t.Fatalf("rate after a rejected sample = %v, want %v", out, want)
			}
		})
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
			t.Cleanup(func() { c.SetNow(nil) })
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
			// A local alpha, not the package constant: an expectation built
			// from the same constant as the code moves with a retune and
			// proves nothing. 300 at 0.35 smooths to 300, and 0 seeds to 105.
			const alpha = 0.35
			wantNext := tc.want*(1-alpha) + 300*alpha
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
			t.Cleanup(func() { c.SetNow(nil) })
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
		r.push(float64(i), t0.Add(time.Duration(i)*time.Second))
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
	// the oldest stamp must slide forward with the values it belongs to
	ts := r.times()
	if len(ts) != len(vals) {
		t.Fatalf("stamps = %d, want %d", len(ts), len(vals))
	}
	if want := t0.Add(10 * time.Second); !ts[0].Equal(want) {
		t.Fatalf("oldest stamp = %v, want %v", ts[0], want)
	}
}

// Once warm, push must stop allocating: the ring reuses one fixed buffer for
// its lifetime instead of sliding a slice (which reallocs on every push).
func TestHistoryRingDoesNotGrowBuffer(t *testing.T) {
	r := &timedRing{}
	for i := range core.HistoryLen * 3 {
		r.push(float64(i), time.Unix(int64(i), 0))
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
// PollTimeout and coalesced ticks widen gaps further. Each sample keeps the
// instant it was actually pushed, so a stall leaves a real gap in the series
// instead of a chart that claims the samples were evenly spaced.
func TestHistoryRingKeepsRealStampsAfterGap(t *testing.T) {
	r := &timedRing{}
	base := time.Unix(1_000_000, 0)
	for i := range 5 {
		r.push(float64(i), base.Add(time.Duration(i)*time.Second))
	}
	r.push(5, base.Add(9*time.Second)) // 4s stall, tick coalesced
	ts := r.times()
	if want := base.Add(4 * time.Second); !ts[4].Equal(want) {
		t.Fatalf("stamp before the gap = %v, want %v", ts[4], want)
	}
	if want := base.Add(9 * time.Second); !ts[5].Equal(want) {
		t.Fatalf("stamp after the gap = %v, want %v", ts[5], want)
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

	// Read the cache directly rather than through sysSnapshot: that falls
	// back to sampling inline whenever the cache is cold, so polling it here
	// would let this test's own call satisfy the wait and a dead background
	// poller would still pass.
	cached := func() *core.SysSample {
		c.sysMu.Lock()
		defer c.sysMu.Unlock()
		return c.sysCache
	}
	deadline := time.Now().Add(2 * time.Second)
	for cached() == nil {
		if time.Now().After(deadline) {
			t.Fatal("background poller never cached a sample")
		}
		time.Sleep(time.Millisecond)
	}
	before := calls.Load()
	if before == 0 {
		t.Fatal("no sample was taken before emit; the wait proved nothing")
	}

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

// The published snapshot must not alias the poller's cache. Drivers, Temps,
// GPUs and NPUs are reference fields, and a remote merge (remote.Merge) or
// a frame's own overlay writes into them in place, so a snapshot that shares
// the backing arrays would show another writer's values on a frame already
// handed out.
func TestSnapshotDoesNotAliasTheCachedSysSample(t *testing.T) {
	c := New(nil, time.Hour)
	sample := &core.SysSample{
		MemTotal: 100,
		Drivers:  map[string]string{"nvidia": "550.1"},
		Temps:    []core.TempReading{{Label: "Tctl", MilliC: 64000}},
		GPUs:     []core.GPUDevice{{Vendor: "nvidia", Index: 0, Name: "RTX 4090"}},
		NPUs:     []string{"amd"},
	}
	c.sysMu.Lock()
	c.sysCache = sample
	c.sysMu.Unlock()
	if cloneSys(nil) != nil {
		t.Fatal("a missing sample must stay missing")
	}

	clone := cloneSys(sample)
	sample.Drivers["nvidia"] = "999.9"
	sample.Temps[0].MilliC = 1
	sample.GPUs[0].Name = "mutated"
	sample.NPUs[0] = "mutated"
	if clone.Drivers["nvidia"] != "550.1" {
		t.Errorf("snapshot driver = %q, want the value at clone time", clone.Drivers["nvidia"])
	}
	if clone.Temps[0].MilliC != 64000 {
		t.Errorf("snapshot temp = %d, want the value at clone time", clone.Temps[0].MilliC)
	}
	if clone.GPUs[0].Name != "RTX 4090" {
		t.Errorf("snapshot GPU = %q, want the value at clone time", clone.GPUs[0].Name)
	}
	if clone.NPUs[0] != "amd" {
		t.Errorf("snapshot NPU = %q, want the value at clone time", clone.NPUs[0])
	}
	// The other direction matters too: writing the snapshot must not reach
	// back into the cache the next frame is taken from.
	clone.Drivers["amdgpu"] = "6.11"
	if _, leaked := sample.Drivers["amdgpu"]; leaked {
		t.Error("writing the snapshot's driver map reached the cached sample")
	}
	if clone.MemTotal != 100 {
		t.Errorf("MemTotal = %d, want 100", clone.MemTotal)
	}
}

func frozenCollector(t *testing.T, frozen time.Time, providers []provider.Provider) *Collector {
	t.Helper()
	c := New(providers, time.Second)
	c.SetNow(func() time.Time { return frozen })
	t.Cleanup(func() { c.SetNow(nil) })
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

// A clock stepped backwards (NTP correction, a laptop resuming from sleep) puts
// the frame's instant before the one this process started on. Uptime is an
// elapsed duration, so it floors at zero rather than reporting a session that
// ends before it began; --json serializes it unguarded.
func TestEmitClampsUptimeOnABackwardStep(t *testing.T) {
	started := time.Unix(1_700_000_000, 0).UTC()
	stepped := started.Add(-5 * time.Minute)
	c := frozenCollector(t, stepped, []provider.Provider{(&fakeProvider{label: "x", m: &provider.Metrics{OutTotal: 10}}).asProvider()})
	c.started = started
	ch := make(chan core.Snapshot, 1)
	c.emit(context.Background(), ch)
	snap := <-ch
	if !snap.At.Equal(stepped) {
		t.Fatalf("At = %v, want the stepped instant %v", snap.At, stepped)
	}
	if snap.Uptime != 0 {
		t.Fatalf("Uptime = %v, want 0 after a backward step", snap.Uptime)
	}
}

func TestEmitDeterministicUnderSameClock(t *testing.T) {
	frozen := time.Unix(1_700_000_000, 0).UTC()
	fp := fakeProvider{label: "ollama", m: &provider.Metrics{OutTotal: 100, InTotal: 20, Running: 1}}
	chA, chB := make(chan core.Snapshot, 1), make(chan core.Snapshot, 1)
	frozenCollector(t, frozen, []provider.Provider{fp.asProvider()}).emit(context.Background(), chA)
	frozenCollector(t, frozen, []provider.Provider{fp.asProvider()}).emit(context.Background(), chB)
	sa, sb := <-chA, <-chB
	// Pin the content before the comparison: two empty snapshots are equal, so
	// the equality below on its own holds for an emit that publishes nothing.
	if !sa.At.Equal(frozen) {
		t.Fatalf("At = %v, want the injected %v", sa.At, frozen)
	}
	if len(sa.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(sa.Providers))
	}
	if p := sa.Providers[0]; p.Label != "ollama" || p.Running != 1 || !p.OK {
		t.Fatalf("provider = %+v, want ollama ok running=1", p)
	}
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

// A collector is built for one live run. A second Run alongside the first
// would start a second pair of pollers, write a second Snapshot per interval
// into the same channel, and fold its polls into the same rate baselines, so
// the second call is refused rather than run alongside the first. A run
// started after the first returned is a restart and still works.
func TestRunRefusesASecondLiveRun(t *testing.T) {
	fp := fakeProvider{label: "run", m: &provider.Metrics{
		OutTotal: 1, Models: []core.ModelInfo{{Name: "m"}},
	}}
	newCollector := func() (*Collector, chan core.Snapshot) {
		c := New([]provider.Provider{fp.asProvider()}, 5*time.Millisecond)
		c.SetSysFn(func() core.SysSample { return core.SysSample{} })
		c.procFn = func() []procs.Info { return nil }
		return c, make(chan core.Snapshot, 8)
	}

	c, ch := newCollector()
	ctx, cancel := context.WithCancel(t.Context())
	first := make(chan error, 1)
	go func() { first <- c.Run(ctx, ch) }()
	// One frame proves the first run holds the claim before the second calls.
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("the first Run never emitted")
	}
	if err := c.Run(ctx, ch); !errors.Is(err, errRunInProgress) {
		t.Fatalf("second Run = %v, want errRunInProgress", err)
	}
	// The live run is still filling the channel, so what the refused Run adds
	// is measured against a drained one: a stale frame here would fail the
	// check spuriously, and would later stand in for the restart's first.
	drain(ch)
	if n := len(ch); n != 0 {
		t.Fatalf("the refused Run emitted %d snapshots", n)
	}
	cancel()
	if err := <-first; err != nil {
		t.Fatalf("first Run = %v, want nil", err)
	}
	drain(ch)

	// The claim is released on return, so a restart is not read as a second
	// live loop and a collector that was stopped can still be run again.
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	restart := make(chan error, 1)
	go func() { restart <- c.Run(ctx2, ch) }()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("the restarted Run never emitted")
	}
	cancel2()
	if err := <-restart; err != nil {
		t.Fatalf("restarted Run = %v, want nil", err)
	}
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

// The negative only means something next to a positive: a ProbeAll that
// probed nothing ever passes it.
func TestProbeAllSkipsWhenNoModelKnown(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		io.WriteString(w, "{\"response\":\"one\",\"done\":true,\"eval_count\":1,\"eval_duration\":1000000}\n")
	}))
	defer srv.Close()

	oldGap := probeWaveGap
	probeWaveGap = 0
	defer func() { probeWaveGap = oldGap }()

	fp := &fakeProvider{label: "x", addr: srv.URL, m: &provider.Metrics{}}
	c := New([]provider.Provider{fp.asProvider()}, time.Second)
	c.ProbeAll() // must not panic or block; no model known yet
	if len(c.probes) != 0 || hits.Load() != 0 {
		t.Fatalf("no model known yet: probes = %d, POSTs = %d, want 0 and 0", len(c.probes), hits.Load())
	}

	fp.m = &provider.Metrics{Models: []core.ModelInfo{{Name: "m"}}}
	emitOnce(t, c)
	c.ProbeAll()
	waitFor(t, func() bool { return hits.Load() == 1 },
		"the same wave never reached the engine once a model was known, so the negative above proved nothing")
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
// and agent completions would then shuffle the ring across replays of the
// same clock.
func TestRecordEqualTimestampOrdersByIdentity(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	for _, tc := range []struct {
		name   string
		record func(*Collector)
		keys   func(*Collector) [][2]string
		want   [][2]string
	}{
		{
			name: "probes order by addr then model",
			record: func(c *Collector) {
				c.RecordProbe(core.ProbeSample{At: at, Addr: "http://127.0.0.1:8000", Model: "b"})
				c.RecordProbe(core.ProbeSample{At: at, Addr: "http://127.0.0.1:11434", Model: "a"})
				c.RecordProbe(core.ProbeSample{At: at, Addr: "http://127.0.0.1:8000", Model: "a"})
			},
			keys: func(c *Collector) [][2]string {
				out := make([][2]string, len(c.probes))
				for i, p := range c.probes {
					out[i] = [2]string{p.Addr, p.Model}
				}
				return out
			},
			want: [][2]string{
				{"http://127.0.0.1:11434", "a"},
				{"http://127.0.0.1:8000", "a"},
				{"http://127.0.0.1:8000", "b"},
			},
		},
		{
			name: "agents order by agent then id",
			record: func(c *Collector) {
				c.RecordAgent(core.AgentEvent{At: at, Agent: "codex", ID: "2"})
				c.RecordAgent(core.AgentEvent{At: at, Agent: "claude", ID: "1"})
				c.RecordAgent(core.AgentEvent{At: at, Agent: "claude", ID: "0"})
			},
			keys: func(c *Collector) [][2]string {
				out := make([][2]string, len(c.agents))
				for i, ev := range c.agents {
					out[i] = [2]string{ev.Agent, ev.ID}
				}
				return out
			},
			want: [][2]string{{"claude", "0"}, {"claude", "1"}, {"codex", "2"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(nil, time.Second)
			tc.record(c)
			got := tc.keys(c)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ring = %v, want %v", got, tc.want)
			}
		})
	}
}

// Agent events arrive over the ingest endpoint from senders whose clocks
// disagree, so arrival order is not time order; the retained slice must
// still be chronological or the agent feed renders a stale event last and
// eviction drops the wrong end. The sender here also runs ahead of the
// collector's clock, so the stamps land on the collector's timeline with the
// sender's spacing intact.
func TestRecordAgentKeepsChronologicalOrder(t *testing.T) {
	c := New(nil, time.Second)
	base := time.Now()
	const skew = 3 * time.Second
	c.SetNow(func() time.Time { return base.Add(10 * time.Second) })
	t.Cleanup(func() { c.SetNow(nil) })
	order := []time.Duration{0, 3 * time.Second, 7 * time.Second, 5 * time.Second}
	for _, d := range order {
		c.RecordAgent(core.AgentEvent{At: base.Add(10*time.Second + d + skew), Agent: "a", OutputTokens: 1})
	}
	for i := 1; i < len(c.agents); i++ {
		if c.agents[i].At.Before(c.agents[i-1].At) {
			t.Fatalf("agent ring not sorted at %d: %v", i, c.agents)
		}
	}
	if !c.agents[len(c.agents)-1].At.Equal(base.Add(10*time.Second + 7*time.Second)) {
		t.Fatalf("newest agent is %v, want %v", c.agents[len(c.agents)-1].At, base.Add(17*time.Second))
	}
}

// An event's timestamp is its sender's clock, and the ingest endpoint accepts
// one running ahead of arrival up to its skew bound. Stored as sent, such a
// stamp is never older than the summary's window cutoff, so its agent stays in
// the list and its tokens in the header aggregate however long the host has
// been quiet. The offset is one clock reading, not elapsed time: the feed
// keeps the sender's spacing, and the agent ages out one window after the
// events that named it arrived.
func TestRecordAgentAgesOutSenderRunningAhead(t *testing.T) {
	c := New(nil, time.Second)
	base := time.Now()
	const skew = 90 * time.Second
	now := base
	c.SetNow(func() time.Time { return now })
	t.Cleanup(func() { c.SetNow(nil) })

	// Two events a second apart, on a host whose clock is 90s fast. The
	// second names the same agent, so one offset covers both.
	for _, d := range []time.Duration{4 * time.Second, 3 * time.Second} {
		if !c.RecordAgent(core.AgentEvent{At: now.Add(skew - d), Agent: "remote", OutputTokens: 40}) {
			t.Fatalf("event at -%s was not retained", d)
		}
	}
	sum := core.Summarize(c.agents, now)
	if len(sum.Rates) != 1 || sum.Rates[0].Tokens != 80 {
		t.Fatalf("rates = %+v, want one remote row of 80 output tokens", sum.Rates)
	}
	if sum.Rates[0].TokPS != 80 {
		t.Errorf("remote = %v tok/s, want 80: a clock offset is not elapsed time", sum.Rates[0].TokPS)
	}

	// The agent leaves once its corrected stamps are a full window old. They
	// land at the instants the events arrived, so the newest is exactly the
	// window old one second past it; raw sender stamps would still be 90s in
	// this machine's future and the row would never go.
	now = base.Add(core.AgentRateWindow + 2*time.Second)
	if later := core.Summarize(c.agents, now); len(later.Rates) != 0 || len(later.Own) != 0 {
		t.Fatalf("a window later = %+v / %+v, want an empty summary", later.Rates, later.Own)
	}
}

// The offset ledger is keyed on the agent name, and a name is not a sender.
// A host whose clock was fast and has since been corrected keeps posting under
// the same name, and the first reading latched for that name would hold every
// later event back by the old error: past AgentRateWindow, so the agent reads
// as idle and its tokens land in no total. A smaller lead is a better estimate
// of the sender's clock than the larger one it replaces, so it takes over.
func TestRecordAgentOffsetFollowsASmallerLead(t *testing.T) {
	c := New(nil, time.Second)
	base := time.Now()
	now := base
	c.SetNow(func() time.Time { return now })
	t.Cleanup(func() { c.SetNow(nil) })

	// First sight: a host 90s fast, as a dead RTC leaves it.
	const stale = 90 * time.Second
	c.RecordAgent(core.AgentEvent{At: now.Add(stale), Agent: "remote", OutputTokens: 40})
	if got := c.agentSkews["remote"]; got != stale {
		t.Fatalf("first lead = %v, want %v", got, stale)
	}

	// The clock is corrected (NTP, a resumed laptop). Its next events carry
	// the true offset, which is far smaller.
	now = base.Add(2 * time.Second)
	for _, d := range []time.Duration{2 * time.Second, time.Second} {
		if !c.RecordAgent(core.AgentEvent{At: now.Add(d), Agent: "remote", OutputTokens: 40}) {
			t.Fatalf("event at +%s was not retained", d)
		}
	}
	if got := c.agentSkews["remote"]; got != time.Second {
		t.Fatalf("offset after the clock was corrected = %v, want 1s", got)
	}
	// The three events have to sit on the recent timeline, not 90s back. The
	// first was corrected by the reading in force when it arrived, so it
	// lands 2s old; the two the corrected clock sent land alongside it. Under
	// a latched 90s offset all three would sit at base and the two later ones
	// would still be, so only the first would fall inside the window.
	sum := core.Summarize(c.agents, now)
	if len(sum.Rates) != 1 {
		t.Fatalf("rates = %+v, want one remote row", sum.Rates)
	}
	if sum.Rates[0].Tokens != 120 {
		t.Errorf("remote tokens = %d, want 120: the corrected events are outside the window", sum.Rates[0].Tokens)
	}
	if sum.Rates[0].Last.After(now) {
		t.Errorf("remote Last = %v, in this machine's future", sum.Rates[0].Last)
	}

	// The superseded row must not delete the offset now in force when it ages
	// out: the ledger holds one entry per (agent, skew), and the count cap
	// walks past the older twin first.
	now = base.Add(core.AgentRateWindow + 2*time.Second)
	c.forgetAgedAgentSkews(now.Add(-core.AgentIDHorizon))
	if got, ok := c.agentSkews["remote"]; !ok {
		t.Fatal("the offset in force was dropped with its superseded row")
	} else if got != time.Second {
		t.Errorf("offset after ageing = %v, want the 1s reading to survive", got)
	}
}

// Two hosts running the same agent post under one canonical name, and each
// carries its own clock error. Latching the first name seen would hold the
// other host's events back by that error, dropping a working agent's tokens
// out of the window. The ledger holds the smallest lead, so a host on this
// machine's timeline pulls the shared name onto it.
func TestRecordAgentSharedNameTakesTheSmallerOffset(t *testing.T) {
	c := New(nil, time.Second)
	base := time.Now()
	now := base
	c.SetNow(func() time.Time { return now })
	t.Cleanup(func() { c.SetNow(nil) })

	const fast = 60 * time.Second
	c.RecordAgent(core.AgentEvent{At: now.Add(fast), Agent: "claude", OutputTokens: 40})

	// A second machine, whose clock is right, reports under the same name.
	now = base.Add(3 * time.Second)
	if !c.RecordAgent(core.AgentEvent{At: now, Agent: "claude", OutputTokens: 40}) {
		t.Fatal("the on-timeline event was not retained")
	}
	if got := c.agentSkews["claude"]; got != 0 {
		t.Fatalf("offset = %v, want 0: an on-timeline host is the better estimate", got)
	}
	sum := core.Summarize(c.agents, now)
	if len(sum.Rates) != 1 || sum.Rates[0].Tokens != 80 {
		t.Errorf("rates = %+v, want one claude row of 80 tokens", sum.Rates)
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

// The ingest server reports what the feed took in its 202 body and its audit
// line, so the recorder has to say which of the two happened.
func TestRecordAgentReportsStored(t *testing.T) {
	c := New(nil, time.Second)
	if !c.RecordAgent(core.AgentEvent{At: time.Now(), ID: "turn-1", Agent: "coder"}) {
		t.Fatal("first store reported a duplicate")
	}
	if c.RecordAgent(core.AgentEvent{At: time.Now(), ID: "turn-1", Agent: "coder"}) {
		t.Fatal("replayed id reported as stored")
	}
	if !c.RecordAgent(core.AgentEvent{At: time.Now(), ID: "turn-2", Agent: "coder"}) {
		t.Fatal("distinct id reported as a duplicate")
	}
}

// The feed retains the newest AgentHistoryLen events. An event stamped behind
// every one of them is what the window trims on arrival, so reporting it
// stored would put a kept-nothing event on the 202 body and the audit line,
// where the gap below accepted is documented as a replay. It must be refused,
// and its id left out of the ledger: no entry was retained, so there is no
// duplicate for the ledger to suppress.
func TestRecordAgentRefusesEventBehindRetainedWindow(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	now := base
	c := New(nil, time.Second)
	c.SetNow(func() time.Time { return now })
	t.Cleanup(func() { c.SetNow(nil) })
	for i := range core.AgentHistoryLen {
		now = base.Add(time.Duration(i) * time.Second)
		c.RecordAgent(core.AgentEvent{At: now, ID: fmt.Sprintf("n%d", i), Agent: "a"})
	}
	if len(c.agents) != core.AgentHistoryLen {
		t.Fatalf("feed holds %d events, want %d", len(c.agents), core.AgentHistoryLen)
	}
	oldest := c.agents[0].At
	if c.RecordAgent(core.AgentEvent{At: oldest.Add(-time.Second), ID: "stale", Agent: "a", OutputTokens: 9}) {
		t.Fatal("an event older than the retained window reported as stored")
	}
	if len(c.agents) != core.AgentHistoryLen {
		t.Fatalf("feed holds %d events after the refusal, want %d", len(c.agents), core.AgentHistoryLen)
	}
	if !c.agents[0].At.Equal(oldest) {
		t.Fatal("the refused event displaced the oldest retained one")
	}
	if core.HasAgentID(c.agents, "stale") {
		t.Fatal("the refused event is in the feed")
	}
	if c.agentIDs.Held("stale") {
		t.Fatal("the refused event's id is ledgered")
	}
	// The newest event still lands: only what cannot be retained is refused.
	now = base.Add((core.AgentHistoryLen + 1) * time.Second)
	if !c.RecordAgent(core.AgentEvent{At: now, ID: "fresh", Agent: "a", OutputTokens: 1}) {
		t.Fatal("a newer event was refused")
	}
}

// emit launches one goroutine per provider and waits; they must all return
// so a second frame draws no more goroutines than the first. A leaked poll
// goroutine would stay live for the rest of the run, the same way a
// production dashboard would accumulate them.
func TestEmitDoesNotLeakGoroutines(t *testing.T) {
	fp := fakeProvider{label: "x", m: &provider.Metrics{
		OutTotal: 1, Models: []core.ModelInfo{{Name: "m"}},
	}}
	c := New([]provider.Provider{fp.asProvider()}, time.Hour)
	c.SetSysFn(func() core.SysSample { return core.SysSample{MemTotal: 1} })
	ch := make(chan core.Snapshot, 1)
	// The baseline is a settled count, not a single sample: the suite runs
	// tests that leave goroutines winding down, and a baseline taken while
	// one is still live is a number emit never has to return to, which would
	// make the check pass on the first poll whatever emit does.
	before := settledGoroutines(2 * time.Second)
	c.emit(context.Background(), ch)
	<-ch
	// Poll rather than sample once: a goroutine on its way out still counts
	// until the scheduler parks it, and one that has just been spawned may
	// not have run at all when the first sample is taken.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
		if got := runtime.NumGoroutine(); got <= before {
			return
		}
	}
	t.Fatalf("goroutines after emit = %d, want no more than the settled %d before it", runtime.NumGoroutine(), before)
}

// settledGoroutines returns the goroutine count once two consecutive samples
// agree, so a baseline is never taken mid-wind-down.
func settledGoroutines(limit time.Duration) int {
	deadline := time.Now().Add(limit)
	prev := runtime.NumGoroutine()
	for time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
		got := runtime.NumGoroutine()
		if got == prev {
			return got
		}
		prev = got
	}
	return prev
}

// A replay that lands after the event has left the display ring is still the
// same logical operation, so the id has to stay dedup-able well past the
// ring's cap: a client retrying a lost 202 after a minute of backoff would
// otherwise have every line of the stream counted twice.
func TestRecordAgentIgnoresReplayAfterRingEviction(t *testing.T) {
	// Explicit timestamps, not time.Now(): a coarse clock (Windows ticks at
	// milliseconds) hands out the same instant to the whole run, and equal
	// timestamps order by id, which would sort "old" newest and pin it.
	base := time.Unix(1_700_000_000, 0).UTC()
	now := base
	c := New(nil, time.Second)
	c.SetNow(func() time.Time { return now })
	t.Cleanup(func() { c.SetNow(nil) })
	if !c.RecordAgent(core.AgentEvent{At: base, ID: "turn-1", Agent: "a", OutputTokens: 7}) {
		t.Fatal("first send reported a duplicate")
	}
	for i := range core.AgentHistoryLen {
		now = base.Add(time.Duration(i+1) * time.Second)
		c.RecordAgent(core.AgentEvent{
			At:    now,
			ID:    fmt.Sprintf("n%d", i),
			Agent: "a",
		})
	}
	if core.HasAgentID(c.agents, "turn-1") {
		t.Fatal("turn-1 still in the display ring; the test proves nothing")
	}
	// The sender never got its 202 and retries the same POST, seconds later.
	now = base.Add((core.AgentHistoryLen + 1) * time.Second)
	if c.RecordAgent(core.AgentEvent{At: base, ID: "turn-1", Agent: "a", OutputTokens: 7}) {
		t.Fatal("replay outside the display ring was stored as new")
	}
	for _, e := range c.agents {
		if e.ID == "turn-1" {
			t.Fatal("replayed event is back in the feed")
		}
	}
}

// The dedup window is bounded by time, not by the display ring: past the
// horizon an id is reusable again rather than pinning a key forever, and a
// fleet emitting faster than the horizon can evict is capped by count.
func TestRecordAgentReusesIDPastHorizon(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	now := base
	c := New(nil, time.Second)
	c.SetNow(func() time.Time { return now })
	t.Cleanup(func() { c.SetNow(nil) })
	c.RecordAgent(core.AgentEvent{At: base, ID: "old", Agent: "a"})
	now = base.Add(core.AgentIDHorizon + time.Second)
	if !c.RecordAgent(core.AgentEvent{At: now, ID: "old", Agent: "a", OutputTokens: 7}) {
		t.Fatal("id older than the dedup horizon must be reusable")
	}
	if !core.HasAgentID(c.agents, "old") {
		t.Fatal("reused id missing from the feed")
	}
	// The same id, still inside the horizon, is still a duplicate.
	if c.RecordAgent(core.AgentEvent{At: now, ID: "old", Agent: "a", OutputTokens: 7}) {
		t.Fatal("reused id accepted twice inside the horizon")
	}
}

// The ledger cannot grow without bound: a flood of distinct ids past the
// count cap evicts the oldest and the feed keeps answering in constant time.
func TestRecordAgentIDLedgerStaysBounded(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	now := base
	c := New(nil, time.Second)
	c.SetNow(func() time.Time { return now })
	t.Cleanup(func() { c.SetNow(nil) })
	const flood = 4 * core.AgentIDLedgerMax
	for i := range flood {
		now = base.Add(time.Duration(i) * time.Millisecond)
		c.RecordAgent(core.AgentEvent{At: now, ID: fmt.Sprintf("e%d", i), Agent: "a"})
	}
	if c.agentIDs.Len() > core.AgentIDLedgerMax || c.agentIDs.Tracked() > core.AgentIDLedgerMax {
		t.Fatalf("ledger holds %d ids in %d slots, want at most %d",
			c.agentIDs.Len(), c.agentIDs.Tracked(), core.AgentIDLedgerMax)
	}
	if c.agentIDs.Held("e0") {
		t.Fatal("oldest id outlived the count cap")
	}
	if !c.agentIDs.Held(fmt.Sprintf("e%d", flood-1)) {
		t.Fatal("newest id evicted")
	}
	// A replay of the newest id is still refused, whichever bound applied.
	if c.RecordAgent(core.AgentEvent{At: now, ID: fmt.Sprintf("e%d", flood-1), Agent: "a"}) {
		t.Fatal("newest id accepted twice")
	}
}

// README's ingest table names the dedup window, since that is what tells a
// sender how long a retry stays safe. A horizon bump has to land there too.
func TestREADMEDocumentsAgentIDHorizon(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("last %d minutes", int(core.AgentIDHorizon/time.Minute))
	if !strings.Contains(string(b), want) {
		t.Fatalf("README.md ingest section must say %q (matches core.AgentIDHorizon)", want)
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
	t.Cleanup(func() { c.SetNow(nil) })

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
	t.Cleanup(func() { c.SetNow(nil) })

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

func TestProcByPortDetachedFromCache(t *testing.T) {
	c := New(nil, time.Hour)
	c.procCache = []procs.Info{{PID: 1, RSS: 10, PortHint: 8000}}
	got := c.procByPort()
	got[8000] = procs.Info{PID: 99}
	c.procMu.Lock()
	cached := c.procCache[0]
	c.procMu.Unlock()
	if cached.PID != 1 {
		t.Fatalf("procByPort aliased the cache: %+v", cached)
	}
}

// A provider failing its very first poll has no history rings yet; emit must
// still include it (with the error surfaced) instead of dereferencing nil.
func TestEmitSurvivesFirstPollError(t *testing.T) {
	fp := fakeProvider{label: "dead", err: errors.New("connection refused")}
	ch := make(chan core.Snapshot, 1)
	c := New([]provider.Provider{fp.asProvider()}, time.Hour)
	// A cold cache makes emit sample the host itself, and sysmon.Sample walks
	// the vendor GPU CLIs, which the collector's own comment calls out as able
	// to take seconds. That has nothing to do with the history ring this test
	// is about, and on a loaded machine it eats the deadline below and fails a
	// run that was correct.
	c.SetSysFn(func() core.SysSample { return core.SysSample{} })
	c.procFn = func() []procs.Info { return nil }
	done := make(chan struct{})
	go func() { defer close(done); c.emit(context.Background(), ch) }()
	select {
	case snap := <-ch:
		if len(snap.Providers) != 1 {
			t.Fatalf("providers = %d, want 1", len(snap.Providers))
		}
		p := snap.Providers[0]
		if p.OK {
			t.Fatalf("failed provider reported ok: %+v", p)
		}
		if p.Err != "connection refused" {
			t.Fatalf("err = %q, want the poll error verbatim", p.Err)
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
	// A cold cache samples the real host here, down to the vendor GPU CLIs.
	// Pin both samplers so the deadline below measures emit, not the machine.
	c.SetSysFn(func() core.SysSample { return core.SysSample{} })
	c.procFn = func() []procs.Info { return nil }
	done := make(chan struct{})
	go func() { defer close(done); c.emit(context.Background(), ch) }()
	var snap core.Snapshot
	select {
	case snap = <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("emit did not produce a snapshot")
	}
	<-done
	if len(snap.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(snap.Providers))
	}
	p := snap.Providers[0]
	if p.OK {
		t.Fatalf("nil metrics reported ok: %+v", p)
	}
	if p.Err != "empty poll result" {
		t.Fatalf("err = %q, want %q", p.Err, "empty poll result")
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
	// A stub sampler: the point is where emit parks, and a cold real sample
	// (GPU vendor CLIs) runs before the send and can outlast the deadline
	// below, which would fail a test that has nothing to do with vitals.
	col.SetSysFn(func() core.SysSample { return core.SysSample{MemTotal: 1} })
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

// drain empties a buffered snapshot channel, so a check on what arrives next
// is a check on the run that starts next and not on what a stopped one left.
func drain(ch chan core.Snapshot) {
	for len(ch) > 0 {
		<-ch
	}
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

// waitUntilEmitParked blocks until an emit goroutine is actually parked on the
// snapshot send. The state line must say select in the same goroutine as the
// emit frame: emit does a cold host-vitals sample and a process-table read
// before it sends, so a match on the function name alone returns while the
// snapshot is still being built and reports a pending send that is not.
func waitUntilEmitParked(t *testing.T) {
	t.Helper()
	waitFor(t, func() bool {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		// The park is a select on the snapshot send (collector.go), so look
		// for one goroutine that is both in emit and blocked, not for the two
		// words anywhere in the dump: a goroutine merely inside this package's
		// emit (a helper, a finished run) is not parked, and matching on that
		// alone let the caller race ahead of emit.
		for _, g := range strings.Split(string(buf[:n]), "\n\ngoroutine ") {
			if strings.Contains(g, "Collector).emit") && strings.Contains(g, "[select]") {
				return true
			}
		}
		return false
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

	// The negative above only means something if this harness would have
	// seen a hit: a wave that never reaches the server passes it too. Load
	// the model back and prove the same path records one.
	fp.m = &provider.Metrics{Models: []core.ModelInfo{{Name: "m"}}}
	emitOnce(t, c)
	c.ProbeAll()
	waitFor(t, func() bool { return hits.Load() == 1 },
		"the same wave never reached the engine with a model loaded, so the negative above proved nothing")
}

// Catalog entries without VRAM must lose to a loaded model, otherwise 'p'
// JIT-loads whatever /v1/models listed first. A blank or embedding name is
// not a probe target at all: a chat completion against an embedding weight is
// a wasted billed request, and an embed-only inventory has nothing to ask.
func TestProbeAllTargetSelection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		models []core.ModelInfo
		want   string // model id the probe POSTs, empty when no probe may run
	}{
		{"prefers the loaded model", []core.ModelInfo{
			{Name: "catalog-only"},
			{Name: "loaded", SizeVRAM: 1 << 30},
		}, "loaded"},
		{"skips a blank model name", []core.ModelInfo{{Name: "   "}}, ""},
		{"skips an embedding model", []core.ModelInfo{
			{Name: "text-embedding-3-small", SizeVRAM: 1 << 30},
			{Name: "llama3"},
		}, "llama3"},
		{"skips an embed-only inventory", []core.ModelInfo{{Name: "nomic-embed-text"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got atomic.Value
			got.Store("")
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
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
				m: &provider.Metrics{Models: tc.models},
			}
			c := New([]provider.Provider{fp.asProvider()}, time.Second)
			emitOnce(t, c)
			c.ProbeAll()
			t.Cleanup(func() {
				waitFor(t, func() bool {
					c.probeMu.Lock()
					defer c.probeMu.Unlock()
					return len(c.probeInflight) == 0
				}, "probe still running")
			})
			if tc.want == "" {
				waitStay(t, 50*time.Millisecond, func() bool { return hits.Load() == 0 },
					"probe ran against a model that must be skipped")
				// The negative above only means something if this harness
				// would have seen a hit. Swap in an inventory that must
				// probe and prove the same wave path records one.
				fp.m = &provider.Metrics{Models: []core.ModelInfo{{Name: "control"}}}
				emitOnce(t, c)
				c.ProbeAll()
				waitFor(t, func() bool { return hits.Load() == 1 },
					"the same wave never reached the engine with a probeable model, so the negative above proved nothing")
				return
			}
			waitFor(t, func() bool { return got.Load().(string) == tc.want },
				"probe did not target the selected model")
		})
	}
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
	t.Cleanup(func() { c.SetNow(nil) })
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

	// The deadline is stamped on the monotonic clock (a Retry-After is a real
	// wait, not a position on the collector's timeline), so it expires by
	// ageing that clock, not by stepping the injected one.
	c.probeMu.Lock()
	for k, until := range c.probeBackoff {
		c.probeBackoff[k] = until.Add(-31 * time.Second)
	}
	c.probeMu.Unlock()
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
		// Exactly -1, 0 or 1: sort.Slice only reads the sign, so a magnitude
		// past it would sort correctly here and be compared as a count by a
		// caller that is not sorting.
		if c := core.AgentCmp(tc.a, tc.b); c != tc.want {
			t.Errorf("%s: AgentCmp = %d, want %d", tc.name, c, tc.want)
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

func TestUnloadedModelClearsCachedKVPct(t *testing.T) {
	fp := &fakeProvider{
		label: "test",
		addr:  "fake://test",
		m: &provider.Metrics{
			Models: []core.ModelInfo{{Name: "llama3"}},
			HasKV:  true,
			KVPct:  75.0,
		},
	}
	c := New([]provider.Provider{fp.asProvider()}, time.Second)
	out := make(chan core.Snapshot, 2)
	c.emit(context.Background(), out)
	snap1 := <-out
	if snap1.Providers[0].KVPct != 75.0 {
		t.Fatalf("first emit KVPct = %v, want 75.0", snap1.Providers[0].KVPct)
	}

	// Model is unloaded, engine no longer reports KV
	fp.m = &provider.Metrics{
		Models: nil,
		HasKV:  false,
	}
	c.emit(context.Background(), out)
	snap2 := <-out
	if snap2.Providers[0].KVPct != 0 {
		t.Fatalf("second emit KVPct = %v, want 0 after model unloaded", snap2.Providers[0].KVPct)
	}
}

// The other side of the cache: a model still loaded on an engine that stopped
// publishing KV keeps reporting the last figure it gave. Blanking the row
// would read as "context free" when the engine still holds it.
func TestLoadedModelKeepsLastKVPctWhenEngineStopsReporting(t *testing.T) {
	fp := &fakeProvider{
		label: "test",
		addr:  "fake://test",
		m:     &provider.Metrics{Models: []core.ModelInfo{{Name: "llama3"}}, HasKV: true, KVPct: 75.0},
	}
	c := New([]provider.Provider{fp.asProvider()}, time.Second)
	out := make(chan core.Snapshot, 2)
	c.emit(context.Background(), out)
	if got := (<-out).Providers[0].KVPct; got != 75.0 {
		t.Fatalf("first emit KVPct = %v, want 75.0", got)
	}

	fp.m = &provider.Metrics{Models: []core.ModelInfo{{Name: "llama3"}}, HasKV: false}
	c.emit(context.Background(), out)
	if got := (<-out).Providers[0].KVPct; got != 75.0 {
		t.Fatalf("second emit KVPct = %v, want the last reported 75.0 while the model stays loaded", got)
	}
}

// A poll error is a decoder's, and encoding/json embeds the whole offending
// literal in an UnmarshalTypeError: an engine answering /api/ps with a
// megabyte-long number puts that megabyte in the error. The snapshot field
// that error reaches is re-rendered on every frame and written whole into
// --json, so it is capped and folded at the boundary that stores it.
func TestProviderErrorIsCappedAndOneLine(t *testing.T) {
	huge := strings.Repeat("9", 200_000)
	multi := errors.New("dial tcp 10.0.0.5:8000: connection refused\ndial tcp 10.0.0.6:8000: connection refused")
	p := fakeProvider{label: "ollama", addr: "http://127.0.0.1:11434", err: errors.New("json: cannot unmarshal number " + huge + " into Go value")}
	ch := make(chan core.Snapshot, 1)
	c := New([]provider.Provider{p.asProvider()}, time.Hour)
	c.emit(context.Background(), ch)
	snap := <-ch
	if len(snap.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(snap.Providers))
	}
	if msg := snap.Providers[0].Err; !strings.Contains(msg, "cannot unmarshal") {
		t.Fatalf("provider error = %q, want the poll's own reason", msg)
	}
	if got := len([]rune(snap.Providers[0].Err)); got > core.SnippetCap {
		t.Errorf("provider error is %d characters, want at most SnippetCap (%d)", got, core.SnippetCap)
	}

	p2 := fakeProvider{label: "vllm", addr: "http://127.0.0.1:8000", err: multi}
	ch2 := make(chan core.Snapshot, 1)
	c2 := New([]provider.Provider{p2.asProvider()}, time.Hour)
	c2.emit(context.Background(), ch2)
	snap2 := <-ch2
	if msg := snap2.Providers[0].Err; !strings.Contains(msg, "connection refused") {
		t.Fatalf("provider error = %q, want the poll's own reason", msg)
	}
	if msg := snap2.Providers[0].Err; strings.ContainsAny(msg, "\n\t") {
		t.Errorf("provider error %q kept a line break; the snapshot row it feeds would print as two lines", msg)
	}
}

// The fold is memoized per provider key, so the cache has to miss on both
// halves of its key: a downed engine that changes its error, and a process
// whose home moves. Serving either from the memo repeats the first error
// forever, or keeps redacting against the account the fold was made for.
func TestProviderErrorFoldMemoFollowsErrorAndHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "private-user")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	fp := &fakeProvider{label: "ollama", addr: "http://127.0.0.1:11434",
		err: errors.New("cannot open " + filepath.Join(home, "models", "m.safetensors"))}
	c := New([]provider.Provider{fp.asProvider()}, time.Hour)
	out := make(chan core.Snapshot, 1)
	c.emit(context.Background(), out)
	if got := (<-out).Providers[0].Err; strings.Contains(got, "private-user") {
		t.Fatalf("poll error kept the account name: %q", got)
	}

	// Same key, new error: the memo must miss, not serve the folded first one.
	fp.err = errors.New("dial tcp 127.0.0.1:11434: connect: connection refused")
	c.emit(context.Background(), out)
	if got := (<-out).Providers[0].Err; !strings.Contains(got, "connection refused") {
		t.Fatalf("stale fold served after the error changed: %q", got)
	}

	// Same error, new home: the memo must miss again, or the fold keeps
	// redacting against the account it was made for.
	other := filepath.Join(t.TempDir(), "other-user")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	fp.err = errors.New("cannot open " + filepath.Join(other, "models", "m.safetensors"))
	t.Setenv("HOME", other)
	t.Setenv("USERPROFILE", other)
	c.emit(context.Background(), out)
	if got := (<-out).Providers[0].Err; strings.Contains(got, "other-user") || !strings.Contains(got, "models") {
		t.Fatalf("error after the home moved = %q, want the new home folded", got)
	}
}

// The offset estimator takes the smallest lead it is shown, and a lead of
// zero is the one reading it can never recover from: a retried POST carries
// the stamp of the original send, so it reads as an on-timeline clock. A
// duplicate is refused before the ledger is written, so the retry that would
// otherwise pin the correction off cannot touch it.
func TestRecordAgentDuplicateDoesNotZeroTheOffset(t *testing.T) {
	c := New(nil, time.Second)
	base := time.Now()
	now := base
	c.SetNow(func() time.Time { return now })
	t.Cleanup(func() { c.SetNow(nil) })

	const fast = 90 * time.Second
	ev := core.AgentEvent{At: now.Add(fast), ID: "turn-1", Agent: "remote", OutputTokens: 40}
	if !c.RecordAgent(ev) {
		t.Fatal("the first event was not retained")
	}
	if got := c.agentSkews["remote"]; got != fast {
		t.Fatalf("offset = %v, want %v", got, fast)
	}

	// The 202 was lost, so the sender replays the same event. It is refused
	// as a duplicate, and the offset it would have read as zero is not taken.
	now = base.Add(30 * time.Second)
	if c.RecordAgent(ev) {
		t.Fatal("the replay was reported as retained")
	}
	if got := c.agentSkews["remote"]; got != fast {
		t.Fatalf("offset after a replay = %v, want the %v reading to survive", got, fast)
	}

	// The sender keeps sending, so the correction stays in force: the next
	// event lands on this machine's timeline rather than 90s in its future.
	if !c.RecordAgent(core.AgentEvent{At: now.Add(fast), ID: "turn-2", Agent: "remote", OutputTokens: 40}) {
		t.Fatal("the follow-up event was not retained")
	}
	if last := c.agents[len(c.agents)-1].At; last.After(now) {
		t.Fatalf("newest event = %v, in this machine's future", last)
	}
}

// The offset ages out with the id ledger, whether or not a smaller lead
// arrives. An agent that stops reporting produces no reading to trigger the
// sweep, and an offset left in force for the life of the process subtracts a
// clock error measured hours ago from every event since. Once it is gone a
// later, larger lead is a fresh reading and reseeds the correction: the
// estimator holds a minimum, and a minimum of nothing is nothing.
func TestRecordAgentOffsetAgesOutWithTheHorizon(t *testing.T) {
	c := New(nil, time.Second)
	base := time.Now()
	now := base
	c.SetNow(func() time.Time { return now })
	t.Cleanup(func() { c.SetNow(nil) })

	const fast = 60 * time.Second
	c.RecordAgent(core.AgentEvent{At: now.Add(fast), Agent: "remote", OutputTokens: 40})
	if got := c.agentSkews["remote"]; got != fast {
		t.Fatalf("offset = %v, want %v", got, fast)
	}

	// One more event inside the horizon, so the sweep is driven by a reading
	// rather than by the event after the quiet spell.
	now = base.Add(time.Minute)
	c.RecordAgent(core.AgentEvent{At: now.Add(fast), Agent: "remote", OutputTokens: 40})

	// The host is quiet past the horizon, then returns with its clock error
	// grown to 3 minutes, as a host that lost NTP for a while does. The
	// aged-out offset must not cap the new reading at the old one.
	now = base.Add(core.AgentIDHorizon + time.Minute)
	const worse = 3 * time.Minute
	if !c.RecordAgent(core.AgentEvent{At: now.Add(worse), Agent: "remote", OutputTokens: 40}) {
		t.Fatal("the event after the quiet spell was not retained")
	}
	if _, ok := c.agentSkews["remote"]; !ok {
		t.Fatal("the offset aged out and no fresh reading replaced it")
	}
	if got := c.agentSkews["remote"]; got != worse {
		t.Fatalf("offset after ageing = %v, want a fresh %v", got, worse)
	}
	if last := c.agents[len(c.agents)-1].At; last.After(now) {
		t.Fatalf("newest event = %v, in this machine's future", last)
	}
}

// probeWaveGap spaces waves but never bounded their width: on a fleet wider
// than the cap, one --probe tick fired a generation against every backend at
// once, and an OpenAI-compatible gateway bills every one of them. A wave takes
// at most probeWaveMax backends, and the ones it holds back are picked up by
// the next wave instead of starving behind the first few.
func TestProbeAllCapsWaveWidthAndRotates(t *testing.T) {
	oldGap := probeWaveGap
	probeWaveGap = 0
	defer func() { probeWaveGap = oldGap }()

	// Spelled out rather than read from probeWaveMax: a cap that drifts is
	// the regression, and an assertion that moves with it cannot catch one.
	const wantPerWave = 4
	const fleet = wantPerWave*2 + 1
	var mu sync.Mutex
	probed := map[string]bool{}
	var inflight, peak atomic.Int32
	// One server stands in for the fleet, tagged by query so each backend
	// keeps its own provider key: a real fleet is a list of distinct
	// endpoints, and collapsing them to one would test dedup instead.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		for {
			hi := peak.Load()
			if n <= hi || peak.CompareAndSwap(hi, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond) // hold the generation open
		inflight.Add(-1)
		mu.Lock()
		probed[r.URL.RawQuery] = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":1}}`)
	}))
	defer srv.Close()

	provs := make([]provider.Provider, 0, fleet)
	for i := range fleet {
		provs = append(provs, (&fakeProvider{
			label: fmt.Sprintf("engine-%02d", i),
			addr:  srv.URL + "/?i=" + strconv.Itoa(i),
			kind:  core.KindOpenAI,
			m:     &provider.Metrics{Models: []core.ModelInfo{{Name: "m"}}},
		}).asProvider())
	}
	c := New(provs, time.Second)
	if len(c.providers) != fleet {
		t.Fatalf("fleet collapsed to %d providers, want %d", len(c.providers), fleet)
	}
	for _, p := range c.providers {
		c.lastModel[providerKey(p)] = "m"
	}

	covered := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(probed)
	}
	drain := func() {
		t.Helper()
		waitFor(t, func() bool {
			c.probeMu.Lock()
			defer c.probeMu.Unlock()
			return len(c.probeInflight) == 0
		}, "a wave never drained")
	}
	defer drain()
	wave := func() {
		t.Helper()
		drain()
		c.ProbeAll()
	}

	wave()
	waitStay(t, 60*time.Millisecond, func() bool {
		c.probeMu.Lock()
		defer c.probeMu.Unlock()
		return len(c.probeInflight) <= wantPerWave
	}, "wave exceeded the per-wave backend cap in flight")
	if p := peak.Load(); p > wantPerWave {
		t.Fatalf("peak concurrent generations = %d, want <= %d", p, wantPerWave)
	}
	if got := covered(); got > wantPerWave {
		t.Fatalf("one wave reached %d backends, want <= %d", got, wantPerWave)
	}

	for range fleet {
		wave()
		if covered() == fleet {
			break
		}
	}
	if got := covered(); got != fleet {
		t.Fatalf("probed %d of %d backends; the ones past the cap starved", got, fleet)
	}
}

// The --probe ticker bills: every wave is a real generation on whatever the
// backend is. --probe 1 would otherwise re-run the same wave every second, so
// the cadenced path holds each backend to ProbeBackendGap. ProbeAll ('p') is
// the operator asking for a number now and is not held to it.
func TestProbeCadencedBackendGap(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, "{\"response\":\"one\",\"done\":true,\"eval_count\":1,\"eval_duration\":1000000}\n")
	}))
	defer srv.Close()

	oldWave := probeWaveGap
	probeWaveGap = 0 // isolate the per-backend floor from the wave gate
	defer func() { probeWaveGap = oldWave }()

	c := New([]provider.Provider{(&fakeProvider{label: "c", addr: srv.URL}).asProvider()}, time.Second)
	c.lastModel[srv.URL] = "m"

	idle := func() {
		t.Helper()
		waitFor(t, func() bool {
			c.probeMu.Lock()
			defer c.probeMu.Unlock()
			return len(c.probeInflight) == 0
		}, "probe never cleared")
	}

	c.ProbeCadenced()
	waitFor(t, func() bool { return hits.Load() == 1 }, "first cadenced wave never reached the engine")
	idle()

	// A 1s --probe tick inside the floor finds nothing to do: this is the
	// generation the gap is refusing to buy.
	c.ProbeCadenced()
	waitStay(t, 50*time.Millisecond, func() bool { return hits.Load() == 1 },
		"cadenced wave re-probed a backend inside ProbeBackendGap")

	// The operator's press is not gated.
	c.ProbeAll()
	waitFor(t, func() bool { return hits.Load() == 2 }, "manual wave was held to the cadence floor")
	idle()

	// ... and it arms the floor in turn, so the ticker does not buy the same
	// generation again right behind it.
	c.ProbeCadenced()
	waitStay(t, 50*time.Millisecond, func() bool { return hits.Load() == 2 },
		"a manual probe did not arm ProbeBackendGap for the ticker")
}
