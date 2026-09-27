package core

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"
)

// RedactHome rewrites the home directory in msg to "~". An absolute path
// under $HOME names the account that owns it, and toktop's diagnostics are
// copied into issues and bug reports; the path is what makes them
// unpostable, not the file name alone.
//
// The rewrite folds case on the platforms whose file systems look names up
// that way: there the same directory reaches the message spelled the way the
// process that named it wrote it, which is not always how UserHomeDir spells
// it ("C:\Users\me" against "c:\users\me"). Everywhere else two spellings
// really are two directories, so folding one into the other would hide the
// path that matters.
func RedactHome(msg string) string {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return msg
	}
	home = filepath.Clean(home)
	if filepath.Dir(home) == home {
		return msg // a filesystem root would swallow every absolute path
	}
	sep := string(filepath.Separator)
	from, to := home+sep, "~"+sep
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return replaceFold(msg, from, to)
	}
	return strings.ReplaceAll(msg, from, to)
}

// replaceFold replaces every case-insensitive occurrence of old with new,
// leaving the matched text's own spelling to the caller. Runes are compared
// one at a time because a folded rune is not always as wide as the one it
// folds from, so searching lowercased copies of the two strings would land on
// the wrong offset.
func replaceFold(s, old, new string) string {
	var b strings.Builder
	for {
		i := indexFold(s, old)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString(new)
		s = s[i+len(old):]
	}
}

// indexFold returns the byte offset of the first case-insensitive occurrence
// of sub in s, or -1.
func indexFold(s, sub string) int {
	for i := 0; i < len(s); {
		if hasPrefixFold(s[i:], sub) {
			return i
		}
		_, w := utf8.DecodeRuneInString(s[i:])
		i += w
	}
	return -1
}

// hasPrefixFold reports whether s begins with prefix, compared without regard
// to case. Simple case folding, the same rule strings.EqualFold applies.
func hasPrefixFold(s, prefix string) bool {
	for _, want := range prefix {
		if s == "" {
			return false
		}
		got, w := utf8.DecodeRuneInString(s)
		s = s[w:]
		if got != want && unicode.ToLower(got) != unicode.ToLower(want) {
			return false
		}
	}
	return true
}
