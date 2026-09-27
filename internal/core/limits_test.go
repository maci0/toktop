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

func TestSatAddU64(t *testing.T) {
	tests := []struct {
		a, b, want uint64
	}{
		{0, 0, 0},
		{5, 7, 12},
		{math.MaxUint64, 0, math.MaxUint64},
		{math.MaxUint64, 1, math.MaxUint64}, // must saturate, not wrap to 0
		{0, math.MaxUint64, math.MaxUint64},
		{1 << 40, 1 << 40, 1 << 41},
	}
	for _, tc := range tests {
		if got := SatAddU64(tc.a, tc.b); got != tc.want {
			t.Errorf("SatAddU64(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestMulSatU64(t *testing.T) {
	tests := []struct {
		a, b, want uint64
	}{
		{0, 0, 0},
		{1, 0, 0}, // a zero factor yields zero rather than dividing by it
		{3, 4096, 12288},
		{1 << 20, 1 << 20, 1 << 40},
		{math.MaxUint64, 4096, math.MaxUint64}, // must saturate, not wrap small
		{^uint64(0) >> 10, 1 << 10, (^uint64(0) >> 10) << 10},
		{^uint64(0)>>10 + 1, 1 << 10, math.MaxUint64},
	}
	for _, tc := range tests {
		if got := MulSatU64(tc.a, tc.b); got != tc.want {
			t.Errorf("MulSatU64(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
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

func TestSatAddPos(t *testing.T) {
	tests := []struct {
		a, b, want int64
	}{
		{0, 0, 0},
		{5, 7, 12},
		{-1, 5, 5}, // a negative leg is not a count
		{5, -1, 5},
		{math.MaxInt64, 0, math.MaxInt64},
		{math.MaxInt64, 1, math.MaxInt64}, // must saturate, not wrap negative
		{1 << 40, 1 << 40, 1 << 41},
	}
	for _, tc := range tests {
		if got := satAddPos(tc.a, tc.b); got != tc.want {
			t.Errorf("satAddPos(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
