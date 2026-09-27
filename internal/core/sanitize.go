// Sanitization for untrusted text rendered into a terminal: engine-supplied
// model names and versions, agent-event fields, SSH-sourced host vitals.
// Escape sequences and control characters in such strings would otherwise be
// interpreted by the operator's terminal (clipboard hijack via OSC 52, cursor
// redraw via CSI, title spoofing), the terminal equivalent of XSS.

package core

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// SanitizeText strips ANSI/ECMA-48 escape sequences (CSI, OSC, DCS, SOS, PM,
// APC and two-byte forms) plus C0 and C1 control characters from s. Newlines
// and tabs survive so layout text is unaffected. Bidi overrides, isolates,
// and zero-width format characters that spoof identity-bearing fields
// (agent names, host labels) are removed too. ZWJ (U+200D) stays so emoji
// sequences remain whole. Ill-formed UTF-8 is dropped so the result is
// always valid UTF-8 and never longer than the input. Other multi-byte
// runes are preserved.
func SanitizeText(s string) string {
	if !needsSanitize(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == 0x1b:
			i = skipEscapeSeq(s, i+1)
		case c == '\n' || c == '\t':
			b.WriteByte(c)
			i++
		case c < 0x20 || c == 0x7f:
			i++
		case c < 0x80:
			b.WriteByte(c)
			i++
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				i++
				continue
			}
			if unsafeRune(r) {
				i += size
				continue
			}
			b.WriteString(s[i : i+size])
			i += size
		}
	}
	return b.String()
}

// unsafeRune reports a code point that must not reach a terminal or an
// identity field: C1 controls, bidi overrides/isolates, and format
// characters used to spoof or hide names. U+200D (ZWJ) is kept so
// 👩‍💻-style emoji survive as one cluster. Variation selectors are stripped
// so "claude" and "claude" + VS1 stay one name; emoji remain as their base
// characters. Line and paragraph separators would split a single-line field.
// Tag characters (U+E0001, U+E0020-U+E007F) are Cf and come out here too:
// "clau" + TAG LATIN SMALL LETTER D + "e" must not be a distinct agent.
func unsafeRune(r rune) bool {
	if r >= 0x80 && r <= 0x9f {
		return true
	}
	if unicode.Is(unicode.Bidi_Control, r) {
		return true
	}
	if unicode.Is(unicode.Cf, r) && r != 0x200D {
		return true
	}
	switch r {
	case 0x034F, // combining grapheme joiner
		0x180B, 0x180C, 0x180D, 0x180F, // mongolian free variation selectors
		0x2028, 0x2029: // line/paragraph separators
		return true
	}
	if r >= 0xFE00 && r <= 0xFE0F {
		return true // variation selectors 1-16
	}
	if r >= 0xE0100 && r <= 0xE01EF {
		return true // variation selectors supplement
	}
	return false
}

// MixedScriptIdentity reports a name that mixes Latin letters with Cyrillic
// or Greek. Those alphabets supply lookalikes for Latin (Cyrillic с vs c),
// so "сlaude" would render next to a real "claude" as the same agent.
// Homoglyphs inside one script are left alone; this is the mixed-script
// impersonation that ingest and similar identity fields can actually use.
func MixedScriptIdentity(s string) bool {
	var latin, lookalike bool
	for _, r := range s {
		switch {
		case r <= unicode.MaxASCII && unicode.IsLetter(r):
			latin = true
		case unicode.Is(unicode.Cyrillic, r) || unicode.Is(unicode.Greek, r):
			lookalike = true
		}
		if latin && lookalike {
			return true
		}
	}
	return false
}

// needsSanitize reports whether s contains any byte that SanitizeText would
// remove. ASCII controls (except newline/tab) and DEL are one class; any
// non-ASCII byte takes the slow path, because bidi controls and zero-width
// marks run past the 0xC2-0xC3 range.
func needsSanitize(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && c != '\n' && c != '\t') || c == 0x7f || c >= 0x80 {
			return true
		}
	}
	return false
}

// skipEscapeSeq consumes one complete escape sequence starting just after the
// ESC byte and returns the index of the first byte past it. Unterminated
// sequences swallow the rest of the string: fail closed rather than emit
// half a payload as visible text.
func skipEscapeSeq(s string, i int) int {
	if i >= len(s) {
		return i
	}
	switch s[i] {
	case '[': // CSI: parameter bytes 0x30-0x3F, intermediates 0x20-0x2F, final 0x40-0x7E
		i++
		for i < len(s) && s[i] >= 0x20 && s[i] <= 0x3f {
			i++
		}
		if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7e {
			i++
		}
	case ']', 'P', 'X', '^', '_': // OSC/DCS/SOS/PM/APC: terminated by BEL or ST
		i++
		for i < len(s) {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
			i++
		}
	default: // two-byte escape: ESC plus one byte
		i++
	}
	return i
}
