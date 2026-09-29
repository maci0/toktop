// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build linux

package core

import (
	"slices"
	"strings"
	"testing"
	"unicode"
)

// FuzzWalkProcStat drives the /proc/PID/stat field walker every /proc reader in
// this tree shares with arbitrary bytes. The comm field is the kernel's own
// name for the process, up to 16 bytes and free to hold spaces, tabs and
// parentheses, so a walk that miscounts the fields past it hands every caller
// a plausible wrong number rather than an error: the process table reads
// another process's utime and RSS, and agent discovery reads a start time
// that either never matches or, worse, matches a later process that reused
// the pid. The field numbering, the value boundaries, the early stop and the
// agreement with a whitespace split of a kernel-shaped body are all asserted.
func FuzzWalkProcStat(f *testing.F) {
	for _, seed := range []string{
		procStatSample,
		"1 (a) S 1 2 3",
		"1 (a)  S 1 2 3",
		"1 (a)\tS 1 2 3",
		"1 (a)",
		"1 (a) ",
		"1 (a)) S 1",
		") S 1 2 3",
		"",
		")",
		"1 (unterminated S 1 2",
		"1 (a) S 1 2 3\n",
		"1 (a) \nS 1 2 3",
		strings.Repeat("9 ", 60) + "5",
		"1 () S",
		"1 (() ) S 1",
		"1 (a) S\t1\t2\t3",
		"1 (a) S  ",
		"1 (a) -1 0x10 1e9",
	} {
		f.Add(seed, 24)
	}
	for _, last := range []int{0, 1, 3, 5, 22, 24, 50, 1000} {
		f.Add(procStatSample, last)
	}

	f.Fuzz(func(t *testing.T, stat string, last int) {
		// A caller names a field it wants; anything below the first one past
		// the comm reads nothing at all, and a runaway bound must not walk
		// forever.
		var got []string
		WalkProcStat(stat, last, func(field int, value string) bool {
			if field < 3 || field > last {
				t.Fatalf("field %d reported for last=%d on %q", field, last, stat)
			}
			if want := 3 + len(got); field != want {
				t.Fatalf("field %d reported where %d came next on %q", field, want, stat)
			}
			if strings.ContainsAny(value, " \t\n") {
				t.Fatalf("field %d value %q spans a separator on %q", field, value, stat)
			}
			got = append(got, value)
			return true
		})
		if avail := max(last-2, 0); len(got) > avail {
			t.Fatalf("walked %d fields, past the %d available for last=%d on %q", len(got), avail, last, stat)
		}

		// A body the kernel would write separates fields with spaces and holds
		// no other whitespace, and the fields past the comm are then exactly
		// the words after its closing paren. This is the differential that
		// catches a shifted walk: every field the caller asked for has to be
		// the split's field of the same number, and the walk has to reach as
		// far as the body does. A body carrying some other whitespace is left
		// to the invariants above, since the walk and a whitespace split are
		// not obliged to agree about where a field ends there.
		if closeP := strings.LastIndexByte(stat, ')'); closeP >= 0 && onlySpaceTab(stat) {
			want := strings.Fields(stat[min(closeP+2, len(stat)):])
			if avail := max(last-2, 0); len(got) != min(len(want), avail) {
				t.Fatalf("walked %d fields, want %d of %d for last=%d on %q", len(got), min(len(want), avail), len(want), last, stat)
			}
			if !slices.Equal(got, want[:len(got)]) {
				t.Fatalf("walk gave %q, split gives %q for last=%d on %q", got, want, last, stat)
			}
		}

		// A caller that reports it has what it came for ends the walk there.
		stopped := 0
		WalkProcStat(stat, last, func(field int, _ string) bool {
			stopped++
			return field != 4
		})
		if want := min(len(got), 2); stopped != want {
			t.Fatalf("early stop walked %d fields, want %d on %q", stopped, want, stat)
		}
	})
}

// onlySpaceTab reports whether every whitespace character in s is a plain
// space or a tab, which is all the kernel writes between stat fields and all
// the walk treats as a separator.
func onlySpaceTab(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) && r != ' ' && r != '\t'
	})
}
