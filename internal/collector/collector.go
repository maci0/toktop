// Package collector polls providers on an interval, derives token rates from
// monotonic counters, and fans snapshots out to the UI.
package collector

import (
	"context"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/logcfg"
	"github.com/maci0/toktop/internal/probe"
	"github.com/maci0/toktop/internal/procs"
	"github.com/maci0/toktop/internal/provider"
	"github.com/maci0/toktop/internal/sysmon"
)

const (
	emaAlpha = 0.35
)

// defaultInterval is the poll period New falls back to when the caller passes
// a non-positive one. A zero or negative interval would otherwise make
// time.NewTicker panic on the first Run.
const defaultInterval = time.Second

// audit builds the process logger for the engine health lines. A var so a
// test can point it at a handler it can read.
var audit = logcfg.Logger

// downState is one engine's current outage: when it started and the reason
// the first failed poll gave.
type downState struct {
	since  time.Time
	reason string
}

type prevSample struct {
	at       time.Time
	outTotal float64
	inTotal  float64
	outEMA   float64
	inEMA    float64
}

// Collector polls every configured engine and the host on one interval and
// hands the result to consumers as a core.Snapshot. It owns the probe ring,
// the agent feed and the smoothing of per-engine rates, so nothing downstream
// has to reconcile two observations of the same engine.
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

	mu           sync.Mutex
	histOut      map[string]*timedRing
	histIn       map[string]*timedRing
	prev         map[string]prevSample
	lastModel    map[string]string // endpoint -> model to probe
	kvPct        map[string]float64
	agents       []core.AgentEvent
	agentIDs     map[string]time.Time // NFC event id -> instant it was recorded
	agentIDOrder []agentIDEntry       // the same ids in insertion order, oldest first
	probes       []core.ProbeSample
	started      time.Time
	baseCtx      context.Context // set by Run; bounds ad-hoc probes past shutdown
	// down holds the endpoints that failed their last poll, with when the
	// outage started and what it said, so the audit log records an engine
	// going away and coming back once each instead of once per poll.
	down map[string]downState
	// slow latches the endpoints whose last successful poll ran past
	// slowPollThreshold, with when the first one did, for the same reason
	// down exists: a poll interval of a second would otherwise write a line
	// per engine per second while an engine is merely struggling.
	slow map[string]time.Time

	probeMu       sync.Mutex // guards the probe fan-out state below
	lastProbeWave time.Time  // wave gate: see probeWaveGap
	probeInflight map[string]bool
	probeBackoff  map[string]time.Time

	// clockMu guards the two fields SetNow writes together. Reads are not
	// confined to the collector's own goroutines: the proc poller calls procFn,
	// which reads now, with no collector lock held. Guarding only the write side
	// would leave that read racing it, and reading now and started separately
	// lets a SetNow land between them, so an uptime is measured from a
	// different clock than the timestamp it is subtracted from.
	clockMu sync.Mutex
	now     func() time.Time // always non-nil: New sets time.Now, SetNow normalizes nil
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
		interval = defaultInterval
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
		agentIDs:      map[string]time.Time{},
		down:          map[string]downState{},
		slow:          map[string]time.Time{},
		probeInflight: map[string]bool{},
		probeBackoff:  map[string]time.Time{},
		now:           time.Now,
		started:       now,
	}
	// CPU tick deltas use this clock, not a second wall-clock read inside
	// the sampler: a frozen or stepped now must move dt the same way emit's
	// snapshot stamp does.
	c.procFn = func() []procs.Info { return procSampler.SnapshotAt(c.instant()) }
	return c
}

// SetNow overrides the clock used to stamp snapshots, probe-wave gating,
// and agent events that arrive without a timestamp. Safe to call while Run is
// going: the poller and the probe fan-out read the clock from their own
// goroutines, and the write is taken under the same lock they read it under.
func (c *Collector) SetNow(fn func() time.Time) {
	if fn == nil {
		fn = time.Now
	}
	c.clockMu.Lock()
	defer c.clockMu.Unlock()
	c.now = fn
	c.started = fn()
}

// instant is the collector clock, so a SetNow override reaches every call
// site. Never call it with clockMu held.
func (c *Collector) instant() time.Time {
	c.clockMu.Lock()
	defer c.clockMu.Unlock()
	return c.now()
}

// clock returns the current instant and the origin it is aged from as one
// pair, for the snapshot header that subtracts one from the other.
func (c *Collector) clock() (time.Time, time.Time) {
	c.clockMu.Lock()
	defer c.clockMu.Unlock()
	return c.now(), c.started
}

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

// startPoller runs one background refresh loop until ctx is done, calling
// warm once up front and refresh on every tick. The two pollers differ only
// in what a refresh does.
func startPoller(ctx context.Context, every time.Duration, warm, refresh func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(every)
		defer t.Stop()
		warm()
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

// startSysPoller refreshes host vitals in the background; emit never blocks
// on it (GPU vendor CLIs can take seconds and would stall every frame). Run
// warms the cache before emitting, so this first pass is a cache hit.
func (c *Collector) startSysPoller(ctx context.Context) <-chan struct{} {
	return startPoller(ctx, c.interval,
		func() { c.sampleSys(false) },
		func() { c.sampleSys(true) })
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
	refresh := func() {
		if infos := c.procFn(); infos != nil {
			c.procMu.Lock()
			c.procCache = infos
			c.procMu.Unlock()
		}
	}
	return startPoller(ctx, c.interval, refresh, refresh)
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

// result is one engine's poll outcome, paired so the fan-out can write each
// engine's slot without a lock. took is wall time, not the collector clock:
// a demo or test run pins now to a seeded timeline, and a poll measured
// against that would report every engine as instant.
type result struct {
	m    *provider.Metrics
	err  error
	took time.Duration
}

func (c *Collector) emit(ctx context.Context, out chan<- core.Snapshot) {
	results := make([]result, len(c.providers))
	var wg sync.WaitGroup
	for i, p := range c.providers {
		wg.Go(func() {
			// Bound by PollTimeout and by the Run context so shutdown does
			// not wait out a stalled scrape the way ProbeAll already
			// cancels in-flight generations.
			pctx, cancel := context.WithTimeout(ctx, provider.PollTimeout)
			defer cancel()
			started := time.Now()
			m, err := p.Poll(pctx)
			results[i] = result{m, err, time.Since(started)}
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}

	now, started := c.clock()
	// Uptime is an elapsed duration, but it is measured against the same clock
	// the rest of the snapshot uses, which is a wall clock on a real run. An
	// NTP step or a manual set (a laptop resuming, a VM snapshot restored)
	// moves that clock backwards, and the raw subtraction then reports a
	// session that started in the future. fmtDur clamps it for the header, but
	// --json serializes Uptime.Seconds() straight out, so the negative is
	// clamped here where the frame is built.
	snap := core.Snapshot{At: now, Uptime: max(now.Sub(started), 0)}
	// Vitals and the process table are independent of c.mu. Sampling them
	// inside the critical section would stall RecordAgent/ProbeAll for the
	// whole vendor-CLI sweep on a cold cache.
	sys := c.sysSnapshot()
	byPort := procsByPort(c.procSnapshot())

	c.mu.Lock()
	// Engine health transitions, collected under the lock and logged after it:
	// an engine that stops answering is the dependency failure an operator
	// needs named, and the dashboard's own "down" marker is replaced a frame
	// later. Collected, not written inline, so a slow stderr cannot stall the
	// poll loop the snapshot depends on.
	var failed, recovered, slow, fast []healthChange
	snap.Agents = slices.Clone(c.agents)
	snap.Probes = slices.Clone(c.probes)
	snap.Sys = cloneSys(sys)
	for i, r := range results {
		ps, changes := c.providerSnapshot(c.providers[i], r, now, byPort)
		for _, change := range changes {
			// One bucket per kind, and every kind is listed: a new boundary
			// chooses its log level here rather than falling through to a
			// default that quietly reports it at the wrong one.
			switch change.kind {
			case changeDown:
				failed = append(failed, change)
			case changeUp:
				recovered = append(recovered, change)
			case changeSlow:
				slow = append(slow, change)
			case changeFast:
				fast = append(fast, change)
			}
		}
		snap.Providers = append(snap.Providers, ps)
	}
	c.mu.Unlock()
	logHealth(failed, slog.LevelWarn, "toktop: engine not answering")
	logHealth(recovered, slog.LevelInfo, "toktop: engine answering again")
	logSlow(slow, slog.LevelWarn, "toktop: engine poll slow")
	logSlow(fast, slog.LevelInfo, "toktop: engine poll back to normal")
	// Send outside the critical section: a stalled consumer must neither pin
	// emit past cancellation nor freeze RecordAgent/RecordProbe/ProbeAll
	// behind c.mu while this send waits for buffer space.
	select {
	case out <- snap:
	case <-ctx.Done():
	}
}

// providerSnapshot builds one engine's snapshot entry from its poll result,
// updating the per-key baselines, history rings and health state that entry is
// keyed on. Call with c.mu held.
//
// The health transitions the poll caused are returned separately so emit can
// collect the whole sweep's transitions and log them after the lock: an engine
// going away is the dependency failure an operator needs named, and a slow
// stderr must not stall the poll loop the snapshot depends on. The list is
// empty when this engine crossed neither boundary.
func (c *Collector) providerSnapshot(p provider.Provider, r result, now time.Time, byPort map[int]procs.Info) (ps core.ProviderSnapshot, changes []healthChange) {
	ps = core.ProviderSnapshot{
		Label: p.Label,
		Kind:  p.Kind,
		Addr:  p.Addr,
	}
	// Per-provider state is keyed by providerKey: the endpoint when the
	// provider has one, the label otherwise, so labels repeat across
	// instances of the same engine kind without sharing baselines.
	key := providerKey(p)
	// ring() (not a bare map index): a provider whose first poll failed
	// has no history yet, and indexing the map there would deref nil.
	outR, inR := c.ring(c.histOut, key), c.ring(c.histIn, key)
	switch {
	case r.err != nil:
		// Folded for the same reason the audit log folds it: an engine error
		// can echo a model or config path under the operator's home, and
		// this text reaches the dashboard and both reports.
		ps.Err = core.RedactHome(r.err.Error())
	case r.m == nil:
		ps.Err = "empty poll result"
	default:
		ps.OK = true
		if was, ok := c.down[key]; ok {
			delete(c.down, key)
			changes = append(changes, healthChange{
				p: p, kind: changeUp, reason: was.reason, since: was.since, heldFor: now.Sub(was.since),
			})
		}
		// A poll that answered inside the budget is also the end of a slow
		// run, so the latch is cleared even when the answer arrived late in
		// the previous poll. Emptied without a line: a failed poll has its
		// own outage line, and a recovery from one already says the engine
		// came back.
		if r.took < slowPollThreshold {
			if since, ok := c.slow[key]; ok {
				delete(c.slow, key)
				changes = append(changes, healthChange{p: p, kind: changeFast, since: since, heldFor: now.Sub(since)})
			}
		} else if _, ok := c.slow[key]; !ok {
			c.slow[key] = now
			changes = append(changes, healthChange{p: p, kind: changeSlow, since: now, took: r.took})
		}
		ps.Models = r.m.Models
		ps.Running = r.m.Running
		ps.Waiting = r.m.Waiting
		ps.TTFTms = r.m.TTFTms
		if port, loopback := loopbackPort(p.Addr); port > 0 && loopback {
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
		ps.OutTokPS, ps.InTokPS = c.rates(key, r.m, now)
		outR.push(ps.OutTokPS, now)
		inR.push(ps.InTokPS, now)
	}
	ps.OutHist, ps.OutStamps = outR.copy(), outR.times()
	ps.InHist, ps.InStamps = inR.copy(), inR.times()
	if ps.Err != "" {
		if _, ok := c.down[key]; !ok {
			c.down[key] = downState{since: now, reason: ps.Err}
			changes = append(changes, healthChange{p: p, kind: changeDown, reason: ps.Err, since: now})
		}
		// The outage supersedes any slow run in progress: the engine is not
		// answering, and its next answer is measured fresh, so a stale latch
		// cannot report a slowdown that ended before the outage began.
		delete(c.slow, key)
	}
	return ps, changes
}

// changeKind is which boundary an engine crossed on the last poll. A poll can
// cross two at once (an engine recovers and comes back slow), so a change
// carries its own kind rather than being inferred from the snapshot entry.
type changeKind uint8

const (
	// changeDown and changeUp are the answering boundary: a poll that
	// failed, and one that answered after failing.
	changeDown changeKind = iota
	changeUp
	// changeSlow and changeFast are the latency boundary: a poll that
	// answered but took too long, and one that answered in time again.
	changeSlow
	changeFast
)

// healthChange is one engine crossing the answering or the latency boundary.
// reason is the failure text that started an answering run, so the recovery
// line names the outage it ends. took is the duration that tripped the latency
// boundary, and is zero on the change that ends the run. heldFor is how long
// the run lasted when the change was reported.
type healthChange struct {
	p       provider.Provider
	kind    changeKind
	reason  string
	since   time.Time
	heldFor time.Duration
	took    time.Duration
}

// slowPollThreshold is the duration past which a poll that still answered is
// audited. Half of provider.PollTimeout: an engine that needs more of the
// budget than that is on its way to the timeout that turns the same engine
// into an outage line, and until that timeout the dashboard reports it
// healthy, since a slow answer and a fast one are the same green. A var so
// tests can shrink it instead of sleeping past the real one.
var slowPollThreshold = provider.PollTimeout / 2

// logHealth writes one audit line per engine that changed state this poll. The
// transitions are already deduplicated in emit, so a fleet of engines that is
// down produces one line when it goes down and one when it returns however
// many intervals passed in between.
func logHealth(changes []healthChange, level slog.Level, msg string) {
	if len(changes) == 0 {
		return
	}
	lg := audit()
	for _, ch := range changes {
		attrs := []any{
			"engine", logcfg.Field(ch.p.Label, 128),
			"addr", logcfg.Field(ch.p.Addr, 256),
			"reason", logcfg.Field(ch.reason, 256),
		}
		if ch.heldFor > 0 {
			attrs = append(attrs, "down_for", ch.heldFor.Round(time.Millisecond))
		}
		lg.Log(context.Background(), level, msg, attrs...)
	}
}

// logSlow writes one audit line per engine that crossed the latency boundary.
// It carries the duration that tripped it, which no snapshot exposes: the
// dashboard shows whether an engine answered, never how long the answer took.
func logSlow(changes []healthChange, level slog.Level, msg string) {
	if len(changes) == 0 {
		return
	}
	lg := audit()
	for _, ch := range changes {
		attrs := []any{
			"engine", logcfg.Field(ch.p.Label, 128),
			"addr", logcfg.Field(ch.p.Addr, 256),
		}
		if ch.took > 0 {
			attrs = append(attrs, "duration", ch.took.Round(time.Millisecond))
		}
		if ch.heldFor > 0 {
			attrs = append(attrs, "slow_for", ch.heldFor.Round(time.Millisecond))
		}
		lg.Log(context.Background(), level, msg, attrs...)
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

// RecordProbe stores a probe sample. Probes complete concurrently and can
// finish out of launch order, but every consumer (probe charts, the "last"
// readout) assumes newest-last ordering: keep the ring sorted by timestamp.
func (c *Collector) RecordProbe(s core.ProbeSample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probes = core.AppendSorted(c.probes, s, core.ProbeHistoryLen, core.ProbeCmp)
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
		key := providerKey(p)
		if model := c.lastModel[key]; model != "" {
			targets = append(targets, probeTarget{
				key: key,
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
	if core.Age(now, c.lastProbeWave) < probeWaveGap {
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

func hostIsLoopback(u *url.URL) bool {
	host := u.Hostname()
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

// httpPort extracts the TCP port from a backend URL. Non-http(s) addresses
// (tests use fake://, and a blank Addr is not a listener) must not fall
// through to port 80: that would attach GPUStack-on-80 process stats to
// an unrelated provider.
func httpPort(u *url.URL) int {
	if u.Scheme != "http" && u.Scheme != "https" {
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

// loopbackPort answers both halves of the engine-block process lookup from
// one parse. Every provider's frame asks for both, so each engine would
// otherwise pay two parses per frame to decide whether a listening port
// belongs to it.
func loopbackPort(addr string) (port int, loopback bool) {
	u, err := url.Parse(addr)
	if err != nil {
		return 0, false
	}
	return httpPort(u), hostIsLoopback(u)
}
