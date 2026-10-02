// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import "testing"

// The context total is the window the model read, so it cannot be smaller
// than the output the same reading reports. parseCodex took the rollout's own
// total_tokens as the window with no floor, where parseQwen, foldCounters,
// parseGeneric, parseDsh and the opencode sqlite reader floor theirs. A
// rollout carrying a total of 120 beside 5000 output published a context no
// row supports, and every consumer of the sample read the pair as a
// measurement. codex counters are cumulative, so the floor also has to hold
// across readings: the total is the largest seen, and the output keeps
// growing under it until a later record raises the total with it.
func TestCodexTotalIsFlooredAtOutput(t *testing.T) {
	for _, c := range []struct {
		name string
		json string
		want int
	}{
		{"below output", `{"input_tokens":10,"output_tokens":5000,"total_tokens":120}`, 5000},
		{"negative beside output", `{"input_tokens":10,"output_tokens":100,"total_tokens":-5}`, 100},
		{"real context", `{"input_tokens":10,"output_tokens":100,"total_tokens":900}`, 900},
		{"no output to floor against", `{"input_tokens":10,"output_tokens":0,"total_tokens":7}`, 7},
	} {
		t.Run(c.name, func(t *testing.T) {
			line := []byte(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":` + c.json + `}}}`)
			v, _, ok := parseCodex(line)
			if !ok {
				t.Fatal("a populated token_count line was rejected")
			}
			if v.total != c.want {
				t.Fatalf("total = %d, want %d", v.total, c.want)
			}
			if v.output > 0 && v.total < v.output {
				t.Fatalf("total %d below output %d: %+v", v.total, v.output, v)
			}
		})
	}
}
