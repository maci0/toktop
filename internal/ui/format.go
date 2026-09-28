package ui

// Formatting and small layout helpers used by the renderers in ui.go:
// number/duration/byte formatting plus visible-width text cutting.
// Rendering primitives live in spark.go, styles in theme.go.

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/rivo/uniseg"

	"github.com/maci0/toktop/internal/core"
)

func primaryModel(p core.ProviderSnapshot) string {
	if len(p.Models) > 0 {
		return p.Models[0].Name
	}
	return "-"
}

func norm(v, vMax float64) float64 {
	if vMax <= 0 || math.IsNaN(vMax) || !(v > 0) {
		return 0
	}
	return clamp01(v / vMax)
}

// unitRound is the fraction a value must reach in the next unit up for the
// rounded rendering to cross the boundary: 999.5 to "%.0f" prints 1000, and
// 999.95 to "%.1f" prints 1000.0. The unit is chosen on the rounded value, so
// a count reads 1.0M rather than 1000.0k, and the same magnitude does not
// change spelling across a boundary.
const (
	unitRound   = 999.95
	unitRoundTo = 999.5
)

// Values past which the scaled form changes spelling. rateNoDecimal is where a
// rate drops its decimal (10.0k/tok/s reads better as 10k, and a dot-separated
// rate looks like a float the reader must convert); countNoDecimal is where a
// count changes unit to M, keeping its one decimal since the trailing zero
// there is uniform.
const (
	rateNoDecimal  = 10000
	countNoDecimal = 1000000
)

func fmtRate(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "0.0"
	}
	k := v / 1000
	switch {
	case v >= rateNoDecimal || k >= unitRound:
		return fmt.Sprintf("%.0fk", k)
	case v >= unitRoundTo:
		return fmt.Sprintf("%.1fk", k)
	case v >= 100:
		return fmt.Sprintf("%.0f", v)
	default:
		return fmt.Sprintf("%.1f", v)
	}
}

func fmtCount(n int64) string {
	k := float64(n) / 1000
	switch {
	case n >= countNoDecimal || k >= unitRound:
		return fmt.Sprintf("%.1fM", k/1000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", k)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func fmtMs(ms float64) string {
	if !(ms > 0) || math.IsInf(ms, 0) {
		return "-"
	}
	// unitRoundTo, for the reason unitRound gives: the unit is chosen on the
	// value as rendered, so 999.6 does not print "1000ms" one frame before
	// 1000 prints "1.00s".
	if ms >= unitRoundTo {
		return fmt.Sprintf("%.2fs", ms/1000)
	}
	return fmt.Sprintf("%.0fms", ms)
}

func fmtDur(d time.Duration) string {
	if d < 0 {
		// A future event (sender clock ahead) or a stepped wall clock can
		// yield a negative idle/uptime; "idle -5s" is not a duration.
		d = 0
	}
	d = d.Truncate(time.Second)
	if d < time.Minute {
		return d.String()
	}
	// Past an hour, minutes-only strings ("75m30s") make the reader do the
	// hours math; uptimes and agent idle spans routinely cross that mark.
	// int64, not int: a 32-bit int wraps a duration past ~68 years of
	// seconds, which host uptime can approach; the hour/minute breakdown
	// would then print garbage.
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", d/time.Minute, int64(d/time.Second)%60)
	}
	return fmt.Sprintf("%dh%02dm", int64(d/time.Hour), int64(d/time.Minute)%60)
}

// humanBytes renders a byte count. The KiB tier exists because a sub-MiB
// value (a small size_vram) would otherwise round to a flat "0MiB", reading
// as no allocation at all.
func humanBytes(b uint64) string {
	const g = 1 << 30
	if b >= g {
		return fmt.Sprintf("%.1fGiB", float64(b)/g)
	}
	if b >= 1<<20 {
		return fmt.Sprintf("%.0fMiB", float64(b)/(1<<20))
	}
	return fmt.Sprintf("%.0fKiB", float64(b)/(1<<10))
}

// humanBytesShort is the compact form used in the system strip. Same unit,
// shorter suffix: the strip has no room for "iB".
func humanBytesShort(b uint64) string {
	const m = 1 << 20
	if b >= 10<<30 {
		return fmt.Sprintf("%.0fG", float64(b)/(1<<30))
	}
	if b >= 1<<30 {
		return fmt.Sprintf("%.1fG", float64(b)/(1<<30))
	}
	if b >= m {
		return fmt.Sprintf("%.0fM", float64(b)/m)
	}
	return fmt.Sprintf("%.0fK", float64(b)/(1<<10))
}

// plainWidth returns the visible width of s when every rune is single-cell,
// else -1. The Width fast path: every frame calls Width on dozens of short
// labels ("TOKTOP", "engine-0", "v0.12.0") and on whole chart and border
// lines, and Width splits on "\n" (genSplit alloc) then walks graphemes.
// Printable ASCII is one cell per byte, and a rune in singleCellRunes is one
// cell per rune, so a byte scan answers for both. Everything else falls back to
// Width, which knows about wide runes and combining marks.
//
// SGR and OSC escape sequences are skipped rather than treated as a bail: the
// styled strings are most of what a frame measures (every chart row, every
// panel body, every clipped frame line), and each one otherwise sent Width
// through a grapheme-cluster walk to arrive at the same count. An escape shape
// this does not recognize is a bail, not a guess.
func plainWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c == 0x1b {
			next, ok := skipEscape(s, i+1)
			if !ok {
				return -1
			}
			i = next
			continue
		}
		if c < 0x20 || c >= 0x7f {
			r, size := utf8.DecodeRuneInString(s[i:])
			if !singleCellRune(r) {
				return -1
			}
			i += size
			w++
			continue
		}
		i++
		w++
	}
	return w
}

// singleCellRunes are the ranges plainWidth counts one cell per rune in. Each
// is a run of code points lipgloss.Width reports as one cell, and each holds
// no Extend, SpacingMark, regional indicator or emoji code point, so no two
// neighbours in them join into one grapheme cluster: a per-rune count is
// therefore the same answer a cluster walk arrives at.
//
// Box drawing and braille dots carry the charts and the panel frames, and the
// typographic marks (dashes, quotes, arrows, daggers, ellipsis) and Latin-1
// punctuation carry the labels. Wide and zero-width code points (CJK, Hangul,
// combining accents, emoji) are outside every range, so they still defer.
var singleCellRunes = [...][2]rune{
	{0x00A0, 0x00AC},
	{0x00AE, 0x00FF},
	{0x2000, 0x200A},
	{0x2010, 0x2027},
	{0x202F, 0x205F},
	{0x2070, 0x20CF},
	{0x20F1, 0x22FF},
	{0x2500, 0x25FC},
	{0x25FF, 0x25FF},
	{0x2700, 0x2704},
	{0x2706, 0x2709},
	{0x270C, 0x2727},
	{0x2729, 0x274B},
	{0x274D, 0x274D},
	{0x274F, 0x2752},
	{0x2756, 0x2756},
	{0x2758, 0x2794},
	{0x2798, 0x27AF},
	{0x27B1, 0x27BE},
	{0x2800, 0x28FF},
}

func singleCellRune(r rune) bool {
	for _, rg := range singleCellRunes {
		if r >= rg[0] && r <= rg[1] {
			return true
		}
	}
	return false
}

// skipEscape returns the index just past one escape sequence whose ESC is at
// i-1, for the shapes that carry no cells: CSI, OSC and the two-byte form.
// ok is false for anything else, so a string with a sequence this does not
// model is measured by lipgloss instead of by a rule that might be wrong.
//
// An unterminated sequence is a bail: swallowing the rest of the string would
// report a width for a payload whose cells were never counted.
func skipEscape(s string, i int) (int, bool) {
	if i >= len(s) {
		return 0, false
	}
	switch s[i] {
	case '[': // CSI: parameter bytes 0x30-0x3F, intermediates 0x20-0x2F, final 0x40-0x7E
		i++
		for i < len(s) && s[i] >= 0x20 && s[i] <= 0x3f {
			i++
		}
		if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7e {
			return i + 1, true
		}
		return 0, false
	case ']': // OSC: terminated by BEL or ST (ESC backslash)
		i++
		for i < len(s) {
			if s[i] == 0x07 {
				return i + 1, true
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2, true
			}
			i++
		}
		return 0, false
	default: // two-byte escape: ESC plus one byte
		return i + 1, true
	}
}

func widthOf(s string) int {
	if w := plainWidth(s); w >= 0 {
		return w
	}
	return lipgloss.Width(s)
}

// shorten truncates s to n visible cells with an ellipsis. Cells, not
// runes: CJK and other wide glyphs occupy two columns, and cutting by rune
// count would let the result render wider than n and break panel alignment.
// The cut also only ever lands between grapheme clusters (user-perceived
// characters): slicing a flag emoji into lone regional indicators or an
// accented letter off its combining mark would print garbage in the pane.
func shorten(s string, n int) string {
	if n <= 0 {
		return ""
	}
	// Probe first: most inputs fit, and the probe is one grapheme walk
	// without building output. The builder below only runs on overflow,
	// where its cost is unavoidable. A single-scan variant built on every
	// call and raised per-frame allocs (8316 to 8682 at 200x50).
	if widthOf(s) <= n {
		return s
	}
	var b strings.Builder
	w := 0
	state := -1
	for rest := s; rest != ""; {
		var cluster string
		cluster, rest, _, state = uniseg.FirstGraphemeClusterInString(rest, state)
		rw := lipgloss.Width(cluster)
		if w+rw > n-1 { // reserve one cell for the ellipsis
			break
		}
		b.WriteString(cluster)
		w += rw
	}
	b.WriteRune('…')
	return b.String()
}

// clip cuts a rendered (possibly styled) line to w visible cells, appending
// an ellipsis when it has to. Styling is dropped across the whole line, since
// strip removes escape sequences; good enough for our own strings.
func clip(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if widthOf(s) <= w {
		return s
	}
	return shorten(strip(s), w)
}

// strip is core.SanitizeText under a shorter name, for the untrusted strings
// (engine-supplied names, agent events) that must never reach the raw
// terminal. Width clipping is clip's job, and happens after this.
func strip(s string) string { return core.SanitizeText(s) }

func padTo(s string, w int) string {
	if gap := w - widthOf(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

// padStart right-aligns s within w visible cells.
func padStart(s string, w int) string {
	if gap := w - widthOf(s); gap > 0 {
		return strings.Repeat(" ", gap) + s
	}
	return s
}

// joinSpread places a left segment row and right segment on one padded line.
func joinSpread(left, right string, width int) string {
	lw := widthOf(left)
	rw := widthOf(right)
	gap := max(width-lw-rw, 1)
	return left + strings.Repeat(" ", gap) + right
}

// joinSpreadLeft packs segments left-to-right up to w visible cells and
// reports how many it kept. It sheds from the right, so the caller controls
// priority by the order it builds segs in.
//
// The kept count is returned so a caller that ends the row with an overflow
// count can drop whole segments from segs, rather than take the packed row
// apart again on the separator it happens to have joined with.
func joinSpreadLeft(segs []string, w int) (string, int) {
	var b strings.Builder
	used := 0
	kept := 0
	for _, s := range segs {
		seg := dim(" │ ") + s
		if kept == 0 {
			seg = s
		}
		sw := widthOf(seg)
		if used+sw > w {
			break
		}
		b.WriteString(seg)
		used += sw
		kept++
	}
	return b.String(), kept
}

func dim(s string) string { return styleDim.Render(s) }

// joinBlocks concatenates blocks vertically, padding every row of the result
// to the widest row across all of them. It is lipgloss.JoinVertical measured
// with widthOf: that call was a fifth of a frame's CPU, spent splitting each
// block and re-measuring rows this package had already cut to a known width.
//
// Each row is measured once, in the pass that splits the blocks, and the
// widths are kept for the copy. Measuring again while padding walked the
// frame's whole byte volume a second time per frame.
func joinBlocks(blocks ...string) string {
	if len(blocks) == 0 {
		return ""
	}
	if len(blocks) == 1 {
		return blocks[0] // a lone block is already the join, trailing rows and all
	}
	rows := make([][]string, len(blocks))
	widths := make([][]int, len(blocks))
	widest := 0
	total := 0
	for i, b := range blocks {
		rows[i] = strings.Split(b, "\n")
		total += len(rows[i])
		ws := make([]int, len(rows[i]))
		for j, ln := range rows[i] {
			w := widthOf(ln)
			ws[j] = w
			if w > widest {
				widest = w
			}
		}
		widths[i] = ws
	}
	var out strings.Builder
	out.Grow(total * (widest + 1))
	for i, lines := range rows {
		for j, ln := range lines {
			out.WriteString(ln)
			if gap := widest - widths[i][j]; gap > 0 {
				out.WriteString(strings.Repeat(" ", gap))
			}
			out.WriteByte('\n')
		}
	}
	// The trailing newline JoinVertical does not leave behind.
	s := out.String()
	return strings.TrimSuffix(s, "\n")
}

// joinAcross is joinBlocks' sibling for side-by-side blocks: every block is
// padded to its own width and the rows of all of them are zipped, with the
// shorter blocks blank-filled at the bottom (lipgloss.Top). The width each
// block is padded to is recorded while it is walked for the row count, so a
// block list is measured once and copied once.
//
// The blocks here are panels of a known inner width, so this is the same
// result lipgloss.JoinHorizontal produces without its per-row width walk.
func joinAcross(blocks ...string) string {
	if len(blocks) == 0 {
		return ""
	}
	if len(blocks) == 1 {
		return blocks[0]
	}
	rows := make([][]string, len(blocks))
	widths := make([]int, len(blocks))
	height := 0
	for i, b := range blocks {
		rows[i] = strings.Split(b, "\n")
		for _, ln := range rows[i] {
			if w := widthOf(ln); w > widths[i] {
				widths[i] = w
			}
		}
		height = max(height, len(rows[i]))
	}
	var out strings.Builder
	out.Grow(height * (len(blocks) + 1))
	for row := range height {
		if row > 0 {
			out.WriteByte('\n')
		}
		for i, lines := range rows {
			if row >= len(lines) {
				out.WriteString(strings.Repeat(" ", widths[i]))
				continue
			}
			out.WriteString(lines[row])
			if gap := widths[i] - widthOf(lines[row]); gap > 0 {
				out.WriteString(strings.Repeat(" ", gap))
			}
		}
	}
	return out.String()
}
