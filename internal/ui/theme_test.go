package ui

import (
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"

	"github.com/maci0/toktop/internal/core"
	"github.com/muesli/termenv"
)

// contrastRatio is the WCAG contrast ratio between two colors; ok is false
// when either side is not #rrggbb (see relLuminance).
func contrastRatio(a, b lipgloss.Color) (ratio float64, ok bool) {
	la, oka := relLuminance(a)
	lb, okb := relLuminance(b)
	if !oka || !okb {
		return 0, false
	}
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05), true
}

// contrastRatioT is contrastRatio with a fatal on non-hex input.
func contrastRatioT(t testing.TB, a, b lipgloss.Color) float64 {
	t.Helper()
	r, ok := contrastRatio(a, b)
	if !ok {
		t.Fatalf("contrast of %q vs %q undefined: not #rrggbb", a, b)
	}
	return r
}

// Secondary text rides on the base background everywhere: footer key hints,
// help rows, gauge tracks, unit labels. WCAG 1.4.3 needs 4.5:1 there; the
// palette must not drift back under it.
func TestDimTextMeetsAAContrastOnBase(t *testing.T) {
	if got := contrastRatioT(t, cDim, cBase); got < 4.5 {
		t.Errorf("cDim on cBase = %.2f:1, want >= 4.5:1", got)
	}
}

// Body text and the status colors carry primary values; keep them comfortably
// above AA too so future palette edits fail loudly here rather than on screen.
func TestStatusTextMeetsAAContrastOnBase(t *testing.T) {
	for name, c := range map[string]lipgloss.Color{
		"cText":   cText,
		"cGreen":  cGreen,
		"cYellow": cYellow,
		"cRed":    cRed,
		"cCyan":   cCyan,
		"cBlue":   cBlue,
	} {
		if got := contrastRatioT(t, c, cBase); got < 4.5 {
			t.Errorf("%s on cBase = %.2f:1, want >= 4.5:1", name, got)
		}
	}
}

// The panel frame is a UI component boundary, so it answers to the non-text
// floor rather than the text one (WCAG 1.4.11). It is the only cue for where
// one panel ends and the next begins: the two chart panels stack with no blank
// row between them, so a border that fails here leaves a low-vision reader one
// unbroken column of glyphs. cBorder was #4a5563, 2.50:1.
func TestPanelBorderMeetsNonTextContrastOnBase(t *testing.T) {
	if got := contrastRatioT(t, cBorder, cBase); got < minGraphicContrast {
		t.Errorf("cBorder on cBase = %.2f:1, want >= %.1f:1", got, minGraphicContrast)
	}
}

// The frame has to stay quieter than the secondary text it encloses, or every
// panel reads as a highlighted block and the text hierarchy inverts.
func TestPanelBorderStaysBelowSecondaryText(t *testing.T) {
	border := contrastRatioT(t, cBorder, cBase)
	dim := contrastRatioT(t, cDim, cBase)
	if border >= dim {
		t.Errorf("cBorder on cBase = %.2f:1, cDim = %.2f:1; the frame must not outrank the text",
			border, dim)
	}
}

// Every heat-ramp color must survive the deepest age fade at or above the
// WCAG 1.4.11 non-text floor: the oldest chart columns still carry history,
// and before fadeClamped they measured ~1.3:1 against the background.
func TestChartFadeHoldsNonTextContrast(t *testing.T) {
	for name, c := range map[string]lipgloss.Color{
		"cyan":   cCyan,
		"green":  cGreen,
		"yellow": cYellow,
		"red":    cRed,
	} {
		got := contrastRatioT(t, fadeClamped(c, 0.30, minGraphicContrast), cBase)
		if got < minGraphicContrast {
			t.Errorf("%s at max fade = %.2f:1 on cBase, want >= %.1f:1", name, got, minGraphicContrast)
		}
	}
}

// The wordmark is one accent, not a per-letter gradient: the site's identity
// is phosphor green on cool dark, and the dashboard has to match it.
func TestWordmarkUsesSiteAccent(t *testing.T) {
	want := lipgloss.NewStyle().Bold(true).Foreground(cGreen).Render("TOKTOP")
	if got := wordmark; got != want {
		t.Errorf("wordmark = %q, want single-accent TOKTOP", got)
	}
}

// fadeClamped must leave alone everything clamping cannot help: fades that
// already clear the floor and non-hex encodings; a color darker than the
// background stays at full strength rather than half-hidden.
func TestFadeClampPassthrough(t *testing.T) {
	// cYellow blended toward black by 0.9: 0xe3*0.9=0xcc, 0xb3*0.9=0xa1,
	// 0x41*0.9=0x3a, and that is well above the floor.
	const shallow = "#cca13a"
	if got := string(fadeClamped(cYellow, 0.9, minGraphicContrast)); got != shallow {
		t.Errorf("fade already above the floor was altered: %q, want %q", got, shallow)
	}
	if got := fadeClamped(lipgloss.Color("21"), 0.5, minGraphicContrast); got != lipgloss.Color("21") {
		t.Errorf("non-hex color was modified: %q", got)
	}
	dark := lipgloss.Color("#101010")
	if got := fadeClamped(dark, 0.5, minGraphicContrast); got != dark {
		t.Errorf("unreachable color was modified: %q", got)
	}
	if got := string(fadeClamped(cRed, 0.30, minGraphicContrast)); got == string(cRed) {
		t.Errorf("deep fade did not darken at all: %q", got)
	}
}

// heatColor colors chart marks (WCAG 1.4.11: >= 3:1 on cBase) and also text:
// the header out-rate and the per-agent rates in the agents-only view ride
// ramp colors at 4.5:1 (WCAG 1.4.3). Its quiet end once returned the surface
// gray #313244, ~1.3:1, hiding small-but-real rates right after startup or
// from slow agents; every point of the ramp must stay legible.
func TestHeatRampMeetsContrastFloors(t *testing.T) {
	for i := range 101 {
		f := float64(i) / 100
		c := heatColor(f)
		got := contrastRatioT(t, c, cBase)
		if got < minGraphicContrast {
			t.Errorf("heatColor(%.2f) on cBase = %.2f:1, want >= %.1f:1", f, got, minGraphicContrast)
		}
		if f <= 0.02 && got < 4.5 {
			t.Errorf("heatColor(%.2f) styles text too: %.2f:1 on cBase, want >= 4.5:1", f, got)
		}
	}
}

func TestHeatColorNaN(t *testing.T) {
	if got := heatColor(math.NaN()); got != cDim {
		t.Errorf("heatColor(NaN) = %v, want cDim (%v)", got, cDim)
	}
}

// The chart fade converts a blended channel straight into an index of
// linearChannel, which has 256 entries. A factor past 1 would index past the
// end of it, and a negative or NaN factor converts to a huge uint64 and does
// the same, so an unrepresentable factor has to clamp instead of indexing.
// clamp01 sends everything below zero (NaN included) to 0 and everything
// above one to 1.
func TestFadeClampsFactorToUnitRange(t *testing.T) {
	for _, f := range []float64{1.5, 2, 1e30, math.Inf(1)} {
		if got := fadeClamped(cRed, f, minGraphicContrast); got != cRed {
			t.Errorf("fadeClamped(%v) = %q, want the undimmed %q", f, got, cRed)
		}
	}
	for _, f := range []float64{-0.5, math.NaN()} {
		// fadeClamped bisects for the shallowest factor still above the
		// contrast floor, so the result is whichever hex that search
		// lands on. What matters here is only that it answers at all:
		// before the clamp it indexed linearChannel out of bounds.
		if got := fadeClamped(cRed, f, minGraphicContrast); !strings.HasPrefix(string(got), "#") || len(got) != 7 {
			t.Errorf("fadeClamped(%v) = %q, want a #rrggbb color", f, got)
		}
	}
}

// The kind badge is the one fixed-width cell in the frame, so every row's
// label starts in the same column. It is padded and cut in visible cells
// because the label beside it is: the %-9.9s it replaced counted runes, and
// a kind spelled in a wide script padded to 9 runes while rendering 18
// cells, pushing that row's label 9 cells right of every other and
// overflowing a narrow pane.
func TestKindBadgeIsFixedWidthInCells(t *testing.T) {
	// The last two are the same word in the two normalization forms, and they
	// look identical here on purpose: NFC is the precomposed U+00E9, NFD is
	// "e" plus U+0301. A badge cut between them renders the decomposed one
	// one cell wider than the composed one, so both must land on the same
	// width or the column below it wobbles.
	composed, decomposed := "caf\u00e9", "cafe\u0301"
	for _, kind := range []string{
		core.KindOllama, core.KindVLLM, core.KindLlamaCPP, core.KindGPUStack,
		"\u65e5\u672c\u8a9e\u30a8\u30f3\u30b8\u30f3", // every rune two cells wide
		composed,
		decomposed,
		"\U0001F469\u200D\U0001F4BB", // ZWJ sequence, one cell
		"unknown-kind-from-a-hostile-engine",
	} {
		if w := lipgloss.Width(kindBadge(kind)); w != kindBadgeCells {
			t.Errorf("kindBadge(%q) renders %d cells, want %d", kind, w, kindBadgeCells)
		}
	}
}

// panel composes its own frame instead of handing the block to
// panelStyle.Render, which re-measured every row with lipgloss's width after
// padBlock had already cut it. The two must print the same bytes on every
// color profile, or the panels change shape the moment the terminal's
// capabilities are detected differently.
func TestPanelFrameMatchesLipglossBorder(t *testing.T) {
	prev := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(prev)

	contents := []string{
		"", "a", "ab\ncd", "世界", "▲ 1.2k ▼ 900",
		styleOK.Render("ok") + "\nxyzzy long",
		"\x1b[31mred\x1b[0m",
		"\x1b]0;title\x07x",
		"éあ\nx",
	}
	for _, prof := range []termenv.Profile{
		termenv.Ascii, termenv.ANSI, termenv.ANSI256, termenv.TrueColor,
	} {
		lipgloss.SetColorProfile(prof)
		for _, content := range contents {
			for innerW := 1; innerW <= 12; innerW++ {
				for innerH := 1; innerH <= 4; innerH++ {
					block := padBlock(content, innerW, innerH)
					want := styleTitle.Render("T") + "\n" + panelStyle.Render(block)
					got := panel("T", content, innerW, innerH)
					if got != want {
						t.Fatalf("profile %v, %dx%d, content %q:\n got %q\nwant %q",
							prof, innerW, innerH, content, got, want)
					}
				}
			}
		}
	}
}

// Cutting the badge must not split a character: a kind that does not fit
// loses whole grapheme clusters, so the result is always valid UTF-8 and
// never ends between a base letter and its combining mark.
func TestKindBadgeCutsBetweenClusters(t *testing.T) {
	// The decomposed entry is the one that can cut between a base letter and
	// its combining mark, and it is the one a byte- or rune-counting cut
	// loses: the composed and decomposed spellings render one cell apart.
	for _, kind := range []string{"日本語エンジン", "café latte", "cafe\u0301 latte", "🙂 ok"} {
		got := kindBadge(kind)
		if !utf8.ValidString(got) {
			t.Errorf("kindBadge(%q) = %q, is not valid UTF-8", kind, got)
		}
		if strings.ContainsRune(got, '�') {
			t.Errorf("kindBadge(%q) = %q, holds a replacement rune", kind, got)
		}
		if w := lipgloss.Width(got); w != kindBadgeCells {
			t.Errorf("kindBadge(%q) renders %d cells, want %d", kind, w, kindBadgeCells)
		}
		if strings.HasPrefix(strings.TrimRight(got, " "), "\u0301") {
			t.Errorf("kindBadge(%q) cut between the base letter and its mark: %q", kind, got)
		}
	}
}

// A block carrying a tab is the one input lipgloss sizes differently (it
// expands tabs into spaces before measuring), so panel defers to it there.
func TestPanelDefersToLipglossOnTab(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	content := "a\tb"
	want := styleTitle.Render("T") + "\n" + panelStyle.Render(padBlock(content, 8, 2))
	if got := panel("T", content, 8, 2); got != want {
		t.Errorf("tabbed panel:\n got %q\nwant %q", got, want)
	}
}

// frame draws the border every panel and the SYS strip share. It is compared
// against panelStyle.Render directly, so both call sites are pinned by one
// check rather than by whatever the frame happens to look like.
func TestFrameMatchesLipglossBorder(t *testing.T) {
	prev := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(prev)

	for _, prof := range []termenv.Profile{
		termenv.Ascii, termenv.ANSI, termenv.ANSI256, termenv.TrueColor,
	} {
		lipgloss.SetColorProfile(prof)
		for innerW := 1; innerW <= 12; innerW++ {
			for innerH := 1; innerH <= 4; innerH++ {
				for _, content := range []string{
					"", "a", "ab\ncd", "世界", "▲ 1.2k ▼ 900", "⠁⠂⠄",
					styleOK.Render("ok") + "\nxyzzy long",
					"\x1b[31mred\x1b[0m", "\x1b]0;title\x07x", "éあ\nx",
				} {
					block := padBlock(content, innerW, innerH)
					want := panelStyle.Render(block)
					if got := frame(block, innerW, innerH); got != want {
						t.Fatalf("profile %v, %dx%d, content %q:\n got %q\nwant %q",
							prof, innerW, innerH, content, got, want)
					}
				}
			}
		}
	}
}

// Every row of a panel is the same visible width, wide glyphs included: a
// short row puts the right border a cell or two left of the others.
func TestPadBlockFillsEveryRow(t *testing.T) {
	for _, content := range []string{"世界", "ab\n世界\nc", "", "x"} {
		for innerW := 1; innerW <= 10; innerW++ {
			for innerH := 1; innerH <= 3; innerH++ {
				for i, ln := range strings.Split(padBlock(content, innerW, innerH), "\n") {
					if w := widthOf(ln); w != innerW {
						t.Errorf("padBlock(%q, %d, %d) row %d is %d cells, want %d",
							content, innerW, innerH, i, w, innerW)
					}
				}
			}
		}
	}
}
