package ui

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/maci0/toktop/internal/core"
)

// Palette matches the toktop.ai tokens in site/worker.js (cool dark, green
// accent, amber pressure). Degrades to nearest 256/16 colors on old terminals.
var (
	cBase = lipgloss.Color("#0d1117")
	// Secondary text must stay >= 4.5:1 on cBase (WCAG 1.4.3). Site --dim
	// #7d8895 is 5.25:1 here; do not drop it back under the floor.
	cDim    = lipgloss.Color("#7d8895")
	cText   = lipgloss.Color("#d7dde5")
	cBorder = lipgloss.Color("#4a5563")
	cRed    = lipgloss.Color("#e36d6d")
	cGreen  = lipgloss.Color("#4cc38a") // --accent
	cYellow = lipgloss.Color("#e3b341") // --warm
	cBlue   = lipgloss.Color("#7aa2d4")
	cCyan   = lipgloss.Color("#5ec8d8")
)

var (
	styleTitle = lipgloss.NewStyle().Bold(true).Foreground(cText)
	styleDim   = lipgloss.NewStyle().Foreground(cDim)
	styleValue = lipgloss.NewStyle().Bold(true).Foreground(cText)

	styleOK   = lipgloss.NewStyle().Foreground(cGreen)
	styleWarn = lipgloss.NewStyle().Foreground(cYellow)
	styleBad  = lipgloss.NewStyle().Foreground(cRed)
	styleInfo = lipgloss.NewStyle().Foreground(cCyan)

	dotUp = styleOK.Render("●")
	// ✗, not a red ●: down must read without color (WCAG 1.4.1), and it
	// matches the ✓/✗ convention the probe rows already use.
	dotBad = styleBad.Render("✗")
	// Partial-up keeps the dot shape: the header spells the count out
	// numerically right beside it ("2/3 engines"), so color is redundant.
	dotWarn = styleWarn.Render("●")

	kindStyles = map[string]lipgloss.Style{
		core.KindOllama:    lipgloss.NewStyle().Foreground(cYellow),
		core.KindVLLM:      lipgloss.NewStyle().Foreground(cGreen),
		core.KindLlamaCPP:  lipgloss.NewStyle().Foreground(cCyan),
		core.KindOpenAI:    lipgloss.NewStyle().Foreground(cBlue),
		core.KindSGLang:    lipgloss.NewStyle().Foreground(cBlue),
		core.KindTRTLLM:    lipgloss.NewStyle().Foreground(cGreen),
		core.KindMLX:       lipgloss.NewStyle().Foreground(cCyan),
		core.KindLMStudio:  lipgloss.NewStyle().Foreground(cBlue),
		core.KindKoboldCPP: lipgloss.NewStyle().Foreground(cYellow),
		core.KindLocalAI:   lipgloss.NewStyle().Foreground(cCyan),
		core.KindTGI:       lipgloss.NewStyle().Foreground(cGreen),
		core.KindLiteLLM:   lipgloss.NewStyle().Foreground(cBlue),
		core.KindGPUStack:  lipgloss.NewStyle().Foreground(cGreen),
		core.KindLemonade:  lipgloss.NewStyle().Foreground(cYellow),
		// Routing proxies share blue (litellm); OmniRoute is detected
		// via its X-OmniRoute-Route-Class header and must not fall through
		// to the dim unknown-kind badge.
		core.KindOmniRoute: lipgloss.NewStyle().Foreground(cBlue),
	}

	panelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(cBorder).
			Padding(0, 1)

	helpStyle = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(cDim).
			Padding(1, 2).
			Background(cBase)
)

// panel wraps content in a titled rounded box. Content is padded/cut to
// innerW x innerH with plain spaces; we deliberately avoid lipgloss Width()
// here because its wrapping mishandles densely styled chart cells.
func panel(title, content string, innerW, innerH int) string {
	body := panelStyle.Render(padBlock(content, innerW, innerH))
	return styleTitle.Render(title) + "\n" + body
}

// padBlock forces content to exactly innerW columns and innerH rows.
func padBlock(content string, innerW, innerH int) string {
	lines := strings.Split(content, "\n")
	if len(lines) > innerH {
		lines = lines[:innerH]
	}
	for i, ln := range lines {
		if gap := innerW - widthOf(ln); gap > 0 {
			ln += strings.Repeat(" ", gap)
		} else {
			ln = clip(ln, innerW)
		}
		lines[i] = ln
	}
	for len(lines) < innerH {
		lines = append(lines, strings.Repeat(" ", innerW))
	}
	return strings.Join(lines, "\n")
}

// kindBadge renders a fixed-width colored tag for a backend kind.
func kindBadge(kind string) string {
	st, ok := kindStyles[kind]
	if !ok {
		st = styleDim
	}
	return st.Render(fmt.Sprintf("%-9.9s", kind))
}

// heatColor maps 0..1 intensity onto a cool-to-hot ramp (cyan, green, amber,
// red). The quiet end floors at cDim: the ramp also colors text (header
// and per-agent rates) that must hold 4.5:1 on cBase (WCAG 1.4.3), and its
// near-zero cells are data-bearing chart marks bound by the same 3:1 floor
// as the faded columns (WCAG 1.4.11). A surface-gray floor measured ~1.3:1,
// invisible to low-vision users exactly when the rate it labels is small
// next to the session peak.
func heatColor(f float64) lipgloss.Color {
	switch {
	case f <= 0.02:
		return cDim
	case f < 0.30:
		return cCyan
	case f < 0.58:
		return cGreen
	case f < 0.82:
		return cYellow
	default:
		return cRed
	}
}

// wordmark is TOKTOP in the site accent. Static: built once, reused every frame.
var wordmark = lipgloss.NewStyle().Bold(true).Foreground(cGreen).Render("TOKTOP")

// linearChannel maps an 8-bit channel to its WCAG linear value. A table,
// because the chart fade recomputes luminance thousands of times per frame
// and math.Pow was the bulk of that path's CPU.
var linearChannel = func() [256]float64 {
	var t [256]float64
	for i := range t {
		f := float64(i) / 255
		if f <= 0.03928 {
			t[i] = f / 12.92
		} else {
			t[i] = math.Pow((f+0.055)/1.055, 2.4)
		}
	}
	return t
}()

// relLuminance computes the WCAG 2.x relative luminance of a #rrggbb hex
// color. ok is false for any other encoding (256-color names): callers must
// treat those as already visible rather than guessing at their brightness.
//
// This runs inside the chart fade, which bisects a blend against the contrast
// floor for every column of every chart, so it parses with strconv: the
// fmt.Sscanf spelling of the same parse allocated a scanner per call and was
// the single largest source of per-frame garbage.
func relLuminance(c lipgloss.Color) (lum float64, ok bool) {
	s := string(c)
	if len(s) != 7 || s[0] != '#' {
		return 0, false
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return 0, false
	}
	return 0.2126*linearChannel[v>>16&0xff] +
		0.7152*linearChannel[v>>8&0xff] +
		0.0722*linearChannel[v&0xff], true
}

// baseLum is cBase's luminance, which is constant: the fade compares every
// blend against it. It is not in a var block with cBase, whose file placement
// keeps the palette together.
var baseLum = func() float64 {
	l, _ := relLuminance(cBase)
	return l
}()

// contrastAgainstBase is the WCAG contrast of c against the panel
// background, with the background's luminance read from baseLum instead of
// recomputed per call. A non-hex color reports 0, the same "not measurable"
// answer contrastRatio gives, so the fade treats it as full strength.
func contrastAgainstBase(c lipgloss.Color) float64 {
	lc, ok := relLuminance(c)
	if !ok {
		return 0
	}
	if lc < baseLum {
		return (baseLum + 0.05) / (lc + 0.05)
	}
	return (lc + 0.05) / (baseLum + 0.05)
}

// contrastRatio returns the WCAG contrast ratio between two colors; ok is
// false when either side is not #rrggbb (see relLuminance).
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
