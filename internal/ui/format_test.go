package ui

import (
	"math"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// Truncation must respect both limits at once: the visible-cell budget, and
// the character itself. A flag emoji is two regional indicators that render
// as one glyph; slicing between them prints half a flag (a lone indicator
// shows as a boxed placeholder). A ZWJ sequence sliced after the joiner
// prints a dangling U+200D. Both appeared when shorten cut per rune.
func TestShortenKeepsCharactersWhole(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
	}{
		{"flags", "\U0001F1E9\U0001F1EA\U0001F1EB\U0001F1F7", 3},
		{"zwj emoji then text", "👩‍💻 writing code", 4},
		{"combining accents", "cafe\u0301 latte", 5},
		{"mixed emoji and cjk", "你好\U0001F1E9\U0001F1EA世界", 6},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shorten(tc.in, tc.n)
			if w := lipgloss.Width(got); w > tc.n {
				t.Errorf("shorten(%q, %d) = %q, renders %d cells, want <= %d", tc.in, tc.n, got, w, tc.n)
			}
			if !strings.HasSuffix(got, "…") {
				t.Fatalf("shorten(%q, %d) = %q, want an ellipsis suffix", tc.in, tc.n, got)
			}
			// Everything before the ellipsis must be whole clusters of the
			// input: no lone regional indicators, no dangling joiners.
			head := strings.TrimSuffix(got, "…")
			if head != "" && !strings.HasPrefix(tc.in, head) {
				t.Fatalf("shorten(%q, %d) = %q: head is not a prefix of the input", tc.in, tc.n, got)
			}
			if ri := strings.IndexRune(head, 0x1F1E9); ri >= 0 {
				// Any regional indicator present must arrive paired with its
				// flag partner, i.e. as part of a two-rune cluster prefix.
				if !utf16PairsBalanced(head) {
					t.Fatalf("shorten(%q, %d) = %q: flag split mid-character", tc.in, tc.n, got)
				}
			}
			if strings.HasSuffix(head, "\u200d") {
				t.Fatalf("shorten(%q, %d) = %q: trailing zero-width joiner", tc.in, tc.n, got)
			}
		})
	}
}

func utf16PairsBalanced(s string) bool {
	count := 0
	for _, r := range s {
		if r >= 0x1F1E6 && r <= 0x1F1FF {
			count++
		}
	}
	return count%2 == 0
}

// Within budget, shortening must be the identity; the ellipsis only appears
// when something was actually cut.
func TestShortenWithinBudgetUnchanged(t *testing.T) {
	for _, s := range []string{"claude", "你好", "\U0001F1E9\U0001F1EA👩‍💻"} {
		if got := shorten(s, 40); got != s {
			t.Errorf("shorten(%q, 40) = %q, want unchanged", s, got)
		}
	}
}

func TestNorm(t *testing.T) {
	if got := norm(50, 100); got != 0.5 {
		t.Errorf("norm(50, 100) = %v, want 0.5", got)
	}
	if got := norm(150, 100); got != 1.0 {
		t.Errorf("norm(150, 100) = %v, want 1.0", got)
	}
	if got := norm(-10, 100); got != 0 {
		t.Errorf("norm(-10, 100) = %v, want 0", got)
	}
	if got := norm(50, 0); got != 0 {
		t.Errorf("norm(50, 0) = %v, want 0", got)
	}
	if got := norm(math.NaN(), 100); got != 0 {
		t.Errorf("norm(NaN, 100) = %v, want 0", got)
	}
	if got := norm(50, math.NaN()); got != 0 {
		t.Errorf("norm(50, NaN) = %v, want 0", got)
	}
}

// widthOf takes the plainWidth fast path for single-cell runes. When it
// does, the answer is the fast path's own; anything that disagrees
// silently misaligns every panel it touches. A string the fast path
// declines resolves to lipgloss.Width inside widthOf, so comparing the two
// there compares lipgloss with itself and passes for any implementation:
// those rows are the fallback branch, and the count below proves both
// branches were reached.
func TestWidthOfFastPathMatchesLipgloss(t *testing.T) {
	accepted, declined := 0, 0
	for _, s := range []string{
		"",
		"TOKTOP",
		"engine-0 v0.12.0",
		"  spaced  ",
		"\u250c\u2500\u2500\u2510", // box drawing
		"\u2800\u2801\u28ff",       // braille dots
		"cpu \u2588\u2584 42%",     // partial blocks
		"\u2801\u2800 \u2502 x",    // mixed
		"\u4f60\u597d",             // wide: must fall back
		"caf\u00e9",                // combining-capable: must fall back
		"\t tab",                   // control: must fall back
	} {
		pw := plainWidth(s)
		w := widthOf(s)
		if pw >= 0 {
			accepted++
			if w != pw {
				t.Errorf("widthOf(%q) = %d, want the fast path's %d", s, w, pw)
			}
			continue
		}
		declined++
		if want := lipgloss.Width(s); w != want {
			t.Errorf("widthOf(%q) = %d, want the fallback %d", s, w, want)
		}
	}
	if accepted == 0 || declined == 0 {
		t.Fatalf("fast path taken for %d inputs and declined for %d, want both branches exercised", accepted, declined)
	}
}

func TestHumanBytesSubMebiTier(t *testing.T) {
	tests := []struct {
		in   uint64
		want string
	}{
		{0, "0KiB"},
		{1536, "2KiB"}, // used to render "0MiB"
		{1 << 20, "1MiB"},
		{3<<20 + 512<<10, "4MiB"},
		{1 << 30, "1.0GiB"},
	}
	for _, tc := range tests {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHumanBytesShortSubMebiTier(t *testing.T) {
	tests := []struct {
		in   uint64
		want string
	}{
		{0, "0K"},
		{1536, "2K"}, // used to render "0M"
		{1 << 20, "1M"},
		{1 << 30, "1.0G"},
	}
	for _, tc := range tests {
		if got := humanBytesShort(tc.in); got != tc.want {
			t.Errorf("humanBytesShort(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A count or rate just below a unit boundary rounds up on screen, so the
// unit has to be picked on the rounded value: 999999 printed "1000.0k"
// while 1000000 printed "1.0M", and 9999.9 printed "10.0k" while 10000
// printed "10k". Two spellings of one magnitude read as two measurements.
func TestUnitBoundariesDoNotChangeSpelling(t *testing.T) {
	counts := []struct {
		n    int64
		want string
	}{
		{999, "999"},
		{1000, "1.0k"},
		{999949, "999.9k"},
		{999999, "1.0M"},
		{1000000, "1.0M"},
		{12500000, "12.5M"},
	}
	for _, tc := range counts {
		if got := fmtCount(tc.n); got != tc.want {
			t.Errorf("fmtCount(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
	rates := []struct {
		v    float64
		want string
	}{
		{99.94, "99.9"},
		{999.4, "999"},
		{999.5, "1.0k"},
		{9999, "10.0k"},
		{9999.9, "10.0k"},
		{10000, "10k"},
	}
	for _, tc := range rates {
		if got := fmtRate(tc.v); got != tc.want {
			t.Errorf("fmtRate(%v) = %q, want %q", tc.v, got, tc.want)
		}
	}
	// fmtMs carries the same rule: the unit is chosen on the value as
	// rendered, so 999.6 does not print "1000ms" one frame before 1000 prints
	// "1.00s".
	durs := []struct {
		ms   float64
		want string
	}{
		{999.4, "999ms"},
		{999.5, "1.00s"},
		{999.6, "1.00s"},
		{1000, "1.00s"},
	}
	for _, tc := range durs {
		if got := fmtMs(tc.ms); got != tc.want {
			t.Errorf("fmtMs(%v) = %q, want %q", tc.ms, got, tc.want)
		}
	}
}

// widthOf answers from a byte scan whenever it can and defers to lipgloss
// otherwise, so the two must agree on every string a frame can hand them. The
// styled and malformed-escape cases are the ones the fast path has to get
// right: it skips SGR and OSC sequences itself, and a wrong skip is a frame
// whose panels no longer line up.
func TestWidthOfAgreesWithLipgloss(t *testing.T) {
	for _, s := range []string{
		"", "abc", "世界", "éあ", "▲ 1.2k", "│ abc │", "⠁⠂⠄",
		"\x1b[31mred\x1b[0m", "\x1b]0;t\x07x", "\x1b[38;2;1;2;3;mx\x1b[0m",
		"\x1b[38;2;1;2;3m世\x1b[0m", "a\x1b[38;5;167mb\x1b[0mc",
		"\x1b[1;37mT\x1b[0m", "\x1b[0K", "\x1b[?25l", "\x1b[m", "\x1bX",
		// Malformed and unterminated: the fast path must defer, not guess.
		"\x1b[", "\x1b]unterminated", "tab\there", "cafe\u0301 latte",
	} {
		if got, want := widthOf(s), lipgloss.Width(s); got != want {
			t.Errorf("widthOf(%q) = %d, lipgloss.Width = %d", s, got, want)
		}
	}
}

// plainWidth is the fast path's own answer, -1 where it declines. A styled
// string is the common case on the frame path, and it must be answered rather
// than deferred, or the measurement walks grapheme clusters for every panel
// body and chart row.
func TestPlainWidthAnswersStyledStrings(t *testing.T) {
	for _, s := range []string{
		"abc", "世界"[:0] + "│ │", "⠁⠂⠄", "\x1b[31mred\x1b[0m",
		"\x1b[38;2;1;2;3mx\x1b[0m", "\x1b]0;t\x07x", styleOK.Render("ok"),
	} {
		if w := plainWidth(s); w < 0 {
			t.Errorf("plainWidth(%q) declined; the frame path measures this every tick", s)
		} else if w != lipgloss.Width(s) {
			t.Errorf("plainWidth(%q) = %d, lipgloss.Width = %d", s, w, lipgloss.Width(s))
		}
	}
	// Everything outside its model still declines, so widthOf keeps deferring
	// to the library on what the scan cannot answer. The accented "e" below is the
	// combining form: the precomposed U+00E9 is one cell like any other Latin
	// letter, and is counted.
	for _, s := range []string{"世界", "cafe\u0301 latte", "\x1b[", "\x1b]unterminated", "a\tb"} {
		if w := plainWidth(s); w >= 0 {
			t.Errorf("plainWidth(%q) = %d, want a decline", s, w)
		}
	}
}

// joinBlocks replaces lipgloss.JoinVertical on the frame path, so the two must
// produce the same bytes: every row padded to the widest across all blocks,
// with the last block's trailing row kept and no trailing newline.
func TestJoinBlocksMatchesLipgloss(t *testing.T) {
	for _, blocks := range [][]string{
		{"a", "bb", "ccc"},
		{"", "x", ""},
		{"\x1b[31mred\x1b[0m", "plain"},
		{"世界", "a"},
		{"a\nbb", "ccc\ndddd"},
		{""},
		{"only"},
		{"trailing\n"},
		{"a", "trailing\n"},
		{"trailing\n", "b"},
		{styleOK.Render("ok") + " x", "y"},
		{"⠁⠂", "│x│"},
	} {
		want := lipgloss.JoinVertical(lipgloss.Left, blocks...)
		if got := joinBlocks(blocks...); got != want {
			t.Errorf("joinBlocks(%q)\n got %q\nwant %q", blocks, got, want)
		}
	}
}

// joinAcross replaces lipgloss.JoinHorizontal(Top) on the mid-row path: each
// block padded to its own widest row, rows zipped, shorter blocks blank-filled
// downward.
func TestJoinAcrossMatchesLipgloss(t *testing.T) {
	for _, blocks := range [][]string{
		{"a\nbb", "ccc", "d"},
		{"a\nbb\nccc", "x"},
		{"only"},
		{"", "a\nb", ""},
		{"\x1b[31mred\x1b[0m", "plain"},
		{"世界", "a\nb"},
		{"⠁⠂", "│x│\n⠄"},
	} {
		want := lipgloss.JoinHorizontal(lipgloss.Top, blocks...)
		if got := joinAcross(blocks...); got != want {
			t.Errorf("joinAcross(%q)\n got %q\nwant %q", blocks, got, want)
		}
	}
}

// plainWidth counts singleCellRunes one per rune, which is only the same
// answer a grapheme walk gives while no two of them cluster together. Both
// properties are checked here rather than assumed: each code point must be one
// cell on its own, and it must not join with the one before it, or the
// dashboard's own glyphs (box drawing, braille dots, arrows, dashes) would
// measure as a different width than lipgloss reports and the panels would stop
// lining up.
func TestSingleCellRunesAreOneCellAndDoNotCluster(t *testing.T) {
	for _, rg := range singleCellRunes {
		for r := rg[0]; r <= rg[1]; r++ {
			s := string(r)
			if w := lipgloss.Width(s); w != 1 {
				t.Fatalf("U+%04X is %d cells, want 1", r, w)
			}
			if w := lipgloss.Width("a" + s); w != 2 {
				t.Fatalf("U+%04X clusters with a preceding 'a' (%d cells, want 2)", r, w)
			}
			if w := lipgloss.Width(s + "a"); w != 2 {
				t.Fatalf("U+%04X clusters with a following 'a' (%d cells, want 2)", r, w)
			}
		}
	}
}

// A code point outside every range is either wide or zero-width, and both have
// to defer rather than be counted as one cell.
func TestSingleCellRuneRejectsWideAndZeroWidth(t *testing.T) {
	for _, r := range []rune{'世', 0x0301, 0x200D, '🙂', 0x1F1E6, 0x1100, 0xAC00} {
		if singleCellRune(r) {
			t.Errorf("U+%04X counted as a single cell; it is not", r)
		}
	}
}
