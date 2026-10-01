// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import "testing"

// The context total is the window the model read, so it cannot be smaller
// than the output the same reading reports. The opencode sqlite reader got
// floorTotal for this; foldCounters, which every JSONL adapter goes through,
// rebuilt a total only when the source omitted it, so a source carrying one
// below its own output published a context window of 5 beside an output of 100
// and every consumer of the sample read the pair as a measurement.
func TestFoldCountersTotalIsFlooredAtOutput(t *testing.T) {
	for _, c := range []struct {
		name               string
		prompt, out, think int
		cached, total      int
		want               int
	}{
		{"below output", 10, 100, 0, 0, 5, 100},
		{"absent beside output", 10, 100, 0, 0, 0, 110},
		{"negative beside output", 10, 100, 0, 0, -5, 110},
		{"real context", 10, 100, 0, 0, 900, 900},
		{"no output to floor against", 10, 0, 0, 0, 7, 7},
	} {
		t.Run(c.name, func(t *testing.T) {
			v, ok := foldCounters(c.prompt, c.out, c.think, c.cached, c.total)
			if !ok {
				t.Fatal("a populated record was rejected")
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
