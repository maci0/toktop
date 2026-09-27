package ui

import (
	"math"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
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

// Both fade helpers convert a blended channel straight into an index of
// linearChannel, which has 256 entries. A factor past 1 would index past the
// end of it, and a negative or NaN factor converts to a huge uint64 and does
// the same, so an unrepresentable factor has to clamp instead of indexing.
// clamp01 sends everything below zero (NaN included) to 0 and everything
// above one to 1.
func TestFadeClampsFactorToUnitRange(t *testing.T) {
	for _, f := range []float64{1.5, 2, 1e30, math.Inf(1)} {
		if got := fadeColor(cRed, f); got != string(cRed) {
			t.Errorf("fadeColor(%v) = %q, want the undimmed %q", f, got, cRed)
		}
		if got := fadeClamped(cRed, f, minGraphicContrast); got != cRed {
			t.Errorf("fadeClamped(%v) = %q, want the undimmed %q", f, got, cRed)
		}
	}
	for _, f := range []float64{-0.5, math.NaN()} {
		if got := fadeColor(cRed, f); got != "#000000" {
			t.Errorf("fadeColor(%v) = %q, want #000000", f, got)
		}
		// fadeClamped bisects for the shallowest factor still above the
		// contrast floor, so the result is whichever hex that search
		// lands on. What matters here is only that it answers at all:
		// before the clamp it indexed linearChannel out of bounds.
		if got := fadeClamped(cRed, f, minGraphicContrast); !strings.HasPrefix(string(got), "#") || len(got) != 7 {
			t.Errorf("fadeClamped(%v) = %q, want a #rrggbb color", f, got)
		}
	}
	if got := fadeColor(cRed, 0); got != "#000000" {
		t.Errorf("fadeColor(0) = %q, want #000000", got)
	}
}
