// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package ui

import (
	"fmt"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// perfSnap is a full-scale dashboard: history at its retention cap, the agent
// feed at its cap, and the probe ring at its cap. A frame that renders this
// without allocating is the budget TestStaticFrameAllocs asserts.
func perfSnap() core.Snapshot {
	now := time.Unix(1789581724, 0)
	out := make([]float64, core.HistoryLen)
	in := make([]float64, core.HistoryLen)
	for i := range out {
		out[i] = 20 + float64(i%37)*3.5
		in[i] = 400 + float64(i%53)*11
	}
	provs := make([]core.ProviderSnapshot, 3)
	for i := range provs {
		provs[i] = core.ProviderSnapshot{
			Label: fmt.Sprintf("engine-%d", i), Kind: core.KindVLLM, OK: i != 2,
			Err:      map[bool]string{true: "", false: "connection refused"}[i != 2],
			Version:  "0.12.1",
			Addr:     fmt.Sprintf("http://127.0.0.1:%d", 8000+i),
			Models:   []core.ModelInfo{{Name: "llama3:8b-instruct-q5_K_M"}, {Name: "qwen2.5:32b"}},
			OutTokPS: 42.7 + float64(i), InTokPS: 1200.5, KVPct: 55, Running: 2, Waiting: 1,
			OutT0: now, InT0: now, OutHist: out, InHist: in,
		}
	}
	agents := make([]core.AgentEvent, core.AgentHistoryLen)
	for i := range agents {
		agents[i] = core.AgentEvent{
			At: now.Add(time.Duration(i) * time.Second), ID: fmt.Sprint(i),
			Agent: []string{"claude", "codex", "dsh"}[i%3], Model: "deepseek-flash",
			Kind:         []string{core.AgentKindTurn, core.AgentKindTool, core.AgentKindNote}[i%3],
			Note:         "edit internal/ui/ui.go",
			PromptTokens: int64(100 + i), OutputTokens: int64(20 + i%97),
		}
	}
	probes := make([]core.ProbeSample, core.ProbeHistoryLen)
	for i := range probes {
		probes[i] = core.ProbeSample{At: now.Add(time.Duration(i) * time.Second), Addr: "http://127.0.0.1:8000",
			Model: "llama3:8b", OK: i%5 != 0, Err: "context deadline exceeded", TTFTms: 120.5, TokPS: 31.2}
	}
	return core.Snapshot{
		At: now, Uptime: 3 * time.Hour,
		Providers: provs,
		Probes:    probes,
		Agents:    agents,
		Sys: &core.SysSample{
			MemTotal: 64 << 30, MemUsed: 31 << 30, SwapTotal: 8 << 30, SwapUsed: 1 << 30,
			CPUModel: "AMD Ryzen 9 7950X", OsName: "Fedora Linux", Kernel: "6.11.0",
			Load1: 3.2, Load5: 2.8, Load15: 2.1, HostUptime: 3 * time.Hour,
			Drivers: map[string]string{"nvidia": "550.1", "amdgpu": "6.11"},
			Temps:   []core.TempReading{{Label: "Tctl", MilliC: 64000}, {Label: "edge", MilliC: 58000}},
			GPUs: []core.GPUDevice{
				{Vendor: "nvidia", Index: 0, Name: "RTX 4090", MilliC: 70000, UtilPct: 88, MemUsed: 20 << 30, MemTotal: 24 << 30},
				{Vendor: "amd", Index: 1, Name: "RX 7900", MilliC: 61000, UtilPct: 40, MemUsed: 12 << 30, MemTotal: 24 << 30},
			},
			RemoteHost: "box",
		},
	}
}

var perfFrameSizes = [][2]int{{120, 40}, {200, 50}}

// allocBudget is the garbage one full frame at perfSnap's scale may create.
// Measured at 6267 (120x40) and 8315 (200x50) after the chart fade stopped
// parsing a hex color per bisection step; the budget leaves ~8% headroom so a
// benign allocation shift does not fail the gate but a regression that
// reinstates per-cell parse/format churn does.
var allocBudget = map[[2]int]float64{
	{120, 40}: 6800,
	{200, 50}: 9000,
}

// TestStaticFrameAllocBudget is the deterministic gate for the render path.
// Allocation counts do not move with frequency scaling or noisy neighbours
// the way ns/op does, so this holds on a loaded CI runner. The instruction
// counts behind the current numbers, per frame at 200x50 on a Ryzen 9 9950X
// (go1.27.1, pinned with taskset, perf stat -r 3 over 200 frames):
//
//	before  65.4M instructions, 14.0M branches, 22.6k allocs
//	after   39.1M instructions,  8.0M branches,  8.3k allocs
func TestStaticFrameAllocBudget(t *testing.T) {
	cfg := Config{Version: "0.10.0", IngestAddr: "127.0.0.1:8420", Agents: true}
	snap := perfSnap()
	for _, sz := range perfFrameSizes {
		budget, ok := allocBudget[sz]
		if !ok {
			t.Fatalf("no budget for %dx%d", sz[0], sz[1])
		}
		got := testing.AllocsPerRun(20, func() {
			_ = StaticFrame(cfg, snap, sz[0], sz[1])
		})
		if got > budget {
			t.Errorf("StaticFrame %dx%d allocates %.0f objects/frame, budget %.0f; "+
				"the frame path must not parse or format per cell", sz[0], sz[1], got, budget)
		}
	}
}

func BenchmarkStaticFrame(b *testing.B) {
	cfg := Config{Version: "0.10.0", IngestAddr: "127.0.0.1:8420", Agents: true}
	for _, sz := range perfFrameSizes {
		snap := perfSnap()
		b.Run(fmt.Sprintf("%dx%d", sz[0], sz[1]), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = StaticFrame(cfg, snap, sz[0], sz[1])
			}
		})
	}
}

func BenchmarkPlainFrame(b *testing.B) {
	cfg := Config{Version: "0.10.0", IngestAddr: "127.0.0.1:8420", Agents: true}
	snap := perfSnap()
	b.ReportAllocs()
	for b.Loop() {
		_ = PlainTextFrame(cfg, snap)
	}
}
