// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build linux

package core

import (
	"strconv"
	"strings"
	"testing"
)

// A /proc/PID/stat body as the kernel writes it, with a comm carrying both a
// space and a paren so a walk that splits on whitespace or on the first ')'
// reads the wrong fields.
const procStatSample = "1234 (Web Content (x)) S 1 1234 1234 0 -1 4194304 " +
	"9123 0 0 0 11 42 0 0 20 0 7 0 987654 12345678 512 " +
	"18446744073709551615 1 2 3 4 5"

// The field numbers are the whole contract: a caller asking for 22 and getting
// the one after it reads a plausible wrong number instead of failing.
func TestWalkProcStatNumbersFields(t *testing.T) {
	got := map[int]string{}
	WalkProcStat(procStatSample, 24, func(field int, value string) bool {
		got[field] = value
		return true
	})

	for _, tc := range []struct {
		field int
		want  string
	}{
		{3, "S"},   // state, the first field past the comm
		{4, "1"},   // ppid
		{14, "11"}, // utime
		{15, "42"}, // stime
		{22, "987654"},
		{23, "12345678"},
		{24, "512"},
	} {
		if got[tc.field] != tc.want {
			t.Errorf("field %d = %q, want %q", tc.field, got[tc.field], tc.want)
		}
	}
}

// The walk stops at the field the caller named, so a caller reading starttime
// does not walk the rest of a ~50 field line to no end.
func TestWalkProcStatStopsAtLast(t *testing.T) {
	var fields []int
	WalkProcStat(procStatSample, 5, func(field int, _ string) bool {
		fields = append(fields, field)
		return true
	})
	if got, want := len(fields), 3; got != want {
		t.Errorf("walked %d fields, want %d", got, want)
	}
}

// A caller that got its field reports it and the walk ends there.
func TestWalkProcStatStopsWhenFnReportsDone(t *testing.T) {
	var fields []int
	WalkProcStat(procStatSample, 24, func(field int, _ string) bool {
		fields = append(fields, field)
		return field != 4
	})
	if got, want := len(fields), 2; got != want {
		t.Errorf("walked %d fields past the stop, want %d", got, want)
	}
}

// A body too short to hold the field asked for yields nothing for it, rather
// than a zero the caller would read as a real value.
func TestWalkProcStatOnTruncatedAndMalformedBodies(t *testing.T) {
	cases := map[string]string{
		"no close paren":  "1234 (Web Content S 1",
		"nothing past it": "1234 (Web Content)",
		"field absent":    "1234 (Web Content) S 1 2",
		"empty":           "",
		"separator only":  "1234 (Web Content)) ",
	}
	for name, stat := range cases {
		saw := false
		WalkProcStat(stat, 24, func(field int, _ string) bool {
			if field == 24 {
				saw = true
			}
			return true
		})
		if saw {
			t.Errorf("%s: field 24 was read out of %q", name, stat)
		}
	}
}

// The number the callers actually parse has to survive the walk, or the shared
// helper would hand back a value neither of them can use.
func TestWalkProcStatValuesParseAsNumbers(t *testing.T) {
	var ticks uint64
	var ok bool
	WalkProcStat(procStatSample, 24, func(field int, value string) bool {
		if field != 22 {
			return true
		}
		n, err := strconv.ParseUint(value, 10, 64)
		ticks, ok = n, err == nil
		return false
	})
	if !ok || ticks != 987654 {
		t.Errorf("starttime = %d (parsed %v), want 987654", ticks, ok)
	}
}

// The comm may hold whitespace of its own, and a walk that split the line on
// it would shift every field after it.
func TestWalkProcStatSkipsCommWithSpaces(t *testing.T) {
	stat := "1 (a b c) S " + strings.Repeat("7 ", 20) + "999"
	var utime string
	WalkProcStat(stat, 14, func(field int, value string) bool {
		if field == 14 {
			utime = value
			return false
		}
		return true
	})
	if utime != "7" {
		t.Errorf("utime = %q, want %q", utime, "7")
	}
}
