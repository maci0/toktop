package ui

import (
	"fmt"
	"math"
	"strconv"
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
	vMax := 0.0
	for _, v := range vs {
		if !math.IsNaN(v) && v > vMax {
			vMax = v
		}
	}
	if vMax <= 0 {
		vMax = 1
	}
	pad := max(w-len(vs), 0)
	cols := make([]float64, pad+len(vs))
	copy(cols[pad:], vs)
	return cols, vMax
}

// brailleBits maps (sub-row, sub-column) within a braille cell to its Unicode
// bit: dot1..8 = 0x01,0x02,0x04,0x08,0x10,0x20,0x40,0x80.
var brailleBits = [4][2]byte{
	{0x01, 0x08},
	{0x02, 0x10},
	{0x04, 0x20},
	{0x40, 0x80},
}

// ChartStyle tunes BrailleChart rendering.
type ChartStyle struct {
	Heat func(float64) lipgloss.Color
	Grid map[int]bool // columns marked true get a faint vertical guide
}

const hexDigits = "0123456789abcdef"

// fadeColor blends a hex color toward black by factor f (0..1). Non-hex
// colors pass through untouched.
//
// The chart fade calls this per bisection step, so the hex is assembled by
// hand: fmt.Sprintf here allocated several objects per step for one 7-byte
// string.
func fadeColor(c lipgloss.Color, f float64) string {
	s := string(c)
	if !strings.HasPrefix(s, "#") || len(s) != 7 {
		return s
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return s
	}
	r := uint64(float64(v>>16&0xff) * f)
	g := uint64(float64(v>>8&0xff) * f)
	b := uint64(float64(v&0xff) * f)
	var out [7]byte
	out[0] = '#'
	for i, ch := range [3]uint64{r, g, b} {
		out[1+i*2] = hexDigits[ch>>4&0xf]
		out[2+i*2] = hexDigits[ch&0xf]
	}
	return string(out[:])
}

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
	s := string(c)
	if !strings.HasPrefix(s, "#") || len(s) != 7 {
		return c
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return c
	}
	r0 := float64(v >> 16 & 0xff)
	g0 := float64(v >> 8 & 0xff)
	b0 := float64(v & 0xff)

	lumOf := func(factor float64) (float64, uint64, uint64, uint64) {
		r := uint64(r0 * factor)
		g := uint64(g0 * factor)
		b := uint64(b0 * factor)
		lum := 0.2126*linearChannel[r] + 0.7152*linearChannel[g] + 0.0722*linearChannel[b]
		return lum, r, g, b
	}

	lum, r, g, b := lumOf(f)
	if contrastAgainstBaseLum(lum) >= min {
		return lipgloss.Color(formatHexRGB(r, g, b))
	}

	// fadeColor is monotonic (less factor = darker = lower ratio), so the
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
// columns fade toward black; Grid columns draw a faint dotted baseline guide
// through empty cells (used to mark timescale boundaries).
func BrailleChart(vals []float64, w, h int, st ChartStyle) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	cols, peak := tailCols(vals, w)

	dotH := h * 4
	type cacheKey struct {
		color   string
		pattern int
	}
	cache := map[cacheKey]string{}

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

	rows := make([]strings.Builder, h)
	for cy := range h {
		rows[cy].Grow(w * 4)
		for cx := range w {
			level := levels[cx]
			pattern := 0
			for sr := range 4 {
				dy := cy*4 + sr
				if float64(dotH-dy) <= level {
					pattern |= int(brailleBits[sr][0]) | int(brailleBits[sr][1])
				}
			}
			// faint guides only where data leaves the bottom row empty
			if pattern == 0 && cy == h-1 && st.Grid[cx] {
				pattern = int(brailleBits[3][0])
			}
			if pattern == 0 {
				rows[cy].WriteByte(' ')
				continue
			}
			col := colColors[cx]
			k := cacheKey{color: string(col), pattern: pattern}
			s, ok := cache[k]
			if !ok {
				s = lipgloss.NewStyle().Foreground(col).Render(string(rune(0x2800 + pattern)))
				cache[k] = s
			}
			rows[cy].WriteString(s)
		}
	}
	out := make([]string, h)
	for i := range rows {
		out[i] = rows[i].String()
	}
	return strings.Join(out, "\n")
}

// GaugeBar renders "[██████░░░░] 62%"-style meter content without label.
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

func clamp01(v float64) float64 {
	if !(v > 0) { // also catches NaN: every comparison with it is false
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
