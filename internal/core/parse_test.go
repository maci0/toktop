// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"math"
	"testing"
)

func TestSatCoercions(t *testing.T) {
	if got := SatInt(2.7); got != 2 {
		t.Errorf("SatInt(2.7) = %d, want 2", got)
	}
	if got := SatUint(8192); got != 8192 {
		t.Errorf("SatUint(8192) = %d", got)
	}
	for _, v := range []float64{0, -5, math.NaN()} {
		if got := SatInt(v); got != 0 {
			t.Errorf("SatInt(%v) = %d, want 0", v, got)
		}
		if got := SatUint(v); got != 0 {
			t.Errorf("SatUint(%v) = %d, want 0", v, got)
		}
	}
	if SatInt(1e300) != math.MaxInt || SatUint(1e300) != math.MaxUint64 {
		t.Error("huge magnitudes must saturate, not wrap")
	}
}

func TestContainsAny(t *testing.T) {
	if !ContainsAny("vllm:requests_running", "running", "waiting") {
		t.Error("matching substring not found")
	}
	if ContainsAny("ollama:model_loaded", "running", "waiting") {
		t.Error("absent substrings reported as found")
	}
	if ContainsAny("anything") {
		t.Error("no substrings must not match")
	}
}
