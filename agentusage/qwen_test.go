// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import "testing"

// The context total is the window the model read, so it cannot be smaller
// than the output the same reading reports. parseQwen took the line's
// totalTokenCount as the window with no floor, where foldCounters (the shared
// fold, which gemini also goes through) floors it at the output. A transcript
// carrying a total of 5 beside 100 candidates published a context no row
// supports, and every consumer of the sample read the pair as a measurement.
func TestQwenTotalIsFlooredAtOutput(t *testing.T) {
	for _, c := range []struct {
		name string
		json string
		want int
	}{
		{"below output", `{"promptTokenCount":10,"candidatesTokenCount":100,"totalTokenCount":5}`, 100},
		{"negative beside output", `{"promptTokenCount":10,"candidatesTokenCount":100,"totalTokenCount":-5}`, 100},
		{"real context", `{"promptTokenCount":10,"candidatesTokenCount":100,"totalTokenCount":900}`, 900},
		{"no output to floor against", `{"promptTokenCount":10,"candidatesTokenCount":0,"totalTokenCount":7}`, 7},
	} {
		t.Run(c.name, func(t *testing.T) {
			line := []byte(`{"type":"assistant","cwd":"/tmp","usageMetadata":` + c.json + `}`)
			v, _, ok := parseQwen(line)
			if !ok {
				t.Fatal("a populated assistant line was rejected")
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
