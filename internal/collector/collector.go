// Package collector polls providers on an interval, derives token rates from
// monotonic counters, and fans snapshots out to the UI.
package collector

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/logcfg"
	"github.com/maci0/toktop/internal/procs"
	"github.com/maci0/toktop/internal/provider"
	"github.com/maci0/toktop/internal/sysmon"
)

// This file holds the poll loop itself: the collector's state, the clock it
// stamps on, and the one function that turns a sweep of poll results into a
// Snapshot. The concerns it fans out to each have their own file: host vitals
// and the process table in host.go, the rate smoothing and history rings in
// rates.go, one engine's entry and its health latches in health.go, the agent
// event feed in agents.go, and the probe wave in probe.go.

// defaultInterval is the poll period New falls back to when the caller passes
// a non-positive one. A zero or negative interval would otherwise make
// time.NewTicker panic on the first Run.
const defaultInterval = time.Second

// auditLog is the process logger for the engine health lines; a test swaps
// it for a handler it can read.
var auditLog = logcfg.NewSwapLogger(logcfg.Logger)

func audit() *slog.Logger { return auditLog.Logger() }

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

	mu        sync.Mutex
	histOut   map[string]*timedRing
	histIn    map[string]*timedRing
	prev      map[string]prevSample
	lastModel map[string]string // endpoint -> model to probe
	kvPct     map[string]float64
	agents    []core.AgentEvent
	agentIDs  core.AgentIDLedger // the NFC event ids already stored
	// agentSkews maps a canonical agent name to that sender's clock offset, so
	// its events are stored on this machine's timeline; agentSkewOrder holds
	// the same offsets in insertion order, oldest first. agentSkewLive names
	// the row agentSkewOrder still has in force for an agent, so a superseded
	// row ageing out cannot delete an offset that was read again at the same
	// value: agent, skew and instant together identify one reading.
	agentSkews     map[string]time.Duration
	agentSkewLive  map[string]agentSkewEntry
	agentSkewOrder []agentSkewEntry
	// windowLost latches a run of agent events the feed window refused, which
	// it does for an event older than everything it retains: a sender whose
	// clock lags contributes nothing and is told only that stored came back
	// under accepted. down and slow latch the same way, for the same reason.
	// One counter and one name rather than a map keyed on agent: the ingest
	// endpoint authenticates nobody, so an unbounded per-agent map would be
	// memory an attacker could name into existence. The run survives the emit
	// that reported it and clears when an event lands again, so a run that
	// keeps going stays one line and its end is still reported.
	windowLost      time.Time // when the current run started; zero when none
	windowRefused   int       // events refused since the last line
	windowAgent     string    // the last agent refused, named on the line
	windowLogged    bool      // the run's opening line is written
	windowRecovered string    // agent that got back in; closes the run
	probes          []core.ProbeSample
	started         time.Time
	baseCtx         context.Context // set by Run; bounds ad-hoc probes past shutdown
	running         bool            // a Run is live: see errRunInProgress
	// down holds the endpoints that failed their last poll, with when the
	// outage started and what it said, so the audit log records an engine
	// going away and coming back once each instead of once per poll.
	down map[string]downState
	// slow latches the endpoints whose last successful poll ran past
	// slowPollThreshold, with when the first one did, for the same reason
	// down exists: a poll interval of a second would otherwise write a line
	// per engine per second while an engine is merely struggling.
	slow map[string]time.Time
	// errFold memoizes the folded text of a poll error per key. A downed
	// engine answers the same failed poll every interval, and folding is a
	// pure function of that error string, so the fold is done once per
	// distinct error rather than once per poll per engine.
	errFold map[string]foldedErr

	probeMu sync.Mutex // guards the probe fan-out state below
	// probeClosing latches once Run has decided to join the fan-out. It is
	// taken under probeMu, and probeWG.Add is made under the same lock, so an
	// Add can never land after the Wait that follows: the WaitGroup misuse
	// check turns that pair into a panic, and a generation started past the
	// join would record into a collector a later Run already owns.
	probeClosing  bool
	probeWG       sync.WaitGroup
	lastProbeWave time.Time // wave gate: see probeWaveGap
	probeCursor   int       // rotation offset into the wave's targets: see probeWaveMax
	probeInflight map[string]bool
	probeBackoff  map[string]time.Time
	probeLast     map[string]time.Time // last launch per endpoint: probeBackendGap
	probeDown     map[string]*probeDownState

	// clockMu guards the two fields SetNow writes together. Reads are not
	// confined to the collector's own goroutines: the proc poller calls procFn,
	// which reads now, with no collector lock held. Guarding only the write side
	// would leave that read racing it, and reading now and started separately
	// lets a SetNow land between them, so an uptime is measured from a
	// different clock than the timestamp it is subtracted from. It is held
	// only long enough to copy the func value out: the clock itself is
	// caller-supplied and is called with clockMu released, as instant
	// documents.
	clockMu sync.Mutex
	now     func() time.Time // always non-nil: New sets time.Now, SetNow normalizes nil
	// pace paces the poll loop and the two host pollers. It is read and
	// written under clockMu with now, because a run that steps its passes
	// off a simulated timeline must stamp those passes from that timeline:
	// one seeded by SetNow and the other by wall-clock time cannot replay.
	// Always non-nil: New sets core.WallPacer, SetPacer normalizes nil.
	pace core.Pacer
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
		agentSkews:    map[string]time.Duration{},
		agentSkewLive: map[string]agentSkewEntry{},
		down:          map[string]downState{},
		slow:          map[string]time.Time{},
		probeInflight: map[string]bool{},
		probeBackoff:  map[string]time.Time{},
		probeLast:     map[string]time.Time{},
		probeDown:     map[string]*probeDownState{},
		now:           time.Now,
		pace:          core.WallPacer,
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
//
// The same clock is handed to the host-vitals sampler, which ages its CPU
// model memo, its host-identity retry window and its sensor layout sweep
// against it. A collector that stamps its frames on a seeded timeline while
// those windows age on the wall clock decides the host strip of a frame by
// how long the process happened to run, which is the one thing a replay
// cannot reproduce.
func (c *Collector) SetNow(fn func() time.Time) {
	if fn == nil {
		fn = time.Now
	}
	sysmon.SetNow(fn)
	// Stamped before the lock is taken, for the reason instant documents: the
	// clock is caller-supplied, and calling it under clockMu is a
	// self-deadlock the moment it reads the collector back. Both fields are
	// then written under the one lock their readers take them under, so no
	// reader can see the new clock beside the old origin.
	started := fn()
	c.clockMu.Lock()
	c.now, c.started = fn, started
	c.clockMu.Unlock()
}

// instant is the collector clock, so a SetNow override reaches every call
// site. The field is read under clockMu and the clock is invoked with it
// released, the way every other injected clock in this program is read: it is
// caller-supplied, and a clock that re-enters the collector deadlocks against
// the very lock that read it. Every goroutine the collector runs reads this
// clock, from the poll loop through the ingest handlers to the UI's probe
// wave, so holding clockMu across the call also makes one slow clock a
// process-wide stall rather than one slow reader.
func (c *Collector) instant() time.Time {
	c.clockMu.Lock()
	fn := c.now
	c.clockMu.Unlock()
	return fn()
}

// clock returns the current instant and the origin it is aged from as one
// pair, for the snapshot header that subtracts one from the other. Both are
// read under one lock so a SetNow cannot land between them, and the clock is
// invoked with that lock released, as instant documents.
func (c *Collector) clock() (time.Time, time.Time) {
	c.clockMu.Lock()
	fn, started := c.now, c.started
	c.clockMu.Unlock()
	return fn(), started
}

// SetPacer replaces what paces the poll loop and the two host pollers.
// Production leaves it on core.WallPacer. A simulated run passes a
// core.VirtualPacer and drives it: the poll that usually waits out a wall-
// clock interval fires when the driver says so, and with SetNow holding the
// clock, one seed reproduces the run's frames. A nil pacer restores the wall
// clock.
//
// Safe to call at any time, but each loop reads it once, when that loop
// starts: a call during a live Run leaves the three tickers already built on
// the old pacer and takes effect on the next Run.
func (c *Collector) SetPacer(p core.Pacer) {
	if p == nil {
		p = core.WallPacer
	}
	c.clockMu.Lock()
	c.pace = p
	c.clockMu.Unlock()
}

// pacer reads the timing source under the same lock now is, so a run cannot
// stamp its frames from one timeline and pace them from another.
func (c *Collector) pacer() core.Pacer {
	c.clockMu.Lock()
	p := c.pace
	c.clockMu.Unlock()
	return p
}

// errRunInProgress is what a second concurrent Run is refused with. A
// collector is built for one run: the pollers, the rate baselines and the
// down/slow latches below all belong to that one.
var errRunInProgress = errors.New("collector: already running")

// Run polls until ctx is cancelled, emitting one Snapshot per interval, and
// returns nil when the context ends the loop.
//
// A second Run while the first is live is refused with errRunInProgress and
// starts nothing. Two Run loops on one collector do not poll twice and report
// twice; they divide one dashboard's state between them. Each loop starts its
// own process-table and host-vitals poller, so the vendor CLIs run twice as
// often and the caches are force-refreshed by each loop's own tick; each
// writes its own Snapshot into the same channel, so the consumer renders every
// frame twice; and both fold their polls into the same prev baselines and the
// same down/slow latches, so a rate is measured across one loop's samples while
// the other loop's sit in between, and a transition that one loop logged is
// logged again by the other against a state the first already moved.
//
// The claim is released when the loop returns, so a restart after a finished
// run is unaffected: what is refused is a second live loop, not a second run.
func (c *Collector) Run(ctx context.Context, out chan<- core.Snapshot) error {
	if !c.claimRun(ctx) {
		return errRunInProgress
	}
	defer c.releaseRun()
	// The probe fan-out is joined, not raced, for the same reason the emit loop
	// is. A generation still in flight would otherwise record its sample and its
	// audit line after Run returned and released the claim, into a collector a
	// later run already owns. Cancellation bounds the wait: every generation
	// reads baseCtx, so probe.Run returns as soon as ctx is done.
	//
	// The latch is taken under probeMu before the Wait, which is what makes
	// the pair safe: a ProbeAll still racing this return finds it, launches
	// nothing, and never calls Add against a Wait in progress.
	defer func() {
		c.probeMu.Lock()
		c.probeClosing = true
		c.probeMu.Unlock()
		c.probeWG.Wait()
	}()
	procDone := c.startProcPoller(ctx)
	defer func() { <-procDone }()
	// Warm the vitals cache before the first emit so that frame is a cache
	// hit. GPU vendor CLIs can take seconds; sampling them inside emit
	// would delay it. RecordAgent is not pinned: sysSnapshot runs outside c.mu.
	c.sampleSys(false)
	sysDone := c.startSysPoller(ctx)
	defer func() { <-sysDone }()
	// The emit loop is joined, not raced: Run must not return while a frame
	// is still being written to out.
	emit := func() { c.emit(ctx, out) }
	<-core.TickWith(ctx, c.pacer(), c.interval, emit, emit)
	return nil
}

// claimRun takes the single live-run claim and records the context ad-hoc
// probes are bounded by. It reports false when another Run already holds it,
// having changed nothing.
func (c *Collector) claimRun(ctx context.Context) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		return false
	}
	c.running = true
	c.baseCtx = ctx
	// A run started after a finished one is a fresh join: the previous run's
	// latch is released here, under the same lock the fan-out reads it under.
	c.probeMu.Lock()
	c.probeClosing = false
	c.probeMu.Unlock()
	return true
}

// releaseRun hands the claim back, so a run started after the previous one
// returned is not read as a second live loop.
func (c *Collector) releaseRun() {
	c.mu.Lock()
	c.running = false
	c.mu.Unlock()
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
	byPort := c.procByPort()

	c.mu.Lock()
	// Engine health transitions, collected under the lock and logged after it:
	// an engine that stops answering is the dependency failure an operator
	// needs named, and the dashboard's own "down" marker is replaced a frame
	// later. Collected, not written inline, so a slow stderr cannot stall the
	// poll loop the snapshot depends on.
	// One bucket per changeKind, indexed by it, so a new boundary picks its
	// log level at the call below rather than falling through a switch that
	// quietly reports it at the wrong one.
	var buckets [changeFast + 1][]healthChange
	homeUnknown := false
	snap.Agents = slices.Clone(c.agents)
	snap.Probes = slices.Clone(c.probes)
	snap.Sys = cloneSys(sys)
	for i, r := range results {
		ps, changes, hu := c.providerSnapshot(c.providers[i], r, now, byPort)
		homeUnknown = homeUnknown || hu
		for _, change := range changes {
			buckets[change.kind] = append(buckets[change.kind], change)
		}
		snap.Providers = append(snap.Providers, ps)
	}
	// Taken under the lock and written after it, like the transitions above:
	// a stalled stderr must not stall the poll loop the snapshot depends on.
	refused := c.drainWindowRefusals()
	c.mu.Unlock()
	logChanges(buckets[changeDown], slog.LevelWarn, "toktop: engine not answering", "down_for")
	logChanges(buckets[changeUp], slog.LevelInfo, "toktop: engine answering again", "down_for")
	logChanges(buckets[changeSlow], slog.LevelWarn, "toktop: engine poll slow", "slow_for")
	logChanges(buckets[changeFast], slog.LevelInfo, "toktop: engine poll back to normal", "slow_for")
	logWindowRefusals(refused)
	// One line per sweep, not one per engine: the condition is the process's,
	// not a given engine's, and the engines that saw it said the same thing.
	if homeUnknown {
		logHomeUnknown()
	}
	// Send outside the critical section: a stalled consumer must neither pin
	// emit past cancellation nor freeze RecordAgent/RecordProbe/ProbeAll
	// behind c.mu while this send waits for buffer space.
	select {
	case out <- snap:
	case <-ctx.Done():
	}
}
