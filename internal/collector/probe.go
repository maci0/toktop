package collector

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/logcfg"
	"github.com/maci0/toktop/internal/probe"
)

// The probe fan-out: a wave of small streaming generations across the known
// backends, the wave gate and rotation that bound how many run at once, the
// inflight and 429-backoff bookkeeping, and the retained samples.

// RecordProbe stores a probe sample. Probes complete concurrently and can
// finish out of launch order, but every consumer (probe charts, the "last"
// readout) assumes newest-last ordering: keep the ring sorted by timestamp.
func (c *Collector) RecordProbe(s core.ProbeSample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probes = core.AppendSorted(c.probes, s, core.ProbeHistoryLen, core.ProbeCmp)
}

// probeWaveGap is the minimum spacing between probe waves. The UI's 'p' key
// auto-repeats when held and the --probe ticker can land on top of a manual
// wave; without a gate each press stacks another concurrent generation on
// every backend. Probing distorts the very metrics it measures, and
// OpenAI-compatible gateways (LiteLLM and friends) may bill every probe
// token, so waves also never overlap per backend.
var probeWaveGap = 500 * time.Millisecond

// probeWaveMax is how many backends one wave probes. probeWaveGap spaces
// waves but never bounds their width: on a fleet of N backends a single tick
// (--probe as low as 1s, or a held 'p') fires N generations at once, and on
// an OpenAI-compatible gateway every one of them is billed. A wave takes this
// many and leaves the rest to the next one, resuming where it stopped
// (probeCursor), so a fleet wider than the cap still gets every backend
// probed rather than the first few over and over.
const probeWaveMax = 4

// probeTarget pairs a probe request with the collector state key it belongs
// to. The key is not always the request's Base: providers with no endpoint
// (see providerKey) share Base "", so inflight and backoff bookkeeping keyed
// on Base alone would have those providers cancel each other's waves. The
// label rides along because the audit line names the engine the way every
// other line does, and the key is an endpoint that need not read as one.
type probeTarget struct {
	key   string
	label string
	req   probe.Request
}

// probeDownState latches one engine's failing probes, so the audit log records
// the start of a run of failures once and its end once. A --probe tick on a
// broken engine would otherwise write a line every interval for as long as the
// operator is away, and a line per wave is the same noise an outage produces on
// the poll path, which is latched for exactly this reason.
type probeDownState struct {
	mu     sync.Mutex
	failed bool
	since  time.Time
}

// auditProbe writes the transition lines for one engine's probe outcome. It
// says nothing about a probe that kept answering: a line per wave would be a
// line per --probe tick on a healthy fleet, and the pane already shows the
// measurement a successful probe exists to make.
func (c *Collector) auditProbe(t probeTarget, s core.ProbeSample, took time.Duration) {
	c.probeMu.Lock()
	d, ok := c.probeDown[t.key]
	if !ok {
		d = &probeDownState{}
		c.probeDown[t.key] = d
	}
	c.probeMu.Unlock()

	d.mu.Lock()
	// since is read before the latch clears, so the recovery line reports how
	// long the failures ran rather than the time since the zero instant.
	since := d.since
	first := false
	if s.OK {
		first = !since.IsZero()
		d.failed, d.since = false, time.Time{}
	} else {
		first = !d.failed
		if first {
			// The collector clock, not the wall clock: the sample's At, the
			// probe-wave gate and the 429 backoff all follow it, so a latch
			// stamped from time.Now ages against a timeline none of them
			// shares and a run replaying on a seeded clock reports a
			// down_for no seed can reproduce.
			d.since = c.instant()
		}
		d.failed = true
	}
	d.mu.Unlock()

	lg := audit()
	attrs := []any{
		"engine", logcfg.Field(t.label, 128),
		"addr", logcfg.Field(t.req.Base, 256),
		"model", logcfg.Field(t.req.Model, 128),
	}
	if s.OK {
		// The transition lines say whether an engine is answering; nothing
		// else on the record says what it answered like. A throughput change
		// is the thing a probe exists to catch, and the pane only holds the
		// last frame of it: a regression that a later probe recovered from is
		// gone, with no record that the engine was ever slow. One line per
		// probe at debug costs an operator who asked for it a line per
		// --probe tick and stays silent under the default floor, so the
		// numbers are attributable to a model id without logging anything by
		// default.
		attrs = append(attrs,
			"duration", took.Round(time.Millisecond),
			"ttft_ms", math.Round(s.TTFTms),
			"tok_per_s", math.Round(s.TokPS),
			"tokens", s.Tokens)
		lg.Debug("toktop: probe ok", attrs...)
		if first {
			attrs = append(attrs, "down_for", core.Age(c.instant(), since).Round(time.Second))
			lg.Info("toktop: probe answering again", attrs...)
		}
		return
	}
	if !first {
		return
	}
	attrs = append(attrs,
		"duration", took.Round(time.Millisecond),
		"reason", logcfg.Field(s.Err, 256))
	lg.Warn("toktop: probe failed", attrs...)
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
				key:   key,
				label: p.Label,
				req:   probe.Request{Kind: p.Kind, Base: p.Addr, Model: model},
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
	examined := 0
	// The walk starts at probeCursor and wraps once, so a wave that fills the
	// cap advances the cursor past exactly the targets it considered: a
	// backend later in the order is probed on the next wave instead of
	// starving behind the first probeWaveMax forever.
	for i := 0; i < len(targets) && len(live) < probeWaveMax; i++ {
		examined++
		t := targets[(c.probeCursor+i)%len(targets)]
		key := t.key
		if c.probeInflight[key] { // one generation per backend at a time
			continue
		}
		if until, ok := c.probeBackoff[key]; ok && time.Now().Before(until) {
			continue // 429/503: wait out Retry-After before POSTing again
		}
		delete(c.probeBackoff, key)
		c.probeInflight[key] = true
		live = append(live, t)
	}
	// Only a wave that examined a target moves the rotation: a wave whose
	// every backend is inflight or in backoff probed nothing, and turning the
	// cursor there would shift which backends the next wave skips.
	if examined > 0 {
		c.probeCursor = (c.probeCursor + examined) % len(targets)
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
			started := time.Now()
			s := probe.Run(pctx, t.req)
			s.At = now
			if s.RetryAfter > 0 {
				c.probeMu.Lock()
				// The wall clock, carrying its monotonic reading, for the same
				// reason the elapsed time below is measured on it: a backoff is
				// a real wait, not a position on the collector's timeline. As an
				// instant on that timeline it survived a clock step badly in
				// both directions, and this is the direction that costs money:
				// an NTP step forward past the deadline drops the wait and
				// re-POSTs into a gateway that just asked for 429 backoff, and
				// every retry is a billed probe. time.Now carries a monotonic
				// reading and time.Now().Before uses it, so a step of either
				// sign leaves the wait the length the engine asked for.
				c.probeBackoff[t.key] = time.Now().Add(s.RetryAfter)
				c.probeMu.Unlock()
			}
			// The wall clock, not the collector's: the sample's At follows the
			// seeded timeline a demo pins, and an elapsed time measured against
			// that one is not a duration the operator ran.
			//
			// A generation the operator cancelled is not an engine failure:
			// probe.Run reports it as an error, and recording it would write a
			// "probe failed" line and a red sample on every quit. The poll path
			// drops its sample on cancellation for the same reason.
			if pctx.Err() != nil {
				return
			}
			c.auditProbe(t, s, time.Since(started))
			c.RecordProbe(s)
		}(t)
	}
}
