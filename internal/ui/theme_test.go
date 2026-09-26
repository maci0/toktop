package ui

import (
	"math"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

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
	shallow := string(fadeColor(cYellow, 0.9)) // well above the floor
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
