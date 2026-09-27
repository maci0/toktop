package core

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
