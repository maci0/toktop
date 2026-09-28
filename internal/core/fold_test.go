package core

import (
	"strings"
	"testing"
)

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

func TestFoldCase(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Users/Foo", "users/foo"},
		{"", ""},
		// A file system folds case and nothing more: every spelling of one
		// name reaches the same key, and the ASCII case is the ordinary one.
		{"\u017Fs", "ss"},
		{"SS", "ss"},
		{"\u212a", "k"},
		// Runes that equalFold keeps apart stay apart. U+0130's fold orbit is
		// itself, so "i" and "İ" are two directory names on a case-insensitive
		// volume and must not share a key; strings.ToLower renders them as one.
		{"i", "i"},
		{"\u0130", "\u0130"},
		{"\u0131", "\u0131"},
		{"Users/\u0130", "users/\u0130"},
		// A rune outside every orbit is itself.
		{"\u00c9", "\u00c9"},
	}
	for _, c := range cases {
		if got := FoldCase(c.in); got != c.want {
			t.Errorf("FoldCase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestFoldCaseAgreesWithEqualFold pins the property dirKey relies on: two
// spellings are one key exactly when strings.EqualFold calls them one. A fold
// that merges more (or fewer) than EqualFold either drops a checkout or holds
// two entries for one directory, so the agreement is the whole contract
// rather than any individual table entry.
func TestFoldCaseAgreesWithEqualFold(t *testing.T) {
	names := []string{
		"i", "I", "\u0130", "\u0131", "k", "K", "\u212a", "s", "S", "\u017f",
		"\u00e9", "\u00c9", "\u1e9b", "a", "A", "ss", "sS", "Ss",
	}
	for _, a := range names {
		for _, b := range names {
			if got, want := FoldCase(a) == FoldCase(b), strings.EqualFold(a, b); got != want {
				t.Errorf("FoldCase(%q)==FoldCase(%q) is %v, EqualFold says %v", a, b, got, want)
			}
		}
	}
}

func TestFoldCaseNoAllocWhenFolded(t *testing.T) {
	const already = "home/user/project"
	if got := FoldCase(already); got != already {
		t.Fatalf("FoldCase(%q) = %q, want it unchanged", already, got)
	}
	if allocs := testing.AllocsPerRun(100, func() { FoldCase(already) }); allocs != 0 {
		t.Errorf("FoldCase on an already-folded path allocated %.1f times, want 0", allocs)
	}
}
