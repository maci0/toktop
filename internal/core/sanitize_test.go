package core

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeTextPassthrough(t *testing.T) {
	cases := []string{
		"",
		"plain text",
		"model: llama-3.1-8b-instruct.Q4_K_M",
		"shell(git status)",
		"héllo wörld ✓ 日本語 🎉",
		"line one\nline two\ttabbed",
	}
	for _, in := range cases {
		got := SanitizeText(in)
		if got != in {
			t.Errorf("SanitizeText(%q) = %q, want unchanged", in, got)
		}
		if !utf8.ValidString(got) {
			t.Errorf("SanitizeText(%q) returned invalid UTF-8", in)
		}
	}
}

func TestSanitizeTextStripsTerminalInjection(t *testing.T) {
	// want is the exact text, not a "no control byte remains" scan: the
	// encoded C1 forms (U+0085, U+0090, U+009B) are every byte >= 0x80, so
	// a byte-level scan cannot see them and a pass-through implementation
	// would satisfy it.
	cases := []struct{ name, in, want string }{
		{"osc52 clipboard hijack", "\x1b]52;c;YU9UQw==\x07stealth", "stealth"},
		{"osc0 title spoof (ST terminated)", "\x1b]0;evil title\x1b\\x", "x"},
		{"csi cursor move + redraw", "\x1b[2J\x1b[3;5Hfake", "fake"},
		{"sgr recolor", "ok\x1b[31mred\x1b[0m", "okred"},
		// The introducer is dropped; its parameters stay as visible text.
		{"8-bit CSI", "a\xc2\x9b2Jb", "a2Jb"},
		{"carriage return overwrite", "safe\rERASED", "safeERASED"},
		{"backspace", "ab\bX", "abX"},
		{"bel", "ring\a", "ring"},
		{"dcs", "\x1bP1$r\x1b\\after", "after"},
		{"bare esc", "pre\x1b", "pre"},
		// ESC plus one non-introducer byte: the only input that reaches
		// skipEscapeSeq's two-byte default arm.
		{"two-byte esc", "pre\x1bZpost", "prepost"},
		{"esc mid-sequence truncation", "\x1b]52;c;no terminator at all", ""},
		{"c1 others", "a\xc2\x85b\xc2\x90c", "abc"},
		{"nul and controls", "a\x00b\x01c\x1fd", "abcd"},
		{"del", "a\x7fb", "ab"},
	}
	for _, tc := range cases {
		got := SanitizeText(tc.in)
		if got != tc.want {
			t.Errorf("%s: SanitizeText(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
		for i := 0; i < len(got); i++ {
			c := got[i]
			if (c < 0x20 && c != '\n' && c != '\t') || c == 0x7f || c == 0x1b {
				t.Errorf("%s: SanitizeText(%q) = %q still contains control byte %#x", tc.name, tc.in, got, c)
			}
		}
	}
}

func TestSanitizeTextKeepsVisibleTailAfterStrippedPayload(t *testing.T) {
	if got := SanitizeText("\x1b]52;c;QUJD\x07visible"); !strings.HasSuffix(got, "visible") || got != "visible" {
		t.Errorf("OSC payload leaked or tail lost: %q", got)
	}
	if got := SanitizeText("\x1b[31mred\x1b[0mclean"); got != "redclean" {
		t.Errorf("CSI handling broke visible text: %q", got)
	}
}

func TestSanitizeTextPreservesUTF8(t *testing.T) {
	in := "café \x1b[31m rouge"
	got := SanitizeText(in)
	want := "café  rouge" // the SGR sequence vanishes, é survives
	if got != want {
		t.Errorf("SanitizeText(%q) = %q, want %q", in, got, want)
	}
	if !strings.Contains(SanitizeText("日本語モデル"), "日本語モデル") {
		t.Error("multibyte text must survive sanitization")
	}
}

func TestSanitizeTextStripsRawC1Byte(t *testing.T) {
	got := SanitizeText("a\x9bb")
	if i := strings.IndexByte(got, 0x9b); i >= 0 {
		t.Fatalf("raw C1 CSI byte survived at %d: %q", i, got)
	}
	if got != "ab" {
		t.Fatalf("SanitizeText(%q) = %q, want %q", "a\x9bb", got, "ab")
	}
}

func TestSanitizeTextDropsIllFormedUTF8(t *testing.T) {
	// A lead byte plus a later continuation must not reassemble into U+00AD
	// (soft hyphen) on a second pass.
	if got := SanitizeText("\xc2\v\xad"); got != "" {
		t.Fatalf("ill-formed UTF-8 survived: %q", got)
	}
	if got := SanitizeText("\xc2\xad"); got != "" {
		t.Fatalf("UTF-8 soft hyphen survived: %q", got)
	}
}

func TestSanitizeTextNewlineTabSurvive(t *testing.T) {
	if got := SanitizeText("a\nb\tc\rd"); got != "a\nb\tcd" {
		t.Errorf("newline/tab handling changed: %q", got)
	}
}

func TestSanitizeTextStripsBidiAndZeroWidth(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"RLO spoofs claude", "\u202eedualc", "edualc"},
		{"LRM/RLM marks", "cla\u200eude\u200f", "claude"},
		{"isolate wrap", "\u2066claude\u2069", "claude"},
		{"zero-width space in name", "clau\u200bde", "claude"},
		{"BOM prefix", "\ufeffclaude", "claude"},
		{"soft hyphen", "cla\u00adude", "claude"},
		{"ZWJ emoji stays one sequence", "👩\u200d💻", "👩\u200d💻"},
		{"VS1 cannot spoof a name", "clau\ufe00de", "claude"},
		{"emoji VS16 stripped, base stays", "\u2764\ufe0f", "\u2764"},
		{"line separator cannot split a row", "foo\u2028bar", "foobar"},
		{"paragraph separator", "foo\u2029bar", "foobar"},
		// Tag characters are invisible Cf; a TAG LATIN SMALL LETTER D
		// inside a name must not create a second agent that looks like claude.
		{"tag char cannot hide in a name", "clau\U000E0064e", "claue"},
		{"language tag prefix", "\U000E0001claude", "claude"},
		{"tag space in a name", "clau\U000E0020de", "claude"},
	}
	for _, tc := range cases {
		if got := SanitizeText(tc.in); got != tc.want {
			t.Errorf("%s: SanitizeText(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestMixedScriptIdentity(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"ascii agent", "claude", false},
		{"hyphenated ascii", "prime-agent", false},
		{"cyrillic-only", "модель", false},
		{"japanese-only", "日本語", false},
		{"cyrillic s in claude", "сlaude", true}, // U+0441 CYRILLIC SMALL LETTER ES
		{"greek o in codex", "cοdex", true},      // U+03BF GREEK SMALL LETTER OMICRON
		{"digits and hyphen", "gpt-4", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		if got := MixedScriptIdentity(tc.in); got != tc.want {
			t.Errorf("%s: MixedScriptIdentity(%q) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestSanitizeTextDropsInvalidUTF8(t *testing.T) {
	// A truncated 2-byte sequence and a lone 0xFF are not UTF-8; leaving
	// them in an identity field or a terminal line is the same class of
	// defect as slicing a character in half. Dropping them keeps the
	// result valid UTF-8 without growing the string.
	got := SanitizeText("caf\xff\xfe")
	if !utf8.ValidString(got) {
		t.Errorf("SanitizeText returned invalid UTF-8: %q", got)
	}
	if got != "caf" {
		t.Errorf("SanitizeText(%q) = %q, want caf with the ill-formed bytes dropped", "caf\xff\xfe", got)
	}
	// A real U+FFFD (3-byte UTF-8) is already valid and stays.
	if got := SanitizeText("ok\uFFFD"); got != "ok\uFFFD" {
		t.Errorf("literal replacement character was altered: %q", got)
	}
}

// A field rendered into one cell must stay one cell: the renderers split a
// row on newlines into frame rows, so a newline in an untrusted name becomes
// a line the dashboard reads as its own output.
func TestSingleLineFoldsNewlinesAndBlanks(t *testing.T) {
	cases := []struct{ in, want string }{
		{"claude", "claude"},
		{"clau\nde", "clau de"},
		{"clau\r\nde", "clau de"},
		{"a\tb", "a b"},
		{"\n\n", ""},
		{"up 9/9 engines\nall clear", "up 9/9 engines all clear"},
		{"\x1b[31mred\x1b[0m", "red"},
	}
	for _, tc := range cases {
		if got := SingleLine(tc.in); got != tc.want {
			t.Errorf("SingleLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
