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
// The rewrite folds case and Unicode normalization on the platforms whose
// file systems look names up that way: there the same directory reaches the
// message spelled the way the process that named it wrote it, which is not
// always how UserHomeDir spells it ("C:\Users\me" against "c:\users\me", a
// macOS home stored decomposed against the composed spelling in argv).
// Everywhere else two spellings really are two directories, so folding one
// into the other would hide the path that matters.
func RedactHome(msg string) string {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return msg
	}
	home = filepath.Clean(home)
	if filepath.Dir(home) == home {
		return msg // a filesystem root would swallow every absolute path
	}
	folded := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	// Windows names one directory with either separator, and a path reaching
	// a message can carry either: a user-supplied argument, an ssh target, or
	// a tool built for another platform all spell it with '/'. Matching only
	// the platform separator leaves the account name in the message, so the
	// other spelling is folded with its own separator. On the platforms that
	// have one separator this is the home itself.
	spellings := []string{home}
	if slash := filepath.ToSlash(home); slash != home {
		spellings = append(spellings, slash)
	}
	bare := make([]string, 0, len(spellings))
	for i, spelling := range spellings {
		sep := string(filepath.Separator)
		if i > 0 {
			sep = "/"
		}
		from, to := spelling+sep, "~"+sep
		if folded {
			// The file systems behind the folding platforms also look names
			// up normalization-insensitively, so the comparison has to be made
			// there too: a home directory macOS stored decomposed ("réne" as
			// "e" plus U+0301) is the same account as the composed spelling a
			// process carries, and comparing bytes leaves the account name in
			// the message.
			msg, from = normalizeSpelling(msg), normalizeSpelling(from)
			msg = replaceFold(msg, from, to)
		} else {
			msg = strings.ReplaceAll(msg, from, to)
		}
		bare = append(bare, normalizeSpelling(spelling))
	}
	// A message that is exactly the home directory carries the same account
	// name as a path under it, and the separator-terminated match above leaves
	// it untouched. A longer message ending in the home path is not rewritten.
	// The comparison folds too: the path above does, so on those platforms
	// the same directory spelled in another case is still the home directory.
	for _, spell := range bare {
		if msg == spell || (folded && strings.EqualFold(msg, spell)) {
			return "~"
		}
	}
	return msg
}

// userHomePrefixes are the directories a home sits under on the systems a
// remote login can land on. Which one applies is not knowable from here, so
// each is tried: a path that does not exist on the peer costs nothing.
var userHomePrefixes = []string{"/home/", "/Users/", `\Users\`}

// RedactUserHome rewrites the home directory of the named account in msg to
// "~". RedactHome only folds the home of the account toktop runs as, and the
// text a remote host produces names the peer's account instead: a failing
// vitals script reports "/home/<user>/.bashrc: No such file" from the far
// side, and that line is quoted into the frame, into the --json report and
// into the audit log, all of which outlive the run and get pasted into
// issues.
//
// Case and Unicode normalization are folded whatever the local platform does
// with names, because the peer's platform is not the local one: the account
// being hidden is the same account spelled a case away, and a fold that
// missed it would leave the name in the text it exists to remove.
func RedactUserHome(user, msg string) string {
	// A user is one path component. A name carrying a separator or a volume
	// would fold text the account never named, and the empty name is not an
	// account at all.
	if user == "" || msg == "" || user == "." || user == ".." {
		return msg
	}
	if strings.ContainsAny(user, `/\:`) {
		return msg
	}
	spelled := normalizeSpelling(user)
	scan := normalizeSpelling(msg)
	for _, prefix := range userHomePrefixes {
		scan = foldUserHomePrefix(scan, prefix+spelled)
	}
	return scan
}

// foldUserHomePrefix rewrites every occurrence of home in msg that the
// account ends, leaving the ones it only starts: "/home/me" hides
// "/home/mem" and "/home/me-too", which are other accounts.
func foldUserHomePrefix(msg, home string) string {
	var b strings.Builder
	for {
		at, n, ok := indexFold(msg, home)
		if !ok {
			b.WriteString(msg)
			return b.String()
		}
		if nameContinues(msg[at+n:]) {
			// Another account's name begins here. Copy the matched bytes
			// rather than the pattern, which folds to them and need not be
			// spelled the same way, and resume after them so the search does
			// not stall on the same prefix. Everything up to the match is
			// copied whole: the drive letter is only pulled along when the
			// "~" below replaces the match, and writing msg[:at] here is what
			// keeps "C:\Users\me-too" from losing its "C:" to a match that
			// is going to be copied back verbatim anyway.
			b.WriteString(msg[:at])
			b.WriteString(msg[at : at+n])
			msg = msg[at+n:]
			continue
		}
		b.WriteString(msg[:volumeStart(msg, at)])
		b.WriteByte('~')
		msg = msg[at+n:]
	}
}

// volumeStart returns where the drive letter of a Windows path beginning at
// at starts, or at itself when there is none. A home under the drive reads
// "C:\Users\me", and leaving the volume behind would fold it to "C:~", which
// names a directory rather than the account that owns it.
func volumeStart(msg string, at int) int {
	if at >= 2 && msg[at-1] == ':' && isDriveLetter(msg[at-2]) {
		return at - 2
	}
	return at
}

func isDriveLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// nameContinues reports whether the text after a matched account name extends
// it into a different name. A path separator or the end of the text end the
// name, and so does a drive letter: "\Users\meC:\Users\me" names the home
// twice, and reading that "C" as the start of a longer account name leaves the
// first mention in the text this exists to remove. Letters, digits and the
// separators a shell or a file system puts inside one do not.
func nameContinues(rest string) bool {
	if rest == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(rest)
	if r == '/' || r == '\\' {
		return false
	}
	if r < utf8.RuneSelf && isDriveLetter(byte(r)) && len(rest) > 1 && rest[1] == ':' {
		return false
	}
	return r == '.' || r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// replaceFold replaces every case-insensitive occurrence of old with new,
// leaving the matched text's own spelling to the caller. Runes are compared
// one at a time because a folded rune is not always as wide as the one it
// folds from, so searching lowercased copies of the two strings would land on
// the wrong offset. The resume offset is the match's own length for the same
// reason: advancing by len(old) skips past the text the match actually spanned.
func replaceFold(s, old, new string) string {
	var b strings.Builder
	for {
		at, n, ok := indexFold(s, old)
		if !ok {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:at])
		b.WriteString(new)
		s = s[at+n:]
	}
}

// indexFold returns where the first case-insensitive occurrence of sub begins
// in s and how many bytes it spans there, and whether it found one. The span
// is the one the occurrence occupies in s, which is not len(sub): the two
// spellings fold together but need not be the same width in bytes (U+212A
// KELVIN SIGN against "k"). Resuming by len(sub) would step past the text the
// match covered, splitting the wide rune or running off the end of s.
func indexFold(s, sub string) (at, n int, ok bool) {
	for i := 0; i < len(s); {
		if n := prefixFoldLen(s[i:], sub); n > 0 {
			return i, n, true
		}
		_, w := utf8.DecodeRuneInString(s[i:])
		i += w
	}
	return 0, 0, false
}

// prefixFoldLen returns the byte length of the case-insensitive match for
// prefix at the start of s, or 0 when s does not begin with it. A match
// always spans at least one byte, so 0 is unambiguous.
func prefixFoldLen(s, prefix string) int {
	i := 0
	for _, want := range prefix {
		if i == len(s) {
			return 0
		}
		got, w := utf8.DecodeRuneInString(s[i:])
		if !equalFoldRune(got, want) {
			return 0
		}
		i += w
	}
	return i
}

// equalFoldRune reports whether a and b are the same letter under simple case
// folding, the rule strings.EqualFold applies and the one the file systems
// behind the platforms that fold case use to match names. unicode.ToLower is
// a different mapping and disagrees with EqualFold on exactly the runes that
// matter here: ToLower leaves U+017F alone and turns "S" into "s", so a home
// spelled with a long s survives the prefix rewrite while RedactHome's own
// bare-home comparison, which calls EqualFold, recognizes it.
func equalFoldRune(a, b rune) bool {
	if a == b {
		return true
	}
	for f := unicode.SimpleFold(a); f != a; f = unicode.SimpleFold(f) {
		if f == b {
			return true
		}
	}
	return false
}
