package ui

import (
	"fmt"
	"math"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// tailCols returns exactly w columns carrying the last w values, zero-padded
// on the left so charts hug the right edge like a scope trace, plus their
// peak (floored at 1 so an all-zero series still renders).
func tailCols(vals []float64, w int) ([]float64, float64) {
	vs := vals
	if len(vs) > w {
		vs = vs[len(vs)-w:]
	}
	vMax := seriesPeak(vs)
	if vMax <= 0 {
		vMax = 1
	}
	pad := max(w-len(vs), 0)
	cols := make([]float64, pad+len(vs))
	copy(cols[pad:], vs)
	return cols, vMax
}

// seriesPeak is the largest value in vals, NaN skipped, and 0 for an empty
// series. It is the vertical scale a chart is drawn against, and the callers
// that publish it (throughputTitle, the plain report) are what give the braille
// plot a text alternative: the marks themselves carry no value a screen reader
// or a monochrome terminal can read.
func seriesPeak(vals []float64) float64 {
	peak := 0.0
	for _, v := range vals {
		if !math.IsNaN(v) && v > peak {
			peak = v
		}
	}
	return peak
}

// brailleRowMask is the pair of dots a filled sub-row sets. Dots 1-3 are the
// left column and 4-6 the right, and 7 and 8 are the bottom sub-row, so each
// entry is one sub-row filled across both columns of the cell.
var brailleRowMask = [4]byte{0x09, 0x12, 0x24, 0xC0}

// brailleGuideDot is the left dot of the bottom sub-row, drawn alone.
const brailleGuideDot = 0x40

// ChartStyle tunes BrailleChart rendering.
type ChartStyle struct {
	Heat func(float64) lipgloss.Color
	Grid map[int]bool // columns marked true get a faint baseline tick where the data leaves the bottom row empty
}

const hexDigits = "0123456789abcdef"

// formatHexRGB assembles the 7-byte "#rrggbb" form by hand: the chart fade
// calls it per bisection step, where fmt.Sprintf allocated several objects
// for one short string.
func formatHexRGB(r, g, b uint64) string {
	var out [7]byte
	out[0] = '#'
	for i, ch := range [3]uint64{r, g, b} {
		out[1+i*2] = hexDigits[ch>>4&0xf]
		out[2+i*2] = hexDigits[ch&0xf]
	}
	return string(out[:])
}

// minGraphicContrast is the WCAG 2.2 AA non-text floor (SC 1.4.11): a
// data-bearing chart mark must keep at least this ratio against the panel
// background, bloom or no bloom.
const minGraphicContrast = 3.0

// fadeClamped blends c toward black by factor f, but never past the darkest
// point that still meets min contrast against the dashboard background, so
// the age fade cannot melt data past legibility. Colors that cannot reach
// the floor at all (non-hex encodings, colors darker than the background)
// come back at full strength rather than half-hidden.
func fadeClamped(c lipgloss.Color, f, min float64) lipgloss.Color {
	r0, g0, b0, ok := parseHexRGB(c)
	if !ok {
		return c
	}
	rf, gf, bf := float64(r0), float64(g0), float64(b0)

	// The three conversions index linearChannel, so a factor outside 0..1
	// (or NaN) would index past the array rather than blend. clamp01 also
	// pins the bisection below to a range lumOf can answer for.
	lumOf := func(factor float64) (float64, uint64, uint64, uint64) {
		factor = clamp01(factor)
		r := uint64(rf * factor)
		g := uint64(gf * factor)
		b := uint64(bf * factor)
		lum := 0.2126*linearChannel[r] + 0.7152*linearChannel[g] + 0.0722*linearChannel[b]
		return lum, r, g, b
	}

	lum, r, g, b := lumOf(f)
	if contrastAgainstBaseLum(lum) >= min {
		return lipgloss.Color(formatHexRGB(r, g, b))
	}

	// The blend is monotonic (less factor = darker = lower ratio), so the
	// shallowest factor still above the floor can be bisected without
	// allocating intermediate strings.
	lo, hi := f, 1.0
	for range 16 {
		mid := (lo + hi) / 2
		midLum, _, _, _ := lumOf(mid)
		if contrastAgainstBaseLum(midLum) >= min {
			hi = mid
		} else {
			lo = mid
		}
	}
	_, rHi, gHi, bHi := lumOf(hi)
	return lipgloss.Color(formatHexRGB(rHi, gHi, bHi))
}

// BrailleChart renders an area chart as braille dot-matrix, btop-style: every
// terminal cell is a 2x4 dot grid, so a w*h chart resolves w*2 by h*4 dots -
// far finer than the block ramp. Values fill upward from the baseline. Older
// columns fade toward black; Grid columns draw a faint dotted guide on the
// bottom row wherever the data leaves it empty (marking timescale boundaries).
func BrailleChart(vals []float64, w, h int, st ChartStyle) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	cols, peak := tailCols(vals, w)

	dotH := h * 4
	levels := make([]float64, w)
	colColors := make([]lipgloss.Color, w)
	denom := float64(max(w-1, 1))
	for cx := range w {
		frac := clamp01(cols[cx] / peak)
		levels[cx] = frac * float64(dotH)
		col := st.Heat(frac)
		if frac > 0.02 {
			f := 0.30 + 0.70*(float64(cx)/denom)
			col = fadeClamped(col, f, minGraphicContrast)
		}
		colColors[cx] = col
	}

	// One style render per column, not per cell: a cell's bytes are its color's
	// escape prefix, the dot rune and the reset suffix, and only the middle one
	// changes as the pattern fills in. Style.Render measured the widest part of
	// this loop (word wrap, getLines, width on every call), so the escape run
	// is taken once per color and the cells concatenate.
	sides := make(map[string][2]string, w)
	for cx := range w {
		col := colColors[cx]
		if _, ok := sides[string(col)]; !ok {
			sides[string(col)] = styleSides(lipgloss.NewStyle().Foreground(col))
		}
	}

	rows := make([]strings.Builder, h)
	for cy := range h {
		rows[cy].Grow(w * 4)
		for cx := range w {
			level := levels[cx]
			pattern := 0
			for sr := range 4 {
				dy := cy*4 + sr
				if float64(dotH-dy) <= level {
					pattern |= int(brailleRowMask[sr])
				}
			}
			// faint guides only where data leaves the bottom row empty
			if pattern == 0 && cy == h-1 && st.Grid[cx] {
				pattern = brailleGuideDot
			}
			if pattern == 0 {
				rows[cy].WriteByte(' ')
				continue
			}
			sd := sides[string(colColors[cx])]
			rows[cy].WriteString(sd[0])
			rows[cy].WriteRune(0x2800 + rune(pattern))
			rows[cy].WriteString(sd[1])
		}
	}
	out := make([]string, h)
	for i := range rows {
		out[i] = rows[i].String()
	}
	return strings.Join(out, "\n")
}

// GaugeBar renders "━━━━── 62%": a heavy rule for the filled part, a dim one
// for the rest, then the percentage. No label, no brackets.
func GaugeBar(pct float64, w int, heat func(float64) lipgloss.Color) string {
	if w < 3 {
		w = 3
	}
	pct = clamp01(pct/100) * 100
	filled := min(int(pct/100*float64(w)), w)
	st := lipgloss.NewStyle().Foreground(heat(pct))
	bar := st.Render(strings.Repeat("━", filled)) + styleDim.Render(strings.Repeat("─", w-filled))
	return bar + " " + fmt.Sprintf("%.0f%%", pct)
}

// styleSides splits a style's rendering of one rune into the escape prefix and
// reset suffix around it, so a caller that draws many cells in the same color
// renders the style once instead of once per cell.
//
// The split reads the style's own output for a sentinel rune rather than
// composing the escape itself, so it tracks the active color profile: a
// terminal that gets truecolor, 256-color or no color at all gets exactly what
// Style.Render would have written for that profile, prefix and suffix included.
//
// A style writes the same prefix and the same suffix whatever it wraps, so
// prefix+suffix is the run's frame: the caller substitutes its own text
// between them and gets the bytes Style.Render would have produced.
func styleSides(st lipgloss.Style) [2]string {
	const sentinel = 'X'
	out := st.Render(string(rune(sentinel)))
	at := strings.IndexByte(out, sentinel)
	if at < 0 { // a profile that rewrote the rune: no prefix to recover
		return [2]string{out, ""}
	}
	return [2]string{out[:at], out[at+1:]}
}

// wrap applies a style's escape run around s. It is Style.Render for text this
// package has already measured and knows to be a single line of cells: the
// render measured it with a grapheme-cluster walk to learn a length the caller
// already had.
func wrap(s [2]string, text string) string {
	if s[0] == "" && s[1] == "" {
		return text
	}
	return s[0] + text + s[1]
}

func clamp01(v float64) float64 {
	if !(v > 0) { // also catches NaN: every comparison with it is false
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
