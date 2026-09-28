package core

import (
	"unicode"
	"unicode/utf8"
)

// FoldASCII lowercases the ASCII letters A-Z in s and leaves every other byte
// alone. It is the fold for matching text whose names are ASCII by
// construction: JSON keys, /proc and CLI field names, unit suffixes, DNS host
// labels.
//
// strings.ToLower is the wrong fold there. It also folds runes whose
// lowercase form is ASCII: U+0130 (LATIN CAPITAL LETTER I WITH DOT ABOVE)
// becomes "i" plus U+0307, and U+212A (KELVIN SIGN) becomes "k". A key or
// unit spelled with either would satisfy a match its producer never wrote,
// which for a sensor key is a value the operator did not measure.
//
// s is returned unchanged when it holds no uppercase ASCII, so the common
// already-folded case allocates nothing.
func FoldASCII(s string) string {
	up := -1
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			up = i
			break
		}
	}
	if up < 0 {
		return s
	}
	b := []byte(s)
	for i := up; i < len(b); i++ {
		if c := b[i]; c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// FoldCase maps every rune of s to the canonical representative of its simple
// case folding orbit, the fold strings.EqualFold compares by. It is the fold
// for a key a case-insensitive file system will equate: a directory name on
// macOS or Windows, where two spellings of one directory have to produce one
// map entry.
//
// strings.ToLower is the wrong fold there too, and the failure runs the other
// way from FoldASCII's: it is a full case mapping rather than the simple fold
// a file system performs, so U+0130 (LATIN CAPITAL LETTER I WITH DOT ABOVE)
// comes back as a bare "i" and the two directories "/Users/i/proj" and
// "/Users/İ/proj", which the volume keeps apart, land on one key. The session
// recorded in one of them then shares an entry with the other and is watched
// as a directory that does not exist, or watched twice, depending on which
// one the store named first. Simple folding keeps them apart: U+0130's orbit
// is itself, while "i" and "I" share one.
//
// Two runes are fold-equivalent exactly when they share an orbit, and
// distinct orbits are disjoint, so mapping to a representative is a fold with
// no collisions: two strings FoldCase renders equal are exactly the two
// strings EqualFold calls equal. s is returned unchanged when it is ASCII
// with no uppercase in it, so the common case allocates nothing.
func FoldCase(s string) string {
	up, wide := -1, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= utf8.RuneSelf {
			wide = true
			break
		}
		if c >= 'A' && c <= 'Z' {
			up = i
			break
		}
	}
	if !wide {
		if up < 0 {
			return s
		}
		b := []byte(s)
		for i := up; i < len(b); i++ {
			if c := b[i]; c >= 'A' && c <= 'Z' {
				b[i] = c + ('a' - 'A')
			}
		}
		return string(b)
	}
	b := make([]byte, 0, len(s))
	for _, r := range s {
		b = utf8.AppendRune(b, foldRune(r))
	}
	return string(b)
}

// foldRune is the per-rune step of FoldCase: the smallest rune of r's simple
// case folding orbit, lowercased when that is ASCII. The orbit is the cycle
// unicode.SimpleFold walks, so its minimum is a representative shared by
// every rune equivalent to r under EqualFold.
func foldRune(r rune) rune {
	if r < utf8.RuneSelf {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}
	least := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if f < least {
			least = f
		}
	}
	if least >= 'A' && least <= 'Z' {
		return least + ('a' - 'A')
	}
	return least
}
