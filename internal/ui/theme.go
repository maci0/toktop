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
	cDim  = lipgloss.Color("#7d8895")
	cText = lipgloss.Color("#d7dde5")
	// The frame color carries the 3:1 non-text floor, not the 4.5:1 one
	// (WCAG 1.4.11): a border is a UI component's boundary, and it is the only
	// thing that says where one panel ends and the next begins. The charts
	// stack with no blank row between them, so a low-vision reader meets one
	// unbroken column of glyphs above and below a rule they cannot see. The
	// former #4a5563 sat at 2.50:1, under the floor every chart mark in this
	// package is held to; this is 3.49:1, still well under cDim so the frame
	// stays quieter than the text it encloses.
	cBorder = lipgloss.Color("#5f6b7a")
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

	// The strip separator, rendered once. Every SYS, header and agent row
	// joins on it, and the joiner re-rendered the same three cells from a
	// literal at each call site: a Style.Render is a word-wrap and a width
	// measurement, and a frame spends those on a constant.
	sepDim = styleDim.Render(" │ ")

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

// backdropStyle is cBase as a background rather than as an assumption. Every
// ratio quoted against cBase above is a claim about how the palette reads on
// that surface, and nothing in the frame was painting it: the drawn dashboard
// set foreground colors and left the backdrop to whatever the terminal profile
// happened to be. On a dark profile the numbers hold. On a light one the
// primary text (#d7dde5) falls to 1.37:1 and cDim to 3.60:1, so the whole
// dashboard is a light-gray wash a low-vision reader cannot read at all
// (WCAG 1.4.3), and the border that separates one panel from the next inverts
// from 3.49:1 to 5.43:1 (WCAG 1.4.11). helpStyle already painted this surface
// for the keys overlay; painting it for the frame makes the palette's documented
// contract true on every terminal instead of on the common one.
var backdropStyle = lipgloss.NewStyle().Background(cBase)

// resetSeq ends every styled run this package writes, and it is what clears the
// backdrop partway along a line: an inner Style.Render closes with it, so the
// cells after it would fall back to the terminal's own background unless the
// backdrop is re-asserted. lipgloss does not do that for a wrapped style, so
// paintBackdrop splits on it.
const resetSeq = "\x1b[0m"

// paintBackdrop fills the frame's cells with cBase and re-asserts it after
// every inner reset, padding short rows out to w so no column falls through to
// the terminal. It is a no-op on a profile with no color at all: styleSides
// recovers an empty run there, so the escape the frame needs is the one the
// palette never had.
func paintBackdrop(s string, w int) string {
	sides := styleSides(backdropStyle)
	if sides[0] == "" && sides[1] == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if gap := w - widthOf(ln); gap > 0 {
			ln += strings.Repeat(" ", gap)
		}
		lines[i] = sides[0] + strings.ReplaceAll(ln, resetSeq, resetSeq+sides[0]) + sides[1]
	}
	return strings.Join(lines, "\n")
}

// panel wraps content in a titled rounded box. Content is padded/cut to
// innerW x innerH with plain spaces; the padding is ours rather than
// lipgloss's, whose wrapping mishandles densely styled chart cells.
func panel(title, content string, innerW, innerH int) string {
	return styleTitle.Render(title) + "\n" + frame(padBlock(content, innerW, innerH), innerW)
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
// expands tabs into spaces before measuring, and the block is measured by
// cluster count here, so the two disagree and the frame's right edge drifts.
// Nothing upstream strips a tab: sanitize keeps \n and \t, and clip and
// shorten only trim, so a provider name or a probe label carrying one reaches
// this. Handing the whole block to lipgloss keeps the fallback a rendering
// question rather than a silent one, at the cost of the fast path for a frame
// that has one.
func frame(block string, innerW int) string {
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
//
// A line is measured once. Clipping rewrites the line, so its width is taken
// again only in that branch; the common case, a line already inside the panel,
// kept the width measured above it instead of re-walking the same bytes.
func padBlock(content string, innerW, innerH int) string {
	lines := strings.Split(content, "\n")
	if len(lines) > innerH {
		lines = lines[:innerH]
	}
	for i, ln := range lines {
		w := widthOf(ln)
		if w > innerW {
			ln = clip(ln, innerW)
			w = widthOf(ln)
		}
		if gap := innerW - w; gap > 0 {
			ln += spaces(gap)
		}
		lines[i] = ln
	}
	for len(lines) < innerH {
		lines = append(lines, spaces(innerW))
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

// The three-band ramps and where they break. Named because the drawn color,
// the drawn mark and the plain report's word are read from one classifier, and
// a threshold spelled twice is a threshold that can disagree with itself.
const (
	tempWarnC, tempCritC   = 60, 80
	memWarnPct, memCritPct = 70, 90
	kvWarnPct, kvCritPct   = 60, 85
)

// band is one step of a three-band ramp. A reading's severity is a band, not a
// color: the drawn frame spends color on it, but color alone leaves a
// colorblind reader, a monochrome terminal and every screen-reader user with
// no way to tell a cool reading from a critical one (WCAG 1.4.1). The same
// step carries all three channels, so no two can report a different band for
// one reading.
type band int

const (
	bandOK band = iota
	bandWarn
	bandCrit
)

// bandOf classifies v against a ramp's two thresholds. A NaN reading is
// bandOK: it carries no value to alarm on.
func bandOf(v, warn, crit float64) band {
	switch {
	case math.IsNaN(v) || v < warn:
		return bandOK
	case v < crit:
		return bandWarn
	}
	return bandCrit
}

// color is the band on the drawn frame. Repetition, not hue, is what separates
// the two steps here: a reader who cannot see amber still sees two marks.
func (b band) color() lipgloss.Color {
	switch b {
	case bandWarn:
		return cYellow
	case bandCrit:
		return cRed
	}
	return cGreen
}

// markPrefix is the non-color channel the drawn frame puts in front of a
// reading, carrying its own trailing space so a caller concatenates it onto
// the value with nothing to trim. The OK band prefixes nothing: the common
// case is a cool reading, and a mark on every row would be one more glyph
// between the reader and the number they came for.
func (b band) markPrefix() string {
	switch b {
	case bandWarn:
		return "! "
	case bandCrit:
		return "!! "
	}
	return ""
}

// word is the severity spelled out for the plain report, which has no color at
// all. Named for the reading rather than the ramp: 92% memory and 92 degrees
// are both "critical", and "hot" would be wrong for the first.
func (b band) word() string {
	switch b {
	case bandWarn:
		return "high"
	case bandCrit:
		return "critical"
	}
	return ""
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
