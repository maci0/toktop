// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package core

import "testing"

// A cut must not strand a ZWJ. U+200D joins the character on either side of
// it, and a base letter with a trailing joiner is one cluster to uniseg while
// the emoji that joiner points at is the next, so the cluster boundary is not
// far enough back to prevent it: capping "a" + ZWJ + emoji at one cluster
// kept the joiner and nothing for it to join. A stranded joiner renders as an
// invisible trailing mark and survives every fold downstream as a character
// no reader can name.
//
// These are also the inputs the whole-cluster prefix and suffix invariants in
// truncate_test.go do not reach, since that string carries a ZWJ only inside
// a sequence no cap ever cuts through.
func TestTruncationNeverStrandsAJoiner(t *testing.T) {
	cases := []struct{ name, in, wantHead, wantTail string }{
		// England: a regional-indicator pair, a joiner, another pair. The
		// joiner is a cluster of its own here, so capping at one cluster kept
		// the first flag and its unpaired joiner.
		{"england flag", "\U0001F1EC\U0001F1E7\u200d\U0001F1EC\U0001F1E7", "\U0001F1EC\U0001F1E7", "\U0001F1EC\U0001F1E7"},
		{"letter then joiner", "a\u200d\U0001F600", "a", "\U0001F600"},
		{"emoji then joiner", "\U0001F600\u200db", "\U0001F600", "b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TruncateClusters(tc.in, 1); got != tc.wantHead {
				t.Errorf("TruncateClusters(%q, 1) = %q, want %q", tc.in, got, tc.wantHead)
			}
			if got := TailClusters(tc.in, 1); got != tc.wantTail {
				t.Errorf("TailClusters(%q, 1) = %q, want %q", tc.in, got, tc.wantTail)
			}
			if got := ClampField(tc.in, 1); got != tc.wantHead {
				t.Errorf("ClampField(%q, 1) = %q, want %q", tc.in, got, tc.wantHead)
			}
		})
	}
}

// The other half of the same guarantee: a string that fits keeps a trailing
// joiner, because the character it joins is inside the cap. Trimming on the
// fast path as well would rewrite a model id or an agent note never cut.
func TestTruncationKeepsAJoinerThatStillHasItsPartner(t *testing.T) {
	const family = "\U0001F468\u200d\U0001F469\u200d\U0001F467"
	for _, n := range []int{1, 2, 3, 8} {
		if got := TruncateClusters(family, n); got != family {
			t.Errorf("TruncateClusters(family, %d) = %q, want the sequence whole", n, got)
		}
		if got := TailClusters(family, n); got != family {
			t.Errorf("TailClusters(family, %d) = %q, want the sequence whole", n, got)
		}
		if got := ClampField(family, n); got != family {
			t.Errorf("ClampField(family, %d) = %q, want the sequence whole", n, got)
		}
	}
}
