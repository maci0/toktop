// Package collector polls providers on an interval, derives token rates from
// monotonic counters, and fans snapshots out to the UI.
package collector

import (
	"context"
	"maps"
	"net"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/probe"
	"github.com/maci0/toktop/internal/procs"
	"github.com/maci0/toktop/internal/provider"
	"github.com/maci0/toktop/internal/sysmon"
)

const (
	emaAlpha = 0.35
)

type prevSample struct {
	at       time.Time
	outTotal float64
	inTotal  float64
	outEMA   float64
	inEMA    float64
}

type Collector struct {
	providers []provider.Provider
	interval  time.Duration
	procFn    func() []procs.Info
	procMu    sync.Mutex
	procCache []procs.Info

	sysMu       sync.Mutex // guards sysFn + sysCache
	sysFn       func() core.SysSample
	sysCache    *core.SysSample // last good sample from the background poller
	sysSampling sync.Mutex      // serializes sampling; vendor CLIs take seconds

	mu        sync.Mutex
	histOut   map[string]*timedRing
	histIn    map[string]*timedRing
	prev      map[string]prevSample
	lastModel map[string]string // endpoint -> model to probe
	kvPct     map[string]float64
	agents    []core.AgentEvent
	probes    []core.ProbeSample
	started   time.Time
	baseCtx   context.Context // set by Run; bounds ad-hoc probes past shutdown

	probeMu       sync.Mutex           // guards the probe fan-out state below
	lastProbeWave time.Time            // wave gate: see probeWaveGap
	probeInflight map[string]bool      // "base|model" -> generation running
	probeBackoff  map[string]time.Time // "base|model" -> earliest next probe (429/503)

	now func() time.Time // snapshot/probe/event stamps; nil means time.Now
}

// New polls providers every interval. Host vitals come from sysmon; call
// SetSysFn before Run when merging remote readings onto the local sample.
func New(providers []provider.Provider, interval time.Duration) *Collector {
	c := &Collector{
		providers:     providers,
		interval:      interval,
		sysFn:         sysmon.Sample,
		histOut:       map[string]*timedRing{},
		histIn:        map[string]*timedRing{},
		prev:          map[string]prevSample{},
		lastModel:     map[string]string{},
		kvPct:         map[string]float64{},
		probeInflight: map[string]bool{},
		probeBackoff:  map[string]time.Time{},
		now:           time.Now,
		started:       time.Now(),
	}
	// CPU tick deltas use this clock, not a second wall-clock read inside
	// the sampler: a frozen or stepped now must move dt the same way emit's
	// snapshot stamp does.
	c.procFn = func() []procs.Info { return procSampler.SnapshotAt(c.instant()) }
	return c
}

// SetNow overrides the clock used to stamp snapshots, probe-wave gating,
// and agent events that arrive without a timestamp. Call before Run.
func (c *Collector) SetNow(fn func() time.Time) {
	if fn == nil {
		fn = time.Now
	}
	c.now = fn
	c.started = fn()
}

func (c *Collector) instant() time.Time { return c.now() }

// procSampler is the shared engine-process sampler; nil-safe when the
// platform has no process table access.
var procSampler = procs.NewSampler()

// SetSysFn overrides the host-vitals sampler (used for ssh targets whose
// stats merge local + remote readings). Call before Run.
func (c *Collector) SetSysFn(fn func() core.SysSample) {
	c.sysMu.Lock()
	c.sysFn = fn
	c.sysMu.Unlock()
}

// startSysPoller refreshes host vitals in the background; emit never blocks
// on it (GPU vendor CLIs can take seconds and would stall every frame). Run
// warms the cache before emitting, so this first pass is a cache hit.
func (c *Collector) startSysPoller(ctx context.Context) {
	go func() {
		t := time.NewTicker(c.interval)
		defer t.Stop()
		c.sampleSys(false) // skip when Run already warmed the cache
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.sampleSys(true)
			}
		}
	}()
}

// sampleSys runs the vitals sampler. Concurrent callers serialize on
// sysSampling so slow tooling is never invoked twice at once; with force
// unset an already-warm cache short-circuits without sampling.
func (c *Collector) sampleSys(force bool) *core.SysSample {
	c.sysSampling.Lock()
	defer c.sysSampling.Unlock()
	c.sysMu.Lock()
	fn, cached := c.sysFn, c.sysCache
	c.sysMu.Unlock()
	if !force && (cached != nil || fn == nil) {
		return cached
	}
	if fn == nil {
		return nil
	}
	s := fn()
	c.sysMu.Lock()
	c.sysCache = &s
	c.sysMu.Unlock()
	return &s
}

// sysSnapshot returns the freshest vitals sample: a warm-cache read that
// never waits on sampling, falling back to one serialized sample before the
// first background refresh has landed.
func (c *Collector) sysSnapshot() *core.SysSample {
	c.sysMu.Lock()
	cached := c.sysCache
	c.sysMu.Unlock()
	if cached != nil {
		return cached
	}
	return c.sampleSys(false)
}

// startProcPoller refreshes the process table in the background; emit never
// blocks on it (Windows CIM enumeration takes seconds).
func (c *Collector) startProcPoller(ctx context.Context) {
	go func() {
		t := time.NewTicker(c.interval)
		defer t.Stop()
		refresh := func() {
			if infos := c.procFn(); infos != nil {
				c.procMu.Lock()
				c.procCache = infos
				c.procMu.Unlock()
			}
		}
		refresh()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refresh()
			}
		}
	}()
}

// procSnapshot returns the latest cached engine processes, detached from the
// poller's buffer so a later refresh cannot mutate a snapshot already in emit.
func (c *Collector) procSnapshot() []procs.Info {
	c.procMu.Lock()
	defer c.procMu.Unlock()
	return slices.Clone(c.procCache)
}

// Run polls until ctx is cancelled, emitting one Snapshot per interval.
func (c *Collector) Run(ctx context.Context, out chan<- core.Snapshot) {
	c.mu.Lock()
	c.baseCtx = ctx
	c.mu.Unlock()
	c.startProcPoller(ctx)
	// Warm the vitals cache before the first emit so that frame is a cache
	// hit. GPU vendor CLIs can take seconds; sampling them inside emit
	// would delay it. RecordAgent is not pinned: sysSnapshot runs outside c.mu.
	c.sampleSys(false)
	c.startSysPoller(ctx)
	t := time.NewTicker(c.interval)
	defer t.Stop()
	c.emit(ctx, out) // immediate first frame
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.emit(ctx, out)
		}
	}
}

func (c *Collector) emit(ctx context.Context, out chan<- core.Snapshot) {
	type result struct {
		m   *provider.Metrics
		err error
	}
	results := make([]result, len(c.providers))
	var wg sync.WaitGroup
	for i, p := range c.providers {
		wg.Go(func() {
			// Bound by PollTimeout and by the Run context so shutdown does
			// not wait out a stalled scrape the way ProbeAll already
			// cancels in-flight generations.
			pctx, cancel := context.WithTimeout(ctx, provider.PollTimeout)
			defer cancel()
			m, err := p.Poll(pctx)
			results[i] = result{m, err}
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}

	now := c.instant()
	snap := core.Snapshot{At: now, Uptime: now.Sub(c.started)}
	// Vitals and the process table are independent of c.mu. Sampling them
	// inside the critical section would stall RecordAgent/ProbeAll for the
	// whole vendor-CLI sweep on a cold cache.
	sys := c.sysSnapshot()
	byPort := procsByPort(c.procSnapshot())

	c.mu.Lock()
	snap.Agents = append([]core.AgentEvent(nil), c.agents...)
	snap.Probes = append([]core.ProbeSample(nil), c.probes...)
	snap.Sys = cloneSys(sys)
	for i, r := range results {
		p := c.providers[i]
		ps := core.ProviderSnapshot{
			Label: p.Label,
			Kind:  p.Kind,
			Addr:  p.Addr,
		}
		// Per-provider state is keyed by endpoint, not display label (see
		// providerKey): labels repeat across instances of the same engine
		// kind, and shared baselines or histories would mix their counters.
		key := providerKey(p)
		if r.err != nil {
			ps.Err = r.err.Error()
		} else if r.m == nil {
			ps.Err = "empty poll result"
		} else {
			ps.OK = true
			ps.Models = r.m.Models
			ps.Running = r.m.Running
			ps.Waiting = r.m.Waiting
			ps.TTFTms = r.m.TTFTms
			if port := urlPort(p.Addr); port > 0 {
				if proc, ok := byPort[port]; ok {
					ps.PID, ps.ProcRSS, ps.ProcCPU = proc.PID, proc.RSS, proc.CPUPct
				}
			}
			if r.m.Version != "" {
				ps.Version = r.m.Version
			}
			if r.m.HasKV {
				ps.KVPct = r.m.KVPct
				c.kvPct[key] = ps.KVPct
			} else {
				ps.KVPct = c.kvPct[key]
			}
			if name := probe.SelectModel(r.m.Models); name != "" {
				c.lastModel[key] = name
			} else {
				// Successful poll with nothing loaded: a stale id would
				// make the next 'p' JIT-load (or bill) a cold model.
				delete(c.lastModel, key)
			}
			outPS, inPS := c.rates(key, r.m, now)
			ps.OutTokPS = outPS
			ps.InTokPS = inPS
			c.ring(c.histOut, key).push(outPS, now, c.interval)
			c.ring(c.histIn, key).push(inPS, now, c.interval)
		}
		// ring() (not a bare map index): a provider whose first poll failed
		// has no history yet, and indexing the map there would deref nil.
		outR, inR := c.ring(c.histOut, key), c.ring(c.histIn, key)
		ps.OutHist, ps.OutT0 = outR.copy(), outR.t0
		ps.InHist, ps.InT0 = inR.copy(), inR.t0
		snap.Providers = append(snap.Providers, ps)
	}
	c.mu.Unlock()
	// Send outside the critical section: a stalled consumer must neither pin
	// emit past cancellation nor freeze RecordAgent/RecordProbe/ProbeAll
	// behind c.mu while this send waits for buffer space.
	select {
	case out <- snap:
	case <-ctx.Done():
	}
}

// procsByPort indexes engine processes by their effective listen port;
// on a collision the first sample wins.
func procsByPort(infos []procs.Info) map[int]procs.Info {
	byPort := make(map[int]procs.Info, len(infos))
	for _, p := range infos {
		if port := p.ListenPort(); port > 0 {
			if _, dup := byPort[port]; !dup {
				byPort[port] = p
			}
		}
	}
	return byPort
}

// rates derives smoothed tok/s deltas since the previous sample.
func (c *Collector) rates(key string, m *provider.Metrics, now time.Time) (outPS, inPS float64) {
	pv, had := c.prev[key]
	if !had {
		c.prev[key] = prevSample{at: now, outTotal: m.OutTotal, inTotal: m.InTotal}
		return 0, 0
	}
	dt := now.Sub(pv.at).Seconds()
	if dt <= 0 {
		// Zero elapsed time cannot yield a rate: 0/0 is NaN and n/0 is
		// +Inf, and either would poison this EMA and every later sample
		// derived from it. Hold the prior rate; keep the older baseline so
		// the next real interval accounts for these tokens too.
		return pv.outEMA, pv.inEMA
	}
	rawOut := max((m.OutTotal-pv.outTotal)/dt, 0) // clamp on counter reset
	rawIn := max((m.InTotal-pv.inTotal)/dt, 0)
	if m.DirectOutPS > 0 { // trust the engine's own tok/s gauge when present
		rawOut = m.DirectOutPS
	}
	outPS = ema(pv.outEMA, rawOut)
	inPS = ema(pv.inEMA, rawIn)
	c.prev[key] = prevSample{
		at: now, outTotal: m.OutTotal, inTotal: m.InTotal,
		outEMA: outPS, inEMA: inPS,
	}
	return outPS, inPS
}

func ema(prev, raw float64) float64 { return prev*(1-emaAlpha) + raw*emaAlpha }

// timedRing is a value history carrying the wall-clock time of its oldest
// sample so charts can place every point on an absolute time axis.
//
// The backing buffer is allocated once at HistoryLen and reused head-first:
// after warm-up push never allocates or copies, where sliding a slice
// (vals = vals[1:]) would realloc on every push for the life of the ring.
type timedRing struct {
	buf  []float64 // fixed capacity HistoryLen, samples in insertion order
	head int       // counts fills while filling; then indexes the oldest element
	t0   time.Time
}

func (r *timedRing) push(v float64, now time.Time, interval time.Duration) {
	if r.buf == nil { // one reservation for the ring's whole life
		r.buf = make([]float64, 0, core.HistoryLen)
	}
	if len(r.buf) < core.HistoryLen { // filling: keep appending in order
		r.buf = append(r.buf, v)
	} else {
		// head is always in [0, HistoryLen) here: filling leaves it at 0,
		// and each overwrite below wraps it after incrementing.
		r.buf[r.head] = v // overwrite the oldest sample
		r.head++
		if r.head == core.HistoryLen {
			r.head = 0
		}
	}
	// Anchor hist[0] to this push instead of sliding it one interval per
	// sample: pushes are not guaranteed evenly spaced (a scrape may take up
	// to PollTimeout, and a stalled emit lets ticks coalesce), and a slid t0
	// would drift behind wall-clock time forever, shifting the whole chart
	// axis into the past. Even spacing reproduces the slide exactly.
	r.t0 = now.Add(-time.Duration(len(r.buf)-1) * interval)
}

// copy returns the samples in insertion order (oldest first), detached from
// the ring so snapshots stay stable across later pushes.
func (r *timedRing) copy() []float64 {
	if len(r.buf) == 0 {
		return nil
	}
	out := make([]float64, len(r.buf))
	n := copy(out, r.buf[r.head:])
	copy(out[n:], r.buf[:r.head])
	return out
}

func (c *Collector) ring(m map[string]*timedRing, key string) *timedRing {
	r, ok := m[key]
	if !ok {
		r = &timedRing{}
		m[key] = r
	}
	return r
}

// RecordAgent stores an agent event (called from the ingest server).
// Events come from many senders whose clocks disagree (the ingest endpoint
// can face a LAN), so arrival order is not time order; every consumer reads
// Agents newest-last (see core.Snapshot), so keep them sorted by timestamp
// the way the probe ring is. A non-empty ID that is already in the retained
// window is ignored, so a retried POST of the same event does not double-count.
func (c *Collector) RecordAgent(ev core.AgentEvent) {
	if ev.At.IsZero() {
		ev.At = c.instant()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if core.HasAgentID(c.agents, ev.ID) {
		return
	}
	c.agents = insertSorted(append(c.agents, ev), agentCmp)
	if len(c.agents) > core.AgentHistoryLen {
		c.agents = c.agents[len(c.agents)-core.AgentHistoryLen:]
	}
}

// RecordProbe stores a probe sample. Probes complete concurrently and can
// finish out of launch order, but every consumer (probe charts, the "last"
// readout) assumes newest-last ordering: keep the ring sorted by timestamp.
func (c *Collector) RecordProbe(s core.ProbeSample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probes = insertSorted(append(c.probes, s), probeCmp)
	if len(c.probes) > core.ProbeHistoryLen {
		c.probes = c.probes[len(c.probes)-core.ProbeHistoryLen:]
	}
}

func agentCmp(a, b core.AgentEvent) int {
	if c := a.At.Compare(b.At); c != 0 {
		return c
	}
	if c := strings.Compare(a.Agent, b.Agent); c != 0 {
		return c
	}
	if c := strings.Compare(a.ID, b.ID); c != 0 {
		return c
	}
	return strings.Compare(a.Note, b.Note)
}

func probeCmp(a, b core.ProbeSample) int {
	if c := a.At.Compare(b.At); c != 0 {
		return c
	}
	if c := strings.Compare(a.Addr, b.Addr); c != 0 {
		return c
	}
	return strings.Compare(a.Model, b.Model)
}

// insertSorted places the element just appended to s (sorted before the
// append) at its stable position: after every element cmp reports as less
// than or equal to it. Time is the primary key; equal timestamps then order
// by identity so concurrent RecordProbe/RecordAgent completions cannot
// shuffle a replay. One binary search plus one shift replaces a full
// re-sort per event; the ingest path holds c.mu across this, so every
// comparison saved unblocks emit and ProbeAll sooner.
func insertSorted[T any](s []T, cmp func(a, b T) int) []T {
	if len(s) == 0 {
		panic("insertSorted: empty slice, caller must append first")
	}
	i := len(s) - 1
	ev := s[i]
	dst := sort.Search(i, func(j int) bool { return cmp(s[j], ev) > 0 })
	copy(s[dst+1:], s[dst:])
	s[dst] = ev
	return s
}

// providerKey is the per-provider state key: the endpoint when known, else
// the display label. Endpoints are unique per instance; labels repeat across
// instances of the same engine kind.
func providerKey(p provider.Provider) string {
	if addr := p.Addr; addr != "" {
		return addr
	}
	return p.Label
}

// probeWaveGap is the minimum spacing between probe waves. The UI's 'p' key
// auto-repeats when held and the --probe ticker can land on top of a manual
// wave; without a gate each press stacks another concurrent generation on
// every backend. Probing distorts the very metrics it measures, and
// OpenAI-compatible gateways (LiteLLM and friends) may bill every probe
// token, so waves also never overlap per backend.
var probeWaveGap = 500 * time.Millisecond

// ProbeAll launches one probe against every known backend, asynchronously.
// Probes ride the Run context so shutdown cancels in-flight generations
// instead of leaving them running for the client's full timeout.
func (c *Collector) ProbeAll() {
	c.mu.Lock()
	var targets []probe.Request
	for _, p := range c.providers {
		if model := c.lastModel[providerKey(p)]; model != "" {
			targets = append(targets, probe.Request{Kind: p.Kind, Base: p.Addr, Model: model})
		}
	}
	ctx := c.baseCtx
	c.mu.Unlock()
	if ctx == nil { // probed before Run: nothing bounds these but the client timeout
		ctx = context.Background()
	}

	now := c.instant()
	c.probeMu.Lock()
	if now.Sub(c.lastProbeWave) < probeWaveGap {
		c.probeMu.Unlock()
		return
	}
	c.lastProbeWave = now
	var live []probe.Request
	for _, t := range targets {
		key := t.Base + "|" + t.Model
		if c.probeInflight[key] { // one generation per backend at a time
			continue
		}
		if until, ok := c.probeBackoff[key]; ok && now.Before(until) {
			continue // 429/503: wait out Retry-After before POSTing again
		}
		delete(c.probeBackoff, key)
		c.probeInflight[key] = true
		live = append(live, t)
	}
	c.probeMu.Unlock()

	// One stamp for the whole wave: probe.Run measures TTFT against the
	// wall clock (real I/O), but the sample's At must follow the collector
	// clock or a frozen/seeded replay would carry a second timeline.
	for _, t := range live {
		go func(t probe.Request) {
			defer func() {
				c.probeMu.Lock()
				delete(c.probeInflight, t.Base+"|"+t.Model)
				c.probeMu.Unlock()
			}()
			// Re-read inside the goroutine: ProbeAll can race Run's first
			// assignment of baseCtx, and a copied nil would bound the
			// generation with Background (surviving shutdown).
			c.mu.Lock()
			pctx := c.baseCtx
			c.mu.Unlock()
			if pctx == nil {
				pctx = ctx
			}
			s := probe.Run(pctx, t)
			s.At = now
			if s.RetryAfter > 0 {
				c.probeMu.Lock()
				c.probeBackoff[t.Base+"|"+t.Model] = c.instant().Add(s.RetryAfter)
				c.probeMu.Unlock()
			}
			c.RecordProbe(s)
		}(t)
	}
}

// cloneSys copies a vitals sample so a snapshot handed to the UI does not
// alias the poller's cache. Drivers/Temps/GPUs/NPUs are reference fields:
// publishing the cache pointer would let a later sample (or an in-place
// overlay) race a render of an earlier frame.
func cloneSys(s *core.SysSample) *core.SysSample {
	if s == nil {
		return nil
	}
	out := *s
	out.Drivers = maps.Clone(s.Drivers)
	out.Temps = slices.Clone(s.Temps)
	out.GPUs = slices.Clone(s.GPUs)
	out.NPUs = slices.Clone(s.NPUs)
	return &out
}

// urlPort extracts the TCP port from a backend URL. Non-http(s) addresses
// (tests use fake://, and a blank Addr is not a listener) must not fall
// through to port 80: that would attach GPUStack-on-80 process stats to
// an unrelated provider.
func urlPort(addr string) int {
	u, err := url.Parse(addr)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return 0
	}
	if _, port, err := net.SplitHostPort(u.Host); err == nil {
		if p, err := strconv.Atoi(port); err == nil && p >= 1 && p <= 65535 {
			return p
		}
		return 0
	}
	if u.Scheme == "https" {
		return 443
	}
	return 80
}
