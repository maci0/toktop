// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package ui

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// A rate is a counter delta over an interval, so a short interval can put two
// engines on one frame at a sum that leaves the float64 range. That total
// overflowed to +Inf here before the running total saturated, and JSONFrame
// cannot encode an infinity: a --once --json run printed no report at all
// instead of the numbers, and the drawn header showed "+Inf tok/s" beside
// engines that had been measured perfectly well.
func TestAggSaturatesInsteadOfOverflowingToInf(t *testing.T) {
	s := core.Snapshot{Providers: []core.ProviderSnapshot{
		{OK: true, OutTokPS: math.MaxFloat64, InTokPS: math.MaxFloat64},
		{OK: true, OutTokPS: math.MaxFloat64, InTokPS: math.MaxFloat64},
	}}
	out, in := aggBoth(s, core.AgentSummary{})
	for name, got := range map[string]float64{"out": out, "in": in} {
		if math.IsInf(got, 0) || math.IsNaN(got) {
			t.Errorf("agg%s = %v, want a finite total", name, got)
		}
		if got != math.MaxFloat64 {
			t.Errorf("agg%s = %v, want the saturated MaxFloat64", name, got)
		}
	}

	// The saturation is a property of the sum, not of a losing engine: one
	// engine alone still reports exactly what it measured.
	single := core.Snapshot{Providers: []core.ProviderSnapshot{
		{OK: true, OutTokPS: 12.5, InTokPS: 3.5},
	}}
	out, in = aggBoth(single, core.AgentSummary{})
	if out != 12.5 || in != 3.5 {
		t.Errorf("aggBoth = %v/%v, want 12.5/3.5 unchanged", out, in)
	}

	// And the report those totals feed still serializes, which is the point.
	if _, err := JSONFrame(Config{Version: "t"}, s); err != nil {
		t.Fatalf("JSONFrame refused a frame whose totals saturate: %v", err)
	}

	// A corrupt reading and a very large one are answered differently: a sum
	// that left the range keeps its sign and pins to the bound, a NaN reads as
	// no throughput, and neither reaches the report as something unusable.
	for _, tc := range []struct {
		name            string
		out, in         float64
		wantOut, wantIn float64
	}{
		{"underflowed too", -math.MaxFloat64, -math.MaxFloat64, -math.MaxFloat64, -math.MaxFloat64},
		{"both directions overflow", math.Inf(1), math.Inf(-1), math.MaxFloat64, -math.MaxFloat64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := core.Snapshot{Providers: []core.ProviderSnapshot{
				{OK: true, OutTokPS: tc.out, InTokPS: tc.in},
				{OK: true, OutTokPS: tc.out, InTokPS: tc.in},
			}}
			gotOut, gotIn := aggBoth(frame, core.AgentSummary{})
			if gotOut != tc.wantOut || gotIn != tc.wantIn {
				t.Errorf("aggBoth = %v/%v, want %v/%v", gotOut, gotIn, tc.wantOut, tc.wantIn)
			}
			if _, err := JSONFrame(Config{Version: "t"}, frame); err != nil {
				t.Errorf("JSONFrame refused %v/%v: %v", tc.out, tc.in, err)
			}
		})
	}

	if got := addRate(math.NaN(), 1); got != 0 {
		t.Errorf("addRate(NaN, 1) = %v, want 0: a corrupt reading is no throughput", got)
	}

	// The aggregate is only half of it: JSONFrame writes each engine's own
	// reading out unsummed, so bounding the totals left the report still
	// refusing to encode whenever a single engine's rate was out of range.
	// This is the case the fuzzer found.
	frame := core.Snapshot{
		Providers: []core.ProviderSnapshot{
			{OK: true, Label: "hot", OutTokPS: math.Inf(1), InTokPS: math.Inf(-1), KVPct: math.NaN()},
		},
		Agents: []core.AgentEvent{{At: time.Now(), Agent: "a", OutputTokens: 1}},
		Probes: []core.ProbeSample{{At: time.Now(), Addr: "127.0.0.1:1", TokPS: math.Inf(1), TTFTms: math.NaN()}},
		Sys: &core.SysSample{
			Load1: math.Inf(1), Load5: math.NaN(), HostUptime: time.Duration(math.MaxInt64),
		},
	}
	report, err := JSONFrame(Config{Version: "t"}, frame)
	if err != nil {
		t.Fatalf("JSONFrame refused a frame whose per-engine readings overflow: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(report), &doc); err != nil {
		t.Fatalf("report does not decode: %v\n%s", err, report)
	}
	// Nothing the report publishes may be something its consumers cannot use.
	for _, key := range []string{"out_tok_per_s", "in_tok_per_s", "uptime_secs"} {
		if v, ok := doc[key].(float64); ok {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Errorf("report %s = %v, which a consumer cannot use", key, v)
			}
		}
	}
	engines, _ := doc["engines"].([]any)
	if len(engines) != 1 {
		t.Fatalf("report has %d engines, want 1", len(engines))
	}
	engine, _ := engines[0].(map[string]any)
	for key, v := range map[string]any{
		"out_tok_per_s": engine["out_tok_per_s"],
		"in_tok_per_s":  engine["in_tok_per_s"],
		"kv_pct":        engine["kv_pct"],
	} {
		if f, ok := v.(float64); ok && (math.IsNaN(f) || math.IsInf(f, 0)) {
			t.Errorf("engine %s = %v, which a consumer cannot use", key, f)
		}
	}
}
