// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import (
	"testing"
	"time"
)

func TestAge(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	tests := []struct {
		name string
		then time.Time
		want time.Duration
	}{
		{"later stamp", base, 0},
		{"a minute back", base.Add(-time.Minute), time.Minute},
		{"an hour back", base.Add(-time.Hour), time.Hour},
		{"stepped backwards", base.Add(5 * time.Minute), 0},
		{"unstamped", time.Time{}, base.Sub(time.Time{})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Age(base, tc.then); got != tc.want {
				t.Fatalf("Age(%v, %v) = %v, want %v", base, tc.then, got, tc.want)
			}
		})
	}
}

// A window younger than its own expiry must survive a clock that reads behind
// the stamp: the whole point of the floor is that "how long ago" never goes
// negative and so never satisfies "still fresh" forever.
func TestAgeKeepsABackwardStepFromReadingAsFresh(t *testing.T) {
	stamp := time.Unix(1_700_000_000, 0).UTC()
	stepped := stamp.Add(-time.Second)
	const window = 30 * time.Second
	if Age(stepped, stamp) >= window {
		t.Fatal("a step backwards left the entry inside its window")
	}
	if Age(stamp.Add(window), stamp) < window {
		t.Fatal("a stamp exactly window old fell out of its window")
	}
}
