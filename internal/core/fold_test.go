package core

import "testing"

func TestFoldASCII(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Temperature", "temperature"},
		{"used memory", "used memory"},
		{"", ""},
		// Runes whose lowercase form is ASCII must not move: a key spelled
		// with either would satisfy a match its producer never wrote.
		{"\u212aelvin", "\u212aelvin"},
		{"\u212a", "\u212a"},
		{"\u0130nput", "\u0130nput"},
		{"\u01C5", "\u01C5"},
		// A multi-byte rune that is not ASCII-uppercase is untouched.
		{"\u00c9", "\u00c9"},
		{"V\u00d6L", "v\u00d6l"},
	}
	for _, c := range cases {
		if got := FoldASCII(c.in); got != c.want {
			t.Errorf("FoldASCII(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFoldASCIINoAllocWhenFolded(t *testing.T) {
	const already = "gpu_utilization"
	if got := FoldASCII(already); got != already {
		t.Fatalf("FoldASCII(%q) = %q, want it unchanged", already, got)
	}
	if allocs := testing.AllocsPerRun(100, func() { FoldASCII(already) }); allocs != 0 {
		t.Errorf("FoldASCII on an already-folded key allocated %.1f times, want 0", allocs)
	}
}
