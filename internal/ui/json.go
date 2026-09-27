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

type jsonReport struct {
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
	Label     string      `json:"label"`
	Kind      string      `json:"kind,omitempty"`
	Addr      string      `json:"addr"`
	OK        bool        `json:"ok"`
	Error     string      `json:"error,omitempty"`
	Version   string      `json:"version,omitempty"`
	PID       int         `json:"pid,omitempty"`
	ProcRSSMB float64     `json:"proc_rss_mb,omitempty"`
	ProcCPU   float64     `json:"proc_cpu_pct,omitempty"`
	Models    []jsonModel `json:"models,omitempty"`
	OutTokPS  float64     `json:"out_tok_per_s"`
	InTokPS   float64     `json:"in_tok_per_s"`
	Running   int         `json:"running"`
	Waiting   int         `json:"waiting"`
	KvPct     float64     `json:"kv_cache_pct"`
	TTFTms    float64     `json:"ttft_ms,omitempty"`
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
	ViaEngine      string    `json:"via_engine,omitempty"`
	Note           string    `json:"note,omitempty"`
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
	CPUModel    string     `json:"cpu_model,omitempty"`
	OsName      string     `json:"os,omitempty"`
	Kernel      string     `json:"kernel,omitempty"`
	MemTotalMB  uint64     `json:"mem_total_mb"`
	MemUsedMB   uint64     `json:"mem_used_mb"`
	SwapTotalMB uint64     `json:"swap_total_mb"`
	SwapUsedMB  uint64     `json:"swap_used_mb"`
	Load1       float64    `json:"load1"`
	Load5       float64    `json:"load5"`
	Load15      float64    `json:"load15"`
	HostUptimeS float64    `json:"host_uptime_secs"`
	RemoteHost  string     `json:"remote_host,omitempty"`
	RemoteErr   string     `json:"remote_error,omitempty"`
	NPUs        []string   `json:"npus,omitempty"`
	Temps       []jsonTemp `json:"temps,omitempty"`
	GPUs        []jsonGPU  `json:"gpus,omitempty"`
}

type jsonTemp struct {
	Label  string `json:"label"`
	MilliC int    `json:"milli_c"`
	IsGPU  bool   `json:"is_gpu,omitempty"`
}

type jsonGPU struct {
	Vendor     string  `json:"vendor"`
	Index      int     `json:"index"`
	Name       string  `json:"name,omitempty"`
	MilliC     int     `json:"milli_c,omitempty"`
	MemUsedMB  uint64  `json:"mem_used_mb"`
	MemTotalMB uint64  `json:"mem_total_mb"`
	UtilPct    float64 `json:"util_pct"`
	PowerW     float64 `json:"power_w,omitempty"`
	Driver     string  `json:"driver,omitempty"`
}

const bytesPerMB = 1 << 20

// originStamp renders the pinned demo origin, empty for a run that started on
// the wall clock. The instant is reported as given rather than in the local
// zone, so a capture taken under two timezones still names the same origin.
func originStamp(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

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

func jsonReportOf(cfg Config, s core.Snapshot) jsonReport {
	now := frameNow(s, time.Time{})
	sum := core.Summarize(s.Agents, now)
	outAgg, inAgg := aggBoth(s, sum)

	rep := jsonReport{
		Version:    cfg.Version,
		Demo:       cfg.Demo,
		DemoSeed:   demoSeed(cfg),
		DemoOrigin: originStamp(cfg.DemoOrigin),
		At:         now,
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
			Last:      r.Last,
		})
	}
	return rep
}

func jsonEngineOf(p core.ProviderSnapshot) jsonEngine {
	e := jsonEngine{
		Label:     core.SanitizeText(p.Label),
		Kind:      core.SanitizeText(p.Kind),
		Addr:      core.SanitizeText(p.Addr),
		OK:        p.OK,
		Error:     core.SanitizeText(p.Err),
		Version:   core.SanitizeText(p.Version),
		PID:       p.PID,
		ProcRSSMB: float64(p.ProcRSS) / bytesPerMB,
		ProcCPU:   p.ProcCPU,
		OutTokPS:  p.OutTokPS,
		InTokPS:   p.InTokPS,
		Running:   p.Running,
		Waiting:   p.Waiting,
		KvPct:     p.KVPct,
		TTFTms:    p.TTFTms,
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
		At:             a.At,
		ID:             core.SanitizeText(a.ID),
		Agent:          core.SanitizeText(a.Agent),
		Model:          core.SanitizeText(a.Model),
		Kind:           core.SanitizeText(a.Kind),
		PromptTokens:   a.PromptTokens,
		OutputTokens:   a.OutputTokens,
		ThinkingTokens: a.ThinkingTokens,
		ViaEngine:      core.SanitizeText(a.ViaEngine),
		Note:           core.SanitizeText(a.Note),
	}
}

func jsonProbeOf(p core.ProbeSample) jsonProbe {
	return jsonProbe{
		At:     p.At,
		Addr:   core.SanitizeText(p.Addr),
		Model:  core.SanitizeText(p.Model),
		OK:     p.OK,
		Error:  core.SanitizeText(p.Err),
		TTFTms: p.TTFTms,
		TokPS:  p.TokPS,
		Tokens: p.Tokens,
	}
}

func jsonSystemOf(s *core.SysSample) *jsonSystem {
	if s == nil {
		return nil
	}
	out := &jsonSystem{
		CPUModel:    core.SanitizeText(s.CPUModel),
		OsName:      core.SanitizeText(s.OsName),
		Kernel:      core.SanitizeText(s.Kernel),
		MemTotalMB:  s.MemTotal / bytesPerMB,
		MemUsedMB:   s.MemUsed / bytesPerMB,
		SwapTotalMB: s.SwapTotal / bytesPerMB,
		SwapUsedMB:  s.SwapUsed / bytesPerMB,
		Load1:       s.Load1,
		Load5:       s.Load5,
		Load15:      s.Load15,
		HostUptimeS: s.HostUptime.Seconds(),
		RemoteHost:  core.SanitizeText(s.RemoteHost),
		RemoteErr:   core.SanitizeText(s.RemoteErr),
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
			Vendor:     core.SanitizeText(g.Vendor),
			Index:      g.Index,
			Name:       core.SanitizeText(g.Name),
			MilliC:     g.MilliC,
			MemUsedMB:  g.MemUsed / bytesPerMB,
			MemTotalMB: g.MemTotal / bytesPerMB,
			UtilPct:    g.UtilPct,
			PowerW:     g.PowerW,
			Driver:     core.SanitizeText(g.Driver),
		})
	}
	return out
}
