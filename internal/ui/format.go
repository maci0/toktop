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

func fmtRate(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "0.0"
	}
	k := v / 1000
	switch {
	case v >= 10000 || k >= unitRound:
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
	case n >= 1000000 || k >= unitRound:
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
// Printable ASCII is one cell per byte; box drawing (U+2500..U+257F) and
// braille dots (U+2800..U+28FF) are one cell per rune, never wide, never
// combining, so a rune count is exact for those too. Everything else falls
// back to Width, which knows about wide runes and combining marks.
func plainWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < 0x20 || c >= 0x7f {
			r, size := utf8.DecodeRuneInString(s[i:])
			if !((r >= 0x2500 && r <= 0x257f) || (r >= 0x2800 && r <= 0x28ff)) {
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

// strip is core.SanitizeText at render width: untrusted strings
// (engine-supplied names, agent events) must never reach the raw terminal.
// Kept as a named alias because clip's cut path and the header's engine
// count both need it and the name reads shorter at those call sites.
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

// joinSpreadLeft packs segments left-to-right up to w visible cells.
func joinSpreadLeft(segs []string, w int) string {
	var b strings.Builder
	used := 0
	for i, s := range segs {
		seg := dim(" │ ") + s
		if i == 0 {
			seg = s
		}
		sw := widthOf(seg)
		if used+sw > w {
			break
		}
		b.WriteString(seg)
		used += sw
	}
	return b.String()
}

func dim(s string) string { return styleDim.Render(s) }
