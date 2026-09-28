package collector

import (
	"context"
	"maps"
	"slices"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/procs"
)

// The host half of a snapshot: the vitals sample and the engine-process
// table. Both are read from background pollers that emit never waits on, so
// a slow vendor CLI or a slow Win32 CIM enumeration cannot stall a frame.

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
	return core.TickWith(ctx, c.pacer(), c.interval,
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
	if !force && cached != nil {
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
	return core.TickWith(ctx, c.pacer(), c.interval, refresh, refresh)
}

// procSnapshot returns the latest cached engine processes, detached from the
// poller's buffer so a later refresh cannot mutate a snapshot already in emit.
func (c *Collector) procSnapshot() []procs.Info {
	c.procMu.Lock()
	defer c.procMu.Unlock()
	return slices.Clone(c.procCache)
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
