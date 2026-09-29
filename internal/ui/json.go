package ui

import (
	"encoding/json"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// The --json report: the same snapshot the frame and the text report draw,
// carried as one JSON object for a script reading toktop's numbers instead
// of a person reading its output. Every free-form string is sanitized on the
// way out, as in the other two renderings, because an engine version, a model
// name and an error string are all attacker-influenced text on a shared port.
//
// The chart histories (OutHist/InHist and their stamps) are left out: they
// are the frame's own buffer, sized by how long the process ran, and a
// consumer wanting a series has a steady --interval to sample.

// JSONFrame renders one snapshot as a single JSON object, indented for a
// human reading a captured run. The error is returned rather than printed,
// so the caller keeps stdout for the report alone.
func JSONFrame(cfg Config, s core.Snapshot) (string, error) {
	b, err := json.MarshalIndent(jsonReportOf(cfg, s), "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// jsonReportSchema is the revision of the report's shape, published as the
// report's `schema` field. It is a separate axis from `version`, which names
// the binary that wrote the report: a consumer pinning toktop to a release
// still has to survive the report gaining a field, and the program version
// alone cannot say whether a field it reads means what it meant a release ago.
//
// It is bumped when a published field is removed, renamed, or changes meaning
// or unit. Adding a field is not a bump: a consumer reading named fields
// ignores what it does not know, and that is the property the revision
// exists to preserve. The number is the contract, so it never moves for a
// change that keeps every published field readable as before.
const jsonReportSchema = 1

type jsonReport struct {
	Schema int `json:"schema"`
	// Version is the toktop that wrote the report, not the report's shape;
	// see jsonReportSchema.
	Version    string       `json:"version"`
	Demo       bool         `json:"demo,omitempty"`
	DemoSeed   *int64       `json:"demo_seed,omitempty"`
	DemoOrigin string       `json:"demo_origin,omitempty"`
	At         time.Time    `json:"at"`
	UptimeSecs float64      `json:"uptime_secs"`
	EnginesUp  int          `json:"engines_up"`
	Engines    []jsonEngine `json:"engines"`
	OutTokPS   float64      `json:"out_tok_per_s"`
	InTokPS    float64      `json:"in_tok_per_s"`
	Agents     []jsonAgent  `json:"agents"`
	Probes     []jsonProbe  `json:"probes"`
	System     *jsonSystem  `json:"system,omitempty"`
	AgentRates []jsonRate   `json:"agent_rates,omitempty"`
}

type jsonEngine struct {
	Label      string      `json:"label"`
	Kind       string      `json:"kind,omitempty"`
	Addr       string      `json:"addr"`
	OK         bool        `json:"ok"`
	Error      string      `json:"error,omitempty"`
	Version    string      `json:"version,omitempty"`
	PID        int         `json:"pid,omitempty"`
	ProcRSSMiB float64     `json:"proc_rss_mib,omitempty"`
	ProcCPU    float64     `json:"proc_cpu_pct,omitempty"`
	Models     []jsonModel `json:"models,omitempty"`
	OutTokPS   float64     `json:"out_tok_per_s"`
	InTokPS    float64     `json:"in_tok_per_s"`
	Running    int         `json:"running"`
	Waiting    int         `json:"waiting"`
	KvPct      float64     `json:"kv_cache_pct"`
	TTFTms     float64     `json:"ttft_ms,omitempty"`
}

type jsonModel struct {
	Name     string `json:"name"`
	SizeVRAM uint64 `json:"size_vram_bytes"`
	CtxMax   uint64 `json:"context_max,omitempty"`
}

type jsonAgent struct {
	At             time.Time `json:"at"`
	ID             string    `json:"id,omitempty"`
	Agent          string    `json:"agent"`
	Model          string    `json:"model,omitempty"`
	Kind           string    `json:"kind,omitempty"`
	PromptTokens   int64     `json:"prompt_tokens"`
	OutputTokens   int64     `json:"output_tokens"`
	ThinkingTokens int64     `json:"thinking_tokens,omitempty"`
	// SpanMs is how long the model spent on this event's tokens, in
	// milliseconds: the same name, unit and bound the ingest wire gives it.
	// It is the denominator core.Summarize prefers, so the report's own
	// agent_rates tok_per_s is not derivable from the report without it. Zero
	// is omitted rather than written, and zero is the documented meaning of an
	// absent span: the rate falls back to the gap between events.
	SpanMs    int64  `json:"span_ms,omitempty"`
	ViaEngine string `json:"via_engine,omitempty"`
	Note      string `json:"note,omitempty"`
}

type jsonRate struct {
	Agent     string    `json:"agent"`
	TokPS     float64   `json:"tok_per_s"`
	PromptPS  float64   `json:"prompt_per_s"`
	Tokens    int64     `json:"tokens"`
	Prompt    int64     `json:"prompt_tokens"`
	Thinking  int64     `json:"thinking_tokens"`
	ViaEngine string    `json:"via_engine,omitempty"`
	Last      time.Time `json:"last"`
}

type jsonProbe struct {
	At     time.Time `json:"at"`
	Addr   string    `json:"addr"`
	Model  string    `json:"model,omitempty"`
	OK     bool      `json:"ok"`
	Error  string    `json:"error,omitempty"`
	TTFTms float64   `json:"ttft_ms,omitempty"`
	TokPS  float64   `json:"tok_per_s"`
	Tokens int       `json:"tokens"`
}

type jsonSystem struct {
	CPUModel     string  `json:"cpu_model,omitempty"`
	OsName       string  `json:"os,omitempty"`
	Kernel       string  `json:"kernel,omitempty"`
	MemTotalMiB  uint64  `json:"mem_total_mib"`
	MemUsedMiB   uint64  `json:"mem_used_mib"`
	SwapTotalMiB uint64  `json:"swap_total_mib"`
	SwapUsedMiB  uint64  `json:"swap_used_mib"`
	Load1        float64 `json:"load1"`
	Load5        float64 `json:"load5"`
	Load15       float64 `json:"load15"`
	HostUptimeS  float64 `json:"host_uptime_secs"`
	// Drivers are the accelerator driver versions the host strip names, keyed
	// by vendor. They are not the per-device jsonGPU driver: this map carries
	// the runtime versions (cuda, amdgpu, the nvidia driver) and the ones read
	// from a remote host, which the dashboard and --plain both print and this
	// report is the only rendering that dropped.
	Drivers    map[string]string `json:"drivers,omitempty"`
	RemoteHost string            `json:"remote_host,omitempty"`
	RemoteErr  string            `json:"remote_error,omitempty"`
	NPUs       []string          `json:"npus,omitempty"`
	Temps      []jsonTemp        `json:"temps,omitempty"`
	GPUs       []jsonGPU         `json:"gpus,omitempty"`
}

type jsonTemp struct {
	Label  string `json:"label"`
	MilliC int    `json:"milli_c"`
	IsGPU  bool   `json:"is_gpu,omitempty"`
}

type jsonGPU struct {
	Vendor      string  `json:"vendor"`
	Index       int     `json:"index"`
	Name        string  `json:"name,omitempty"`
	MilliC      int     `json:"milli_c,omitempty"`
	MemUsedMiB  uint64  `json:"mem_used_mib"`
	MemTotalMiB uint64  `json:"mem_total_mib"`
	UtilPct     float64 `json:"util_pct"`
	PowerW      float64 `json:"power_w,omitempty"`
	Driver      string  `json:"driver,omitempty"`
}

// bytesPerMiB scales the byte counts below into the *_mib fields. It is 2^20,
// not 10^6, matching the MiB/GiB the TUI renderers print; the kernel's
// meminfo kB column is likewise a KiB. The fields are named for the unit they
// carry: a consumer reading mem_total_mib against a 10^6-based expectation is
// still off by 4.9%, but the name no longer claims otherwise.
const bytesPerMiB = 1 << 20

// demoSeed reports the seed only in a demo run, where --seed is in effect.
// Seed 0 is a working seed, so the field carries a pointer: a plain int with
// omitempty would drop exactly that one and leave a replay reading a report
// that never names the seed its frame prints.
func demoSeed(cfg Config) *int64 {
	if !cfg.Demo {
		return nil
	}
	seed := cfg.DemoSeed
	return &seed
}

// originStamp renders the pinned demo origin, empty for a run that started on
// the wall clock. The instant is reported as given rather than in the local
// zone, so a capture taken under two timezones still names the same origin.
//
// At full precision, because this string is the input a replay is fed. The
// timeline is laid out from the origin, and --origin takes a fractional RFC
// 3339 instant, so formatting to whole seconds moved every stamp in the
// replayed run ahead of the capture by the fraction it dropped: the run whose
// bytes the report was read to reproduce was no longer reproducible from it.
// A whole-second origin formats without a fraction either way, so the common
// capture is unchanged.
func originStamp(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339Nano)
}

// stamp is how every instant in the report is rendered: UTC, RFC 3339, same
// format, one zone.
//
// A stamp in a report is not one machine's clock reading. A pushed agent's
// events keep the offset their sender wrote (ingest parses RFC 3339 and
// collector.RecordAgent subtracts a clock lead, neither of which converts),
// while probes and the frame's own `at` carry the local one, so a single
// report can hold 14:02:03+05:30 beside 08:35:12-07:00 and a consumer that
// reads the wall time out of the string without applying the offset places the
// two five hours apart. UTC is the same instant, so this changes no reading
// and leaves the offset in the string, where a conforming parser still finds
// it.
func stamp(t time.Time) time.Time {
	return t.UTC()
}

func jsonReportOf(cfg Config, s core.Snapshot) jsonReport {
	now := frameNow(s, time.Time{})
	sum := core.Summarize(s.Agents, now)
	outAgg, inAgg := aggBoth(s, sum)

	rep := jsonReport{
		Schema:     jsonReportSchema,
		Version:    cfg.Version,
		Demo:       cfg.Demo,
		DemoSeed:   demoSeed(cfg),
		DemoOrigin: originStamp(cfg.DemoOrigin),
		At:         stamp(now),
		UptimeSecs: s.Uptime.Seconds(),
		OutTokPS:   outAgg,
		InTokPS:    inAgg,
		Engines:    make([]jsonEngine, 0, len(s.Providers)),
		Agents:     make([]jsonAgent, 0, len(s.Agents)),
		Probes:     make([]jsonProbe, 0, len(s.Probes)),
		System:     jsonSystemOf(s.Sys),
	}
	for _, p := range s.Providers {
		if p.OK {
			rep.EnginesUp++
		}
		rep.Engines = append(rep.Engines, jsonEngineOf(p))
	}
	for _, a := range s.Agents {
		rep.Agents = append(rep.Agents, jsonAgentOf(a))
	}
	for _, pr := range s.Probes {
		rep.Probes = append(rep.Probes, jsonProbeOf(pr))
	}
	for _, r := range sum.Rates {
		rep.AgentRates = append(rep.AgentRates, jsonRate{
			Agent:     core.SanitizeText(r.Agent),
			TokPS:     r.TokPS,
			PromptPS:  r.PromptPS,
			Tokens:    r.Tokens,
			Prompt:    r.Prompt,
			Thinking:  r.Thinking,
			ViaEngine: core.SanitizeText(r.ViaEngine),
			Last:      stamp(r.Last),
		})
	}
	return rep
}

func jsonEngineOf(p core.ProviderSnapshot) jsonEngine {
	e := jsonEngine{
		Label:      core.SanitizeText(p.Label),
		Kind:       core.SanitizeText(p.Kind),
		Addr:       core.SanitizeText(p.Addr),
		OK:         p.OK,
		Error:      core.SanitizeText(p.Err),
		Version:    core.SanitizeText(p.Version),
		PID:        p.PID,
		ProcRSSMiB: float64(p.ProcRSS) / bytesPerMiB,
		ProcCPU:    p.ProcCPU,
		OutTokPS:   p.OutTokPS,
		InTokPS:    p.InTokPS,
		Running:    p.Running,
		Waiting:    p.Waiting,
		KvPct:      p.KVPct,
		TTFTms:     p.TTFTms,
	}
	for _, m := range p.Models {
		e.Models = append(e.Models, jsonModel{
			Name:     core.SanitizeText(m.Name),
			SizeVRAM: m.SizeVRAM,
			CtxMax:   m.CtxMax,
		})
	}
	return e
}

func jsonAgentOf(a core.AgentEvent) jsonAgent {
	return jsonAgent{
		At:             stamp(a.At),
		ID:             core.SanitizeText(a.ID),
		Agent:          core.SanitizeText(a.Agent),
		Model:          core.SanitizeText(a.Model),
		Kind:           core.SanitizeText(a.Kind),
		PromptTokens:   a.PromptTokens,
		OutputTokens:   a.OutputTokens,
		ThinkingTokens: a.ThinkingTokens,
		SpanMs:         spanMillis(a.Span),
		ViaEngine:      core.SanitizeText(a.ViaEngine),
		Note:           core.SanitizeText(a.Note),
	}
}

// spanMillis is an event's span in whole milliseconds, rounded rather than
// truncated. Duration.Milliseconds truncates, and the local producers measure
// their spans off a monotonic clock, so a span of a few hundred microseconds
// reached this file as 0: the one value the schema defines as "the sender
// does not know the span", which hands the rate back to the gap between
// events and leaves the report's own tok_per_s unrecomputable from the report.
// A span past core.MaxEventSpan is already clamped to zero upstream, and a
// negative one (a sender whose clock stepped back) floors at zero rather than
// reporting a span the model did not take.
func spanMillis(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64((d + time.Millisecond/2) / time.Millisecond)
}

func jsonProbeOf(p core.ProbeSample) jsonProbe {
	return jsonProbe{
		At:     stamp(p.At),
		Addr:   core.SanitizeText(p.Addr),
		Model:  core.SanitizeText(p.Model),
		OK:     p.OK,
		Error:  core.SanitizeText(p.Err),
		TTFTms: p.TTFTms,
		TokPS:  p.TokPS,
		Tokens: p.Tokens,
	}
}

// jsonDrivers sanitizes the host's accelerator driver map on the way into the
// report. A remote target's section is parsed from another host's own output,
// so both halves of the pair are text this process did not write. A pair
// whose vendor sanitizes to nothing is dropped rather than filed under an empty
// key, and a map with nothing left in it is nil, so the field is omitted
// instead of printed as {}.
func jsonDrivers(drivers map[string]string) map[string]string {
	if len(drivers) == 0 {
		return nil
	}
	out := make(map[string]string, len(drivers))
	for vendor, version := range drivers {
		key := core.SanitizeText(vendor)
		if key == "" {
			continue
		}
		out[key] = core.SanitizeText(version)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func jsonSystemOf(s *core.SysSample) *jsonSystem {
	if s == nil {
		return nil
	}
	out := &jsonSystem{
		CPUModel:     core.SanitizeText(s.CPUModel),
		OsName:       core.SanitizeText(s.OsName),
		Kernel:       core.SanitizeText(s.Kernel),
		MemTotalMiB:  s.MemTotal / bytesPerMiB,
		MemUsedMiB:   s.MemUsed / bytesPerMiB,
		SwapTotalMiB: s.SwapTotal / bytesPerMiB,
		SwapUsedMiB:  s.SwapUsed / bytesPerMiB,
		Load1:        s.Load1,
		Load5:        s.Load5,
		Load15:       s.Load15,
		HostUptimeS:  s.HostUptime.Seconds(),
		Drivers:      jsonDrivers(s.Drivers),
		RemoteHost:   core.SanitizeText(s.RemoteHost),
		RemoteErr:    core.SanitizeText(s.RemoteErr),
	}
	for _, n := range s.NPUs {
		if n = core.SanitizeText(n); n != "" {
			out.NPUs = append(out.NPUs, n)
		}
	}
	for _, t := range s.Temps {
		out.Temps = append(out.Temps, jsonTemp{
			Label:  core.SanitizeText(t.Label),
			MilliC: t.MilliC,
			IsGPU:  t.IsGPU,
		})
	}
	for _, g := range s.GPUs {
		out.GPUs = append(out.GPUs, jsonGPU{
			Vendor:      core.SanitizeText(g.Vendor),
			Index:       g.Index,
			Name:        core.SanitizeText(g.Name),
			MilliC:      g.MilliC,
			MemUsedMiB:  g.MemUsed / bytesPerMiB,
			MemTotalMiB: g.MemTotal / bytesPerMiB,
			UtilPct:     g.UtilPct,
			PowerW:      g.PowerW,
			Driver:      core.SanitizeText(g.Driver),
		})
	}
	return out
}
