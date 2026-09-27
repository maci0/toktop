// Package collector polls providers on an interval, derives token rates from
// monotonic counters, and fans snapshots out to the UI.
package collector

import (
	"context"
	"maps"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/probe"
	"github.com/maci0/toktop/internal/procs"
	"github.com/maci0/toktop/internal/provider"
	"github.com/maci0/toktop/internal/sysmon"
	"golang.org/x/text/unicode/norm"
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
	agentIDs  map[string]struct{} // NFC ids of the retained agents, mirroring c.agents
	probes    []core.ProbeSample
	started   time.Time
	baseCtx   context.Context // set by Run; bounds ad-hoc probes past shutdown

	probeMu       sync.Mutex // guards the probe fan-out state below
	lastProbeWave time.Time  // wave gate: see probeWaveGap
	probeInflight map[string]bool
	probeBackoff  map[string]time.Time

	now func() time.Time // always non-nil: New sets time.Now, SetNow normalizes nil
}

// New polls providers every interval. Host vitals come from sysmon; call
// SetSysFn before Run when merging remote readings onto the local sample.
// Duplicate non-empty endpoints collapse to their first occurrence: --add
// can name the same engine twice and the collector keys rate baselines,
// histories and probe state by endpoint, so a repeat would be polled twice,
// summed twice by the UI aggregates, and push duplicate history samples.
func New(providers []provider.Provider, interval time.Duration) *Collector {
	seen := map[string]bool{}
	deduped := providers[:0:0]
	for _, p := range providers {
		key := providerKey(p)
		if key != "" && !seen[key] {
			seen[key] = true
			deduped = append(deduped, p)
		}
	}
	providers = deduped
	if interval <= 0 {
		interval = time.Second
	}
	// One clock read for both the default and the start stamp: a second read
	// would leave started a hair later than the clock it is compared against,
	// so uptime and the rate baselines would sit on two origins.
	now := time.Now()
	c := &Collector{
		providers:     providers,
		interval:      interval,
		sysFn:         sysmon.Sample,
		histOut:       map[string]*timedRing{},
		histIn:        map[string]*timedRing{},
		prev:          map[string]prevSample{},
		lastModel:     map[string]string{},
		kvPct:         map[string]float64{},
		agentIDs:      map[string]struct{}{},
		probeInflight: map[string]bool{},
		probeBackoff:  map[string]time.Time{},
		now:           time.Now,
		started:       now,
	}
	// CPU tick deltas use this clock, not a second wall-clock read inside
	// the sampler: a frozen or stepped now must move dt the same way emit's
	// snapshot stamp does.
	c.procFn = func() []procs.Info { return procSampler.SnapshotAt(c.now()) }
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

// instant is the collector clock, so a SetNow override reaches every call
// site.
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
func (c *Collector) startSysPoller(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(c.interval)
		defer t.Stop()
		c.sampleSys(false) // skip when Run already warmed the cache
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if ctx.Err() != nil {
					return
				}
				c.sampleSys(true)
			}
		}
	}()
	return done
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
func (c *Collector) startProcPoller(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
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
				if ctx.Err() != nil {
					return
				}
				refresh()
			}
		}
	}()
	return done
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
	procDone := c.startProcPoller(ctx)
	defer func() { <-procDone }()
	// Warm the vitals cache before the first emit so that frame is a cache
	// hit. GPU vendor CLIs can take seconds; sampling them inside emit
	// would delay it. RecordAgent is not pinned: sysSnapshot runs outside c.mu.
	c.sampleSys(false)
	sysDone := c.startSysPoller(ctx)
	defer func() { <-sysDone }()
	t := time.NewTicker(c.interval)
	defer t.Stop()
	c.emit(ctx, out) // immediate first frame
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if ctx.Err() != nil {
				return
			}
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
	snap.Agents = slices.Clone(c.agents)
	snap.Probes = slices.Clone(c.probes)
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
		// ring() (not a bare map index): a provider whose first poll failed
		// has no history yet, and indexing the map there would deref nil.
		outR, inR := c.ring(c.histOut, key), c.ring(c.histIn, key)
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
			if port := urlPort(p.Addr); port > 0 && isLoopbackURL(p.Addr) {
				if proc, ok := byPort[port]; ok {
					ps.PID, ps.ProcRSS, ps.ProcCPU = proc.PID, proc.RSS, proc.CPUPct
				}
			}
			if r.m.Version != "" {
				ps.Version = r.m.Version
			}
			if name := probe.SelectModel(r.m.Models); name != "" {
				c.lastModel[key] = name
			} else {
				// Successful poll with nothing loaded: a stale id would
				// make the next 'p' JIT-load (or bill) a cold model, and
				// an unloaded engine has no KV cache in use.
				delete(c.lastModel, key)
				delete(c.kvPct, key)
			}
			if r.m.HasKV {
				ps.KVPct = r.m.KVPct
				c.kvPct[key] = ps.KVPct
			} else {
				ps.KVPct = c.kvPct[key]
			}
			outPS, inPS := c.rates(key, r.m, now)
			ps.OutTokPS = outPS
			ps.InTokPS = inPS
			outR.push(outPS, now)
			inR.push(inPS, now)
		}
		ps.OutHist, ps.OutStamps = outR.copy(), outR.times()
		ps.InHist, ps.InStamps = inR.copy(), inR.times()
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
		// The baseline is seeded so the next scrape's delta starts here,
		// but a direct gauge does not need history: report it now instead
		// of blanking a live engine for one interval.
		if m.HasDirectOutPS {
			outPS = m.DirectOutPS
		}
		c.prev[key] = prevSample{at: now, outTotal: m.OutTotal, inTotal: m.InTotal, outEMA: outPS}
		return outPS, 0
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
	if m.HasDirectOutPS { // trust the engine's own tok/s gauge when present
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

// timedRing is a value history carrying the wall-clock time of every sample,
// so charts can place each point on an absolute time axis.
//
// The backing buffer is allocated once at HistoryLen and reused head-first:
// after warm-up push never allocates or copies, where sliding a slice
// (vals = vals[1:]) would realloc on every push for the life of the ring.
// Times ride in a parallel ring with the same head, so a sample and its
// instant are always overwritten together.
type timedRing struct {
	buf  []float64   // fixed capacity HistoryLen, samples in insertion order
	ts   []time.Time // the instant each buf entry was pushed
	head int         // counts fills while filling; then indexes the oldest element
}

func (r *timedRing) push(v float64, now time.Time) {
	if r.buf == nil { // one reservation for the ring's whole life
		r.buf = make([]float64, 0, core.HistoryLen)
		r.ts = make([]time.Time, 0, core.HistoryLen)
	}
	if len(r.buf) < core.HistoryLen { // filling: keep appending in order
		r.buf = append(r.buf, v)
		r.ts = append(r.ts, now)
		return
	}
	// head is always in [0, HistoryLen) here: filling leaves it at 0,
	// and each overwrite below wraps it after incrementing.
	r.buf[r.head] = v // overwrite the oldest sample
	r.ts[r.head] = now
	r.head++
	if r.head == core.HistoryLen {
		r.head = 0
	}
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

// times returns each sample's instant, oldest first, paired with copy.
func (r *timedRing) times() []time.Time {
	if len(r.ts) == 0 {
		return nil
	}
	out := make([]time.Time, len(r.ts))
	n := copy(out, r.ts[r.head:])
	copy(out[n:], r.ts[:r.head])
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

// RecordAgent stores an agent event (called from the ingest server) and
// reports whether it was retained.
// Events come from many senders whose clocks disagree (the ingest endpoint
// can face a LAN), so arrival order is not time order; every consumer reads
// Agents newest-last (see core.Snapshot), so keep them sorted by timestamp
// the way the probe ring is. A non-empty ID that is already in the retained
// window is ignored, so a retried POST of the same event does not double-count.
func (c *Collector) RecordAgent(ev core.AgentEvent) bool {
	if ev.At.IsZero() {
		ev.At = c.instant()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// The id index answers the dedup check in one map probe. Scanning the
	// window instead cost a full slice walk plus an NFC normalization per
	// retained event, under the mutex emit needs, for every ingested line.
	id := ""
	if ev.ID != "" {
		id = norm.NFC.String(ev.ID)
		if _, dup := c.agentIDs[id]; dup {
			return false
		}
	}
	c.agents = core.InsertSorted(append(c.agents, ev), core.AgentCmp)
	if id != "" {
		c.agentIDs[id] = struct{}{}
	}
	if len(c.agents) > core.AgentHistoryLen {
		drop := c.agents[:len(c.agents)-core.AgentHistoryLen]
		for _, e := range drop {
			if e.ID != "" {
				delete(c.agentIDs, norm.NFC.String(e.ID))
			}
		}
		c.agents = c.agents[len(c.agents)-core.AgentHistoryLen:]
	}
	return true
}

// RecordProbe stores a probe sample. Probes complete concurrently and can
// finish out of launch order, but every consumer (probe charts, the "last"
// readout) assumes newest-last ordering: keep the ring sorted by timestamp.
func (c *Collector) RecordProbe(s core.ProbeSample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probes = core.InsertSorted(append(c.probes, s), core.ProbeCmp)
	if len(c.probes) > core.ProbeHistoryLen {
		c.probes = c.probes[len(c.probes)-core.ProbeHistoryLen:]
	}
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

// probeTarget pairs a probe request with the collector state key it belongs
// to. The key is not always the request's Base: providers with no endpoint
// (see providerKey) share Base "", so inflight and backoff bookkeeping keyed
// on Base alone would have those providers cancel each other's waves.
type probeTarget struct {
	key string
	req probe.Request
}

// ProbeAll launches one probe against every known backend, asynchronously.
// Probes ride the Run context so shutdown cancels in-flight generations
// instead of leaving them running for the client's full timeout.
func (c *Collector) ProbeAll() {
	c.mu.Lock()
	var targets []probeTarget
	for _, p := range c.providers {
		if model := c.lastModel[providerKey(p)]; model != "" {
			targets = append(targets, probeTarget{
				key: providerKey(p),
				req: probe.Request{Kind: p.Kind, Base: p.Addr, Model: model},
			})
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
	var live []probeTarget
	for _, t := range targets {
		key := t.key
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
		go func(t probeTarget) {
			defer func() {
				c.probeMu.Lock()
				delete(c.probeInflight, t.key)
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
			s := probe.Run(pctx, t.req)
			s.At = now
			if s.RetryAfter > 0 {
				c.probeMu.Lock()
				c.probeBackoff[t.key] = c.instant().Add(s.RetryAfter)
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

func isLoopbackURL(addr string) bool {
	u, err := url.Parse(addr)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
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
