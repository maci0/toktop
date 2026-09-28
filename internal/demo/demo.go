// Package demo simulates a small inference fleet so toktop has something to
// show without any real backends. Deterministic per seed: the seed decides
// every value, and SetOrigin pins the instant the timeline starts at, so a
// run is reproduced frame for frame.
package demo

import (
	"context"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/maci0/toktop/internal/core"
)

type backend struct {
	label, kind, addr, model string
	outBase, inBase          float64
	burstEvery               int // seconds between load bursts
}

// probeStreamIncr is the PCG stream selector for the probe draw. It keeps
// ProbeAll's numbers off the frame stream, so a probe wave fired from the UI
// goroutine at an arbitrary point in a tick cannot shift what the next frame
// reports. A replay with the same seed then depends on how many waves ran,
// not on how the two goroutines interleaved.
const probeStreamIncr = 0x70726f62655f7374

// Source emits plausible Snapshots on ch once per tick.
type Source struct {
	interval time.Duration
	backends []backend
	// rng drives the frame timeline: histories, vitals, agent events and the
	// background probes genProbe drops in. Its draw count is a function of
	// the frames elapsed alone, so two sources stepped the same instants
	// agree on every charted value.
	rng *rand.Rand
	// probeRng drives ProbeAll's synthesized samples, called from the UI
	// goroutine on a keypress or the --probe ticker. Keeping it separate is
	// what makes the seed the whole run: an interleaving that is up to the
	// OS scheduler cannot move the frame stream forward.
	probeRng *rand.Rand
	seed     int64
	mu       sync.Mutex

	start   time.Time
	now     time.Time // last simulated instant; zero until the first frame
	t       float64
	histOut map[string][]float64
	histIn  map[string][]float64
	// ts is the instant each history sample was produced, per label. Both
	// directions are sampled together, so one ring of stamps serves both.
	ts     map[string][]time.Time
	kv     []float64
	nextEv time.Time
	nextPr time.Time
	// probeEvery is the simulated auto-probe cadence, zero when off, and
	// nextUIProbe the simulated instant the next wave is due. The wave is
	// fired by frame, not by a wall-clock ticker, so a replay's wave count is
	// a function of the frames elapsed.
	probeEvery  time.Duration
	nextUIProbe time.Time

	memPct  float64
	swapPct float64
	agents  []core.AgentEvent
	probes  []core.ProbeSample
	// agentIDs is the id ledger a replayed POST is answered from. The
	// retained feed is a poor stand-in for it: it holds a couple of minutes
	// of generated events, so a sender retrying an Idempotency-Key after its
	// first copy has rolled out of the ring finds nothing to match and counts
	// its stream twice. The live collector keeps the same ledger for the same
	// reason, so both are one core.AgentIDLedger and a change to the dedup
	// window reaches a replay answered here and there alike.
	agentIDs core.AgentIDLedger
}

// NewSource builds the simulated fleet: interval is the simulated tick (a
// non-positive value becomes a second) and seed fixes the whole run, so two
// sources stepped at the same instants produce the same frames.
func NewSource(interval time.Duration, seed int64) *Source {
	if interval <= 0 {
		interval = time.Second
	}
	backends := []backend{
		{label: "ollama", kind: core.KindOllama, addr: "http://127.0.0.1:11434", model: "llama3.1:8b-instruct-q4_K_M",
			outBase: 38, inBase: 120, burstEvery: 17},
		{label: "vllm-a100", kind: core.KindVLLM, addr: "http://127.0.0.1:8000", model: "Qwen/Qwen2.5-32B-Instruct-AWQ",
			outBase: 210, inBase: 900, burstEvery: 11},
		{label: "sglang-h200", kind: core.KindSGLang, addr: "http://127.0.0.1:30000", model: "deepseek-ai/DeepSeek-R1-Distill-Llama-70B-FP8",
			outBase: 340, inBase: 1500, burstEvery: 13},
		{label: "trt-llm", kind: core.KindTRTLLM, addr: "http://127.0.0.1:8001", model: "meta-llama/Llama-3.3-70B-Instruct-engine",
			outBase: 260, inBase: 1100, burstEvery: 19},
		{label: "mlx-studio", kind: core.KindMLX, addr: "http://127.0.0.1:1234", model: "mlx-community/Qwen2.5-Coder-14B-4bit",
			outBase: 46, inBase: 180, burstEvery: 23},
	}
	return &Source{
		interval: interval,
		rng:      rand.New(rand.NewPCG(uint64(seed), 0)),
		probeRng: rand.New(rand.NewPCG(uint64(seed), probeStreamIncr)),
		seed:     seed,
		backends: backends,
		histOut:  map[string][]float64{},
		histIn:   map[string][]float64{},
		ts:       map[string][]time.Time{},
		// One KV slot per backend, indexed by backend position in frame.
		kv:      make([]float64, len(backends)),
		memPct:  52,
		swapPct: 14,
	}
}

// Seed reports the seed this source draws from. Every simulated value comes
// from it, on one of two streams: the frame stream, whose values depend on
// the seed and the frames elapsed, and the probe stream behind ProbeAll,
// whose values depend on the seed and the waves run. Neither depends on
// wall-clock timing or on goroutine interleaving, so the seed is the whole
// run: the dashboard shows it, and a run is reproduced by starting toktop
// with the same --seed.
func (s *Source) Seed() int64 { return s.seed }

var agentNames = []string{"coder-agent", "ops-agent", "research-agent", "swarm-07"}
var evKinds = []string{core.AgentKindTurn, core.AgentKindTool, core.AgentKindTool, core.AgentKindNote, core.AgentKindError}
var notes = []string{
	"planning patch series",
	"shell(git status)",
	"summarizing diff",
	"retry after 429",
	"writing tests",
	"browser(search docs)",
	"final answer composed",
}

// Run blocks until ctx is done. The ticker only paces real time; each
// frame's simulated instant starts at the first clock read (shared with
// ProbeAll/RecordAgent/Now) and then advances by interval, so ticker jitter,
// coalesced ticks, and a probe that wins the race with the first frame
// cannot pull timestamps off the seeded trajectory.
func (s *Source) Run(ctx context.Context, ch chan<- core.Snapshot) {
	tick := time.NewTicker(s.interval)
	defer tick.Stop()
	now := s.Now()
	for {
		snap := s.stepAt(now)
		select { // a stalled consumer must not pin the goroutine past cancel
		case ch <- snap:
		case <-ctx.Done():
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			now = now.Add(s.interval)
		}
	}
}

// stepAt applies one simulated frame at now and returns the snapshot. Tests
// drive this directly so two sources with the same seed can be compared
// without a wall-clock ticker.
func (s *Source) stepAt(now time.Time) core.Snapshot {
	s.mu.Lock()
	if s.start.IsZero() {
		s.start = now
		s.nextEv = now.Add(2 * s.interval)
		s.nextPr = now.Add(4 * s.interval)
		// Armed on the origin, not one cadence out, so the first wave lands
		// with the first frame the way the wall-clock ticker fired one
		// straight away when the dashboard came up.
		if s.probeEvery > 0 {
			s.nextUIProbe = now
		}
	}
	s.now = now
	s.mu.Unlock()
	s.frame(now)
	return s.snapshot(now)
}

// Now is the current simulated instant. The first caller pins the origin
// (one wall-clock read, or SetOrigin when the caller pins it); later calls
// and Run share that origin so probes, ingest stamps, and frames stay on one
// timeline. Tests drive time through stepAt instead.
func (s *Source) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stamp()
}

// SetOrigin pins the instant the simulated timeline starts at, for a caller
// that has to reproduce a run against a fixed axis: without it two runs of
// one seed report the same values stamped with their own launch times, so
// their frames cannot be diffed against each other. Call it before Run, or
// any other caller of Now; a source that has already produced a frame panics
// rather than moving a timeline its stamped history sits on.
func (s *Source) SetOrigin(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.start.IsZero() {
		panic("demo: SetOrigin after the first frame")
	}
	s.now = at
}

// stamp is the current simulated instant. The first call pins the origin
// from the wall clock; after that it never reads the clock again. Caller
// holds s.mu.
func (s *Source) stamp() time.Time {
	if s.now.IsZero() {
		s.now = time.Now()
	}
	return s.now
}

// frame mutates every simulated channel (rng, histories, vitals). It takes
// the same lock as the externally callable RecordAgent/ProbeAll/snapshot:
// Run drives it from one goroutine, but probes and agent events arrive from
// the UI goroutine, and rand.Rand is not safe for concurrent use.
func (s *Source) frame(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.After(s.nextEv) {
		s.genEvent(now)
		s.nextEv = now.Add(time.Duration(2+s.rng.IntN(5)) * s.interval)
	}
	if now.After(s.nextPr) {
		s.genProbe(now)
		s.nextPr = now.Add(time.Duration(6+s.rng.IntN(6)) * s.interval)
	}
	// Auto-probe waves ride the simulated clock, so the wave count and every
	// sample's instant are functions of the frames elapsed. A cadence shorter
	// than the poll interval catches up in a loop rather than leaving the
	// schedule behind the timeline.
	for s.probeEvery > 0 && !now.Before(s.nextUIProbe) {
		s.probeAllLocked(now)
		s.nextUIProbe = s.nextUIProbe.Add(s.probeEvery)
	}
	s.t += s.interval.Seconds()
	for i, b := range s.backends {
		wave := math.Sin(s.t/9+float64(i)*1.7)*0.25 + 1
		burst := 0.0
		if int(s.t)%b.burstEvery < 3 {
			burst = b.outBase * 1.6
		}
		jitter := 1 + s.rng.NormFloat64()*0.06
		out := clamp((b.outBase*wave+burst)*jitter, 0, 4000)
		in := clamp((b.inBase*wave+burst*4)*jitter, 0, 20000)
		s.histOut[b.label] = ring(s.histOut[b.label], out)
		s.histIn[b.label] = ring(s.histIn[b.label], in)
		s.ts[b.label] = ringTime(s.ts[b.label], now)

		target := 45 + 40*math.Sin(s.t/14+float64(i)) + s.rng.NormFloat64()*3
		s.kv[i] += (clamp(target, 3, 99) - s.kv[i]) * 0.15
	}

	memTarget := 50 + 18*math.Sin(s.t/19) + s.rng.NormFloat64()*2
	s.memPct += (clamp(memTarget, 20, 95) - s.memPct) * 0.12
	swapTarget := 10 + 8*math.Sin(s.t/31+1) + s.rng.NormFloat64()
	s.swapPct += (clamp(swapTarget, 0, 60) - s.swapPct) * 0.08
}

// sysSample builds host vitals consistent with the simulated fleet.
func (s *Source) sysSample() core.SysSample {
	const GiB = 1 << 30
	sys := core.SysSample{
		CPUModel:  "Simulated EPYC 9754 (demo)",
		MemTotal:  384 * GiB,
		SwapTotal: 8 * GiB,
		Load1:     clamp(1.2+math.Abs(math.Sin(s.t/21))*4+s.rng.NormFloat64()*0.1, 0, 64),
	}
	sys.Load5 = sys.Load1 * 0.85
	sys.Load15 = sys.Load1 * 0.7
	sys.MemUsed = uint64(float64(sys.MemTotal) * s.memPct / 100)
	sys.SwapUsed = uint64(float64(sys.SwapTotal) * s.swapPct / 100)
	cpu := 58 + 16*math.Sin(s.t/23) + s.rng.NormFloat64()*2
	sys.Temps = []core.TempReading{
		{Label: "package", MilliC: int(cpu * 1000)},
		{Label: "nvme0", MilliC: int((41 + 6*math.Abs(math.Sin(s.t/30))) * 1000)},
	}
	gpuBase := 62 + 20*math.Abs(math.Sin(s.t/16))
	a100 := core.GPUDevice{
		Vendor: "nvidia", Index: 0, Name: "A100-SXM4-80GB",
		MilliC: int(gpuBase * 1000), MemTotal: 80 * GiB,
		MemUsed: uint64(float64(80*GiB) * clamp(45+30*math.Sin(s.t/12), 5, 99) / 100),
		UtilPct: clamp(55+40*math.Sin(s.t/9), 0, 100),
		PowerW:  280 + 120*math.Abs(math.Sin(s.t/14)),
	}
	h200 := a100
	h200.Index, h200.Name, h200.MemTotal = 1, "H200-SXM-141GB", 141*GiB
	h200.MemUsed = uint64(float64(h200.MemTotal) * clamp(50+28*math.Sin(s.t/10+2), 5, 99) / 100)
	mi210 := core.GPUDevice{
		Vendor: "amd", Index: 0, Name: "MI210",
		MilliC: int((gpuBase + 6) * 1000), MemTotal: 64 * GiB,
		MemUsed: uint64(float64(64*GiB) * clamp(35+25*math.Sin(s.t/15+1), 5, 99) / 100),
		UtilPct: clamp(40+45*math.Sin(s.t/11+3), 0, 100),
		PowerW:  190 + 90*math.Abs(math.Sin(s.t/17)),
	}
	sys.GPUs = []core.GPUDevice{a100, h200, mi210}
	return sys
}

// genEvent synthesizes one agent event; caller holds s.mu.
func (s *Source) genEvent(now time.Time) {
	b := s.backends[s.rng.IntN(len(s.backends))]
	ev := core.AgentEvent{
		At:           now,
		Agent:        agentNames[s.rng.IntN(len(agentNames))],
		Model:        b.model,
		Kind:         evKinds[s.rng.IntN(len(evKinds))],
		PromptTokens: int64(400 + s.rng.IntN(9000)),
		OutputTokens: int64(30 + s.rng.IntN(1200)),
	}
	if ev.Kind == core.AgentKindNote || ev.Kind == core.AgentKindError {
		ev.Note = notes[s.rng.IntN(len(notes))]
	}
	s.addAgent(ev)
}

func (s *Source) addAgent(ev core.AgentEvent) {
	s.agents = core.AppendSorted(s.agents, ev, core.AgentHistoryLen, core.AgentCmp)
}

// RecordAgent lets external scripts push events into the demo feed too. It
// reports whether the event was retained, like a live collector: false for an
// id the ledger still holds from the last core.AgentIDHorizon, and for an event
// that sorts behind the retained window, which the feed would trim on arrival.
// The check is the ledger's rather than a scan of the feed, so a POST replayed
// after its first copy has rolled out of the ring is still answered as the
// duplicate it is.
func (s *Source) RecordAgent(ev core.AgentEvent) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.stamp()
	id := ""
	if ev.ID != "" {
		id = norm.NFC.String(ev.ID)
		if s.agentIDs.Seen(id, now) {
			return false
		}
	}
	if ev.At.IsZero() {
		ev.At = now
	}
	agents, kept := core.AppendRetained(s.agents, ev, core.AgentHistoryLen, core.AgentCmp)
	if !kept {
		return false
	}
	s.agents = agents
	if id != "" {
		s.agentIDs.Add(id, now)
	}
	return true
}

func (s *Source) addProbe(p core.ProbeSample) {
	s.probes = core.AppendSorted(s.probes, p, core.ProbeHistoryLen, core.ProbeCmp)
}

// ProbeEvery arms the simulated auto-probe cadence: a wave is fired at every
// d of simulated time, from the frame that crosses the boundary. It is what
// --probe drives in demo mode; without it the cadence would sit on a
// wall-clock ticker and two runs of one seed would disagree on how many waves
// ran and on the instant each one is stamped, so the run would not replay.
// A non-positive d turns auto-probe off. Call it before Run.
func (s *Source) ProbeEvery(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.start.IsZero() {
		panic("demo: ProbeEvery after the first frame")
	}
	s.probeEvery = max(d, 0)
	s.nextUIProbe = time.Time{}
}

// ProbeAll satisfies the UI prober interface by synthesizing samples now.
// It draws from probeRng, not the frame stream: a wave the operator fires
// half a tick early must not move every value the next frame reports.
func (s *Source) ProbeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probeAllLocked(s.stamp())
}

// probeAllLocked synthesizes one wave of samples stamped at. Caller holds
// s.mu.
func (s *Source) probeAllLocked(at time.Time) {
	for i := range s.backends {
		s.addProbe(s.synthProbe(s.probeRng, s.backends[i], at, 60, 180, 0.3, 1))
	}
}

// synthProbe fabricates one plausible probe result; spans size the random
// ttft/duration draws. The caller names the stream so a background probe
// stays on the frame timeline and a UI-triggered one does not.
func (s *Source) synthProbe(rng *rand.Rand, b backend, at time.Time, ttftLo, ttftSpan, durLo, durSpan float64) core.ProbeSample {
	ttft := ttftLo + rng.Float64()*ttftSpan
	dur := durLo + rng.Float64()*durSpan
	n := int(dur * (b.outBase / (1 + rng.Float64())))
	return core.ProbeSample{
		At: at, Addr: b.addr, Model: b.model, OK: true,
		TTFTms: ttft,
		TokPS:  float64(n) / dur,
		Tokens: n,
	}
}

// genProbe drops in one background probe; caller holds s.mu.
func (s *Source) genProbe(now time.Time) {
	b := s.backends[s.rng.IntN(len(s.backends))]
	s.addProbe(s.synthProbe(s.rng, b, now, 80, 140, 0.4, 1.2))
}

func (s *Source) snapshot(now time.Time) core.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	sys := s.sysSample()
	snap := core.Snapshot{
		At:     now,
		Uptime: now.Sub(s.start),
		Sys:    &sys,
		Agents: slices.Clone(s.agents),
		Probes: slices.Clone(s.probes),
	}
	for i, b := range s.backends {
		ps := core.ProviderSnapshot{
			Label:    b.label,
			Kind:     b.kind,
			Addr:     b.addr,
			OK:       true,
			Models:   []core.ModelInfo{{Name: b.model}},
			OutTokPS: tail(s.histOut[b.label]),
			InTokPS:  tail(s.histIn[b.label]),
			KVPct:    clamp(s.kv[i], 0, 100),
			TTFTms:   90 + 70*math.Abs(math.Sin(s.t/10+float64(i))),

			Running:   1 + i%3 + int(clamp(math.Sin(s.t/7+float64(i))*1.5+1.5, 0, 4)),
			Waiting:   int(clamp(math.Sin(s.t/13+float64(i*2))*3+3, 0, 24)),
			OutHist:   slices.Clone(s.histOut[b.label]),
			InHist:    slices.Clone(s.histIn[b.label]),
			OutStamps: slices.Clone(s.ts[b.label]),
			InStamps:  slices.Clone(s.ts[b.label]),
		}
		snap.Providers = append(snap.Providers, ps)
	}
	return snap
}

func clamp(v, lo, hi float64) float64 { return min(max(v, lo), hi) }

func tail(h []float64) float64 {
	if len(h) == 0 {
		return 0
	}
	return h[len(h)-1]
}

func ring(h []float64, v float64) []float64 {
	if len(h) >= core.HistoryLen {
		h = h[1:]
	}
	return append(h, v)
}

// ringTime slides the stamp ring alongside a value ring, so every sample
// keeps the simulated instant it was produced at.
func ringTime(h []time.Time, at time.Time) []time.Time {
	if len(h) >= core.HistoryLen {
		h = h[1:]
	}
	return append(h, at)
}
