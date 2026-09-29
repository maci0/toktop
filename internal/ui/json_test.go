// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package ui

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// The report publishes a per-agent rate and the token counts it was derived
// from, so a consumer can check one against the other. The denominator was
// missing: core.Summarize prefers the sender-reported span over the gap
// between events, and the span was carried nowhere in the report, leaving
// agent_rates[].tok_per_s uncheckable against the agents[] rows beside it.

func decodeReport(t *testing.T, s core.Snapshot) map[string]any {
	t.Helper()
	out, err := JSONFrame(Config{Version: "test"}, s)
	if err != nil {
		t.Fatalf("JSONFrame: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("report is not JSON: %v\n%s", err, out)
	}
	return doc
}

func TestJSONAgentCarriesTheSpanItsRateUses(t *testing.T) {
	now := time.Now()
	snap := core.Snapshot{
		At:     now,
		Agents: []core.AgentEvent{{At: now, Agent: "coder", Kind: core.AgentKindTurn, OutputTokens: 400, Span: 2 * time.Second}},
	}
	doc := decodeReport(t, snap)

	agents, _ := doc["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("report carried %d agents, want 1", len(agents))
	}
	ev := agents[0].(map[string]any)
	if got := ev["span_ms"]; got != float64(2000) {
		t.Fatalf("span_ms = %v, want 2000", got)
	}

	// The rate the report publishes is that span as the denominator, so the
	// two now reconcile: 400 tokens over 2s is 200 tok/s.
	rates, _ := doc["agent_rates"].([]any)
	if len(rates) != 1 {
		t.Fatalf("report carried %d agent rates, want 1", len(rates))
	}
	if got := rates[0].(map[string]any)["tok_per_s"]; got != float64(200) {
		t.Fatalf("tok_per_s = %v, want 200", got)
	}
}

// Zero is the documented meaning of an absent span (the rate falls back to
// the gap between events), so an event that reports none omits the field
// rather than writing a 0 a consumer has to read as a real duration.
func TestJSONAgentOmitsAnAbsentSpan(t *testing.T) {
	now := time.Now()
	doc := decodeReport(t, core.Snapshot{At: now, Agents: []core.AgentEvent{{At: now, Agent: "coder", OutputTokens: 10}}})
	ev := doc["agents"].([]any)[0].(map[string]any)
	if _, ok := ev["span_ms"]; ok {
		t.Fatalf("an event with no span reported one: %v", ev)
	}
}

// The dashboard strip and --plain both name the host's accelerator driver
// versions; --json was the one rendering that dropped them, and a report
// consumed by a script cannot read a field that is not there.
func TestJSONSystemCarriesHostDrivers(t *testing.T) {
	doc := decodeReport(t, core.Snapshot{Sys: &core.SysSample{
		CPUModel: "EPYC",
		Drivers:  map[string]string{"nvidia": "570.86.15", "cuda": "12.4"},
	}})
	sys := doc["system"].(map[string]any)
	drivers, _ := sys["drivers"].(map[string]any)
	if got := drivers["nvidia"]; got != "570.86.15" {
		t.Errorf("drivers[nvidia] = %v, want 570.86.15", got)
	}
	if got := drivers["cuda"]; got != "12.4" {
		t.Errorf("drivers[cuda] = %v, want 12.4", got)
	}
}

func TestJSONSystemOmitsAbsentDrivers(t *testing.T) {
	doc := decodeReport(t, core.Snapshot{Sys: &core.SysSample{CPUModel: "EPYC"}})
	sys := doc["system"].(map[string]any)
	if _, ok := sys["drivers"]; ok {
		t.Fatalf("a host with no drivers reported some: %v", sys)
	}
}

// A remote host's driver versions arrive parsed out of another machine's own
// output, so both halves of the pair are sanitized before they reach the
// report, and a vendor that sanitizes away is not filed under an empty key.
func TestJSONSystemSanitizesDrivers(t *testing.T) {
	doc := decodeReport(t, core.Snapshot{Sys: &core.SysSample{
		Drivers: map[string]string{"nvidia": "\x1b[31m570\x1b[0m", "\x1b[2J": "9.9.9"},
	}})
	drivers := doc["system"].(map[string]any)["drivers"].(map[string]any)
	for key, val := range drivers {
		if key == "" {
			t.Errorf("a driver was filed under an empty vendor: %v", drivers)
		}
		if s, _ := val.(string); s != "570" {
			t.Errorf("drivers[%q] = %q, want the escape sequences stripped", key, s)
		}
	}
}

// A pushed agent's events keep the offset their sender wrote, while the
// frame's own stamps carry the local one. Both zones in one report means a
// consumer reading the wall time out of the string places the two hours or
// more apart, so every stamp is rendered in UTC.
func TestJSONStampsAreUTCWhoseverTheZone(t *testing.T) {
	kolkata := time.FixedZone("IST", 5*3600+1800)
	now := time.Date(2026, 3, 29, 8, 35, 12, 0, time.UTC)
	snap := core.Snapshot{
		At:     now.Local(),
		Agents: []core.AgentEvent{{At: now.In(kolkata), Agent: "coder", Kind: core.AgentKindTurn, OutputTokens: 10}},
		Probes: []core.ProbeSample{{At: now.In(kolkata), Addr: "127.0.0.1:8080"}},
	}
	doc := decodeReport(t, snap)

	if got := doc["at"].(string); got != "2026-03-29T08:35:12Z" {
		t.Errorf("report at = %q, want UTC", got)
	}
	agent := doc["agents"].([]any)[0].(map[string]any)
	if got := agent["at"].(string); got != "2026-03-29T08:35:12Z" {
		t.Errorf("agents[0].at = %q, want the sender's instant in UTC", got)
	}
	probe := doc["probes"].([]any)[0].(map[string]any)
	if got := probe["at"].(string); got != "2026-03-29T08:35:12Z" {
		t.Errorf("probes[0].at = %q, want UTC", got)
	}
}
