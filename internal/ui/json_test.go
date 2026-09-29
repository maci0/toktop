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

// The report names the binary that wrote it, but the program version cannot
// tell a consumer whether a field it reads still means what it meant a
// release ago: the same field can change unit or sense across two minors
// under one schema. The schema field is the report's own revision, on its own
// axis, so a consumer pins to it and treats `version` as provenance.
func TestJSONReportNamesItsSchema(t *testing.T) {
	doc := decodeReport(t, core.Snapshot{At: time.Now()})
	if got := doc["schema"]; got != float64(jsonReportSchema) {
		t.Errorf("schema = %v, want %d", got, jsonReportSchema)
	}
	if _, ok := doc["version"]; !ok {
		t.Errorf("report carries no version to name the binary that wrote it: %v", doc)
	}
}

// Every list in the report is published as an array, whether or not it holds
// anything, so a consumer reads one shape from a quiet run and a busy one.
// agent_rates is the one that carried an omitempty, which made the key vanish
// on a fleet that reported nothing while engines, agents and probes stayed
// present as [].
func TestJSONReportPublishesEveryListAsAnArray(t *testing.T) {
	doc := decodeReport(t, core.Snapshot{At: time.Now()})
	for _, key := range []string{"engines", "agents", "probes", "agent_rates"} {
		v, ok := doc[key]
		if !ok {
			t.Errorf("report has no %q on a snapshot with nothing in it: %v", key, doc)
			continue
		}
		if _, isArray := v.([]any); !isArray {
			t.Errorf("%q = %#v, want an array", key, v)
		}
	}
}

// The engines list is the only part of the report a script reads to know what
// is being measured at all, and the array check above reaches it only on a
// snapshot with nothing in it, so nothing pinned the fields inside. Every
// field jsonEngineOf reads has to survive the round trip at its own unit, and
// the units are where a copy of this map goes wrong: bytes become MiB, and the
// rates, the KV percentage and the TTFT are passed through unscaled while the
// memory is not.
func TestJSONEngineCarriesEveryFieldItReads(t *testing.T) {
	doc := decodeReport(t, core.Snapshot{
		At: time.Now(),
		Providers: []core.ProviderSnapshot{{
			Label:    "sglang",
			Kind:     "sglang",
			Addr:     "127.0.0.1:30000",
			OK:       true,
			Version:  "0.4.6",
			PID:      4242,
			ProcRSS:  512 << 20, // 512 MiB
			ProcCPU:  37.5,
			Models:   []core.ModelInfo{{Name: "qwen3-32b", SizeVRAM: 32 << 30, CtxMax: 40960}},
			OutTokPS: 12.5,
			InTokPS:  340.25,
			Running:  3,
			Waiting:  1,
			KVPct:    42.5,
			TTFTms:   88,
		}},
	})

	engines, _ := doc["engines"].([]any)
	if len(engines) != 1 {
		t.Fatalf("report carried %d engines, want 1: %v", len(engines), doc)
	}
	e := engines[0].(map[string]any)
	for _, key := range []string{
		"label", "kind", "addr", "ok", "version", "pid", "proc_cpu_pct",
		"out_tok_per_s", "in_tok_per_s", "running", "waiting", "kv_cache_pct", "ttft_ms",
	} {
		if _, ok := e[key]; !ok {
			t.Errorf("engine has no %q: %v", key, e)
		}
	}
	// proc_rss_mib is the one scaled field, so a copy of the map that forgets
	// the division publishes 536870912 where a consumer reads MiB.
	if got := e["proc_rss_mib"]; got != float64(512) {
		t.Errorf("proc_rss_mib = %v, want 512", got)
	}
	for key, want := range map[string]float64{
		"proc_cpu_pct": 37.5, "out_tok_per_s": 12.5, "in_tok_per_s": 340.25,
		"running": 3, "waiting": 1, "kv_cache_pct": 42.5, "ttft_ms": 88, "pid": 4242,
	} {
		if got := e[key]; got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}

	models, _ := e["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("engine carried %d models, want 1: %v", len(models), e)
	}
	m := models[0].(map[string]any)
	if m["name"] != "qwen3-32b" || m["size_vram_bytes"] != float64(32<<30) || m["context_max"] != float64(40960) {
		t.Errorf("model = %v, want qwen3-32b at 32GiB with a 40960 context", m)
	}
}

// A report whose engine rows come off the wire has to be as safe to print as
// the terminal rendering next to it: the version, the model name, the label
// and the error string all come from whatever answered on the port, and the
// escape sequences in them are what a consumer's terminal executes when it
// cats the report. Sanitizing one and not another is invisible until the one
// left in does.
func TestJSONEngineSanitizesAttackerControlledText(t *testing.T) {
	const esc = "\x1b[31m"
	doc := decodeReport(t, core.Snapshot{
		At: time.Now(),
		Providers: []core.ProviderSnapshot{{
			Label:   "engine" + esc + "1m",
			Kind:    "openai",
			Addr:    "127.0.0.1:1",
			OK:      false,
			Err:     "connect failed\n" + esc + "2J" + "\x07",
			Version: esc + "0.1",
			Models:  []core.ModelInfo{{Name: "llama\x003"}},
		}},
	})
	e := doc["engines"].([]any)[0].(map[string]any)

	for key, want := range map[string]string{
		"label":   "engine1m",
		"kind":    "openai",
		"addr":    "127.0.0.1:1",
		"error":   "connect failed\n2J",
		"version": "0.1",
	} {
		got, _ := e[key].(string)
		if got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	model := e["models"].([]any)[0].(map[string]any)
	if got, _ := model["name"].(string); got != "llama3" {
		t.Errorf("models[0].name = %q, want llama3", got)
	}
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

// A span shorter than a millisecond is still a span. The field's zero means
// "the sender does not know it", and a truncated sub-millisecond span put a
// real denominator in that state, so the report's own tok_per_s could not be
// recomputed from the report that carries it.
func TestSpanMsRoundsSubMillisecondSpan(t *testing.T) {
	now := time.Date(2026, 3, 29, 8, 35, 12, 0, time.UTC)
	for _, tc := range []struct {
		name string
		in   time.Duration
		want int64
	}{
		{"absent", 0, 0},
		{"negative, a sender whose clock stepped back", -3 * time.Millisecond, 0},
		{"rounds up from under a millisecond", 900 * time.Microsecond, 1},
		{"rounds up over the half", 1500 * time.Microsecond, 2},
		{"rounds down under the half", 1400 * time.Microsecond, 1},
		{"a whole millisecond is unchanged", 2000 * time.Millisecond, 2000},
	} {
		if got := spanMillis(tc.in); got != tc.want {
			t.Errorf("spanMillis(%s) = %d, want %d", tc.name, got, tc.want)
		}
		agent := jsonAgentOf(core.AgentEvent{At: now, Span: tc.in})
		if agent.SpanMs != tc.want {
			t.Errorf("jsonAgentOf span %s: span_ms = %d, want %d", tc.name, agent.SpanMs, tc.want)
		}
	}
}
