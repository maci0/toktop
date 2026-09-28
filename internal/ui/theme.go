package ui

import (
	"math"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/maci0/toktop/internal/core"
)

// Palette follows the toktop.ai tokens in site/worker.js (cool dark, green
// accent, amber pressure): bg, fg, dim, accent and warm are shared values.
// The border and status colors are TUI-only, the site drawing its dividers
// from --line. Degrades to nearest 256/16 colors on old terminals.
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

	// panelStyle is the whole lipgloss frame. panel() and frame() compose
	// their own border out of borderStyle instead, which avoids a second
	// width measurement per row; this survives only as frame()'s fallback
	// for a block carrying a tab.
	panelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(cBorder).
			Padding(0, 1)

	// borderStyle is the frame color alone, without lipgloss's border and
	// padding pass. panel() composes the frame from it directly.
	borderStyle = lipgloss.NewStyle().Foreground(cBorder)

	helpStyle = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(cDim).
			Padding(1, 2).
			Background(cBase)
)

// panel wraps content in a titled rounded box. Content is padded/cut to
// innerW x innerH with plain spaces; the padding is ours rather than
// lipgloss's, whose wrapping mishandles densely styled chart cells.
func panel(title, content string, innerW, innerH int) string {
	return styleTitle.Render(title) + "\n" + frame(padBlock(content, innerW, innerH), innerW, innerH)
}

// frame draws the rounded border around a block padBlock has already cut to
// exactly innerW columns and innerH rows.
//
// The border is composed rather than handed to panelStyle.Render, which spent
// a third of a frame's CPU re-measuring every row with lipgloss's width. That
// width is known here: the rows are innerW cells, the edge is innerW+2 box
// runes, and a box rune is one cell (see singleCellRunes), so a grapheme-cluster
// walk over the block answers nothing the caller has not already decided.
//
// A block carrying a tab is the exception lipgloss handles differently: it
// expands tabs into spaces before measuring. A tab should not reach a frame
// (clip and shorten fold them away), and deferring keeps that a rendering
// question rather than a silent one.
func frame(block string, innerW, innerH int) string {
	if strings.ContainsRune(block, '\t') {
		return panelStyle.Render(block)
	}
	lines := strings.Split(block, "\n")
	out := make([]string, 0, len(lines)+2)
	// One style render per frame. The escape run around the text is the same
	// for every string the style wraps, so it is rendered once and the text
	// substituted into it.
	sides := styleSides(borderStyle)
	edge := strings.Repeat("─", max(innerW+2, 0))
	vert := wrap(sides, "│")
	out = append(out, wrap(sides, "╭"+edge+"╮"))
	for _, ln := range lines {
		out = append(out, vert+" "+ln+" "+vert)
	}
	out = append(out, wrap(sides, "╰"+edge+"╯"))
	return strings.Join(out, "\n")
}

// padBlock forces content to exactly innerW columns and innerH rows.
//
// Every line reaches innerW: a line that overflows is clipped first and then
// padded, because clip returns the ellipsis short of the cut it was given (a
// wide glyph that will not fit half a cell), and a block whose rows are not
// all the same width is a block whose right border does not line up.
func padBlock(content string, innerW, innerH int) string {
	lines := strings.Split(content, "\n")
	if len(lines) > innerH {
		lines = lines[:innerH]
	}
	for i, ln := range lines {
		if widthOf(ln) > innerW {
			ln = clip(ln, innerW)
		}
		if gap := innerW - widthOf(ln); gap > 0 {
			ln += strings.Repeat(" ", gap)
		}
		lines[i] = ln
	}
	for len(lines) < innerH {
		lines = append(lines, strings.Repeat(" ", innerW))
	}
	return strings.Join(lines, "\n")
}

// kindBadgeCells is the visible width every kind badge occupies, so the
// label beside it starts in the same column on every row.
const kindBadgeCells = 9

// kindBadge renders a fixed-width colored tag for a backend kind. Padded and
// cut in visible cells, like every other cell in this package: the %-9.9s it
// replaced counted runes, so a kind spelled in a wide script (a discovered
// engine named in Japanese) padded to 9 runes and rendered 18 cells, pushing
// its label 9 cells right of every other row and overflowing a narrow pane.
// shorten also cuts between grapheme clusters, where the precision verb cut
// between runes and left a combining mark or a multi-byte sequence split.
func kindBadge(kind string) string {
	st, ok := kindStyles[kind]
	if !ok {
		st = styleDim
	}
	return st.Render(padTo(shorten(kind, kindBadgeCells), kindBadgeCells))
}

// heatColor maps 0..1 intensity onto a cool-to-hot ramp (cyan, green, amber,
// red). The quiet end floors at cDim: the ramp also colors text (header
// and per-agent rates) that must hold 4.5:1 on cBase (WCAG 1.4.3), and its
// near-zero cells are data-bearing chart marks bound by the same 3:1 floor
// as the faded columns (WCAG 1.4.11). A surface-gray floor measured ~1.3:1,
// invisible to low-vision users exactly when the rate it labels is small
// next to the session peak.
func heatColor(f float64) lipgloss.Color {
	if math.IsNaN(f) || f <= 0.02 {
		return cDim
	}
	switch {
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

// heatBand is the three-band ramp the percentage meters share: green below
// warn, amber below crit, red above. A NaN reading is green, since it carries
// no value to alarm on.
func heatBand(v, warn, crit float64) lipgloss.Color {
	if math.IsNaN(v) || v < warn {
		return cGreen
	}
	if v < crit {
		return cYellow
	}
	return cRed
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

// parseHexRGB splits a "#rrggbb" color into its three 0..255 channels. ok is
// false for any other encoding (256-color names, #rgb, #rrggbbaa), which every
// caller here passes through or treats as already visible rather than
// guessing at its channels. The one place the three color consumers agree on
// what counts as hex, so a second spelling cannot drift from this one.
func parseHexRGB(c lipgloss.Color) (r, g, b uint64, ok bool) {
	s := string(c)
	if len(s) != 7 || s[0] != '#' {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return v >> 16 & 0xff, v >> 8 & 0xff, v & 0xff, true
}

// relLuminance computes the WCAG 2.x relative luminance of a #rrggbb hex
// color. ok is false for any other encoding (256-color names): callers must
// treat those as already visible rather than guessing at their brightness.
//
// Only the base background goes through this: the chart fade computes
// luminance inline from linearChannel, so a per-column parse would be wasted
// work.
func relLuminance(c lipgloss.Color) (lum float64, ok bool) {
	r, g, b, ok := parseHexRGB(c)
	if !ok {
		return 0, false
	}
	return 0.2126*linearChannel[r] +
		0.7152*linearChannel[g] +
		0.0722*linearChannel[b], true
}

// baseLum is cBase's luminance, which is constant: the fade compares every
// blend against it. It is not in a var block with cBase, whose file placement
// keeps the palette together.
var baseLum = func() float64 {
	l, _ := relLuminance(cBase)
	return l
}()

// contrastAgainstBaseLum is the WCAG contrast of a luminance against the
// panel background, read from baseLum instead of recomputed per call.
func contrastAgainstBaseLum(lc float64) float64 {
	if lc < baseLum {
		return (baseLum + 0.05) / (lc + 0.05)
	}
	return (lc + 0.05) / (baseLum + 0.05)
}
