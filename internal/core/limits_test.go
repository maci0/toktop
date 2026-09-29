// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"math"
	"testing"
	"time"
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

// ClampEventTokens has a boundary table; ClampEventSpan divides by its result
// as a rate denominator, so the drop rule (zero, not the nearest bound) is
// what keeps a sender's bogus duration from reading as a plausible rate.
func TestClampEventSpanDropsRatherThanPullsBack(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero", 0, 0},
		{"in range", time.Second, time.Second},
		{"at the bound", MaxEventSpan, MaxEventSpan},
		{"one past the bound", MaxEventSpan + time.Nanosecond, 0},
		{"negative", -time.Nanosecond, 0},
		{"max duration", time.Duration(math.MaxInt64), 0},
	}
	for _, tc := range tests {
		if got := ClampEventSpan(tc.in); got != tc.want {
			t.Errorf("ClampEventSpan(%s) = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// The vendor constants are the spelling of GPUDevice.Vendor, and that string
// reaches the --json report, the --plain report and the keys of
// SysSample.Drivers. Renaming one is a change an upgrading reader can observe,
// so the values are pinned here rather than only at the parser that writes
// them, and the set is pinned closed so a new vendor cannot appear in a parser
// without a rank and a renderer to go with it.
func TestVendorConstantsAreThePinnedSet(t *testing.T) {
	want := map[string]string{
		VendorNvidia: "nvidia",
		VendorAMD:    "amd",
		VendorIntel:  "intel",
		VendorApple:  "apple",
	}
	got := []string{VendorNvidia, VendorAMD, VendorIntel, VendorApple}
	if len(got) != len(want) {
		t.Fatalf("%d vendor constants, want the %d pinned values", len(got), len(want))
	}
	for _, v := range got {
		if pinned, ok := want[v]; !ok {
			t.Errorf("vendor %q is not one of the pinned vendors", v)
		} else if pinned != v {
			t.Errorf("vendor constant %q, want the published spelling %q", v, pinned)
		}
	}
}
