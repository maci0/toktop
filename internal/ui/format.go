package ui

// Formatting and small layout helpers used by the renderers in ui.go:
// number/duration/byte formatting plus visible-width text cutting.
// Rendering primitives live in spark.go, styles in theme.go.

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/rivo/uniseg"

	"github.com/maci0/toktop/internal/core"
)

func norm(v, maxV float64) float64 {
	if maxV <= 0 {
		return 0
	}
	return v / maxV
}

func fmtRate(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "0.0"
	}
	switch {
	case v >= 10000:
		return fmt.Sprintf("%.0fk", v/1000)
	case v >= 1000:
		return fmt.Sprintf("%.1fk", v/1000)
	case v >= 100:
		return fmt.Sprintf("%.0f", v)
	default:
		return fmt.Sprintf("%.1f", v)
	}
}

func fmtCount(n int64) string {
	switch {
	case n >= 1000000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func fmtMs(ms float64) string {
	if !(ms > 0) || math.IsInf(ms, 0) {
		return "-"
	}
	if ms >= 1000 {
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

func humanBytes(b uint64) string {
	const g = 1 << 30
	if b >= g {
		return fmt.Sprintf("%.1fGiB", float64(b)/g)
	}
	return fmt.Sprintf("%.0fMiB", float64(b)/(1<<20))
}

// humanBytesShort is the compact form used in the system strip.
func humanBytesShort(b uint64) string {
	const m = 1 << 20
	if b >= 10<<30 {
		return fmt.Sprintf("%.0fG", float64(b)/(1<<30))
	}
	if b >= 1<<30 {
		return fmt.Sprintf("%.1fG", float64(b)/(1<<30))
	}
	return fmt.Sprintf("%.0fM", float64(b)/m)
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
	if lipgloss.Width(s) <= n {
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

// clip hard-cuts a rendered (possibly styled) line to w visible cells.
// Styling is dropped past the cut; good enough for our own strings.
func clip(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	return shorten(strip(s), w)
}

// strip removes ANSI escapes and control characters so clip can cut safely.
// Untrusted strings (engine-supplied names, agent events) must never reach
// the raw terminal; SanitizeText is the same guard applied at render time.
func strip(s string) string { return core.SanitizeText(s) }

func padTo(s string, w int) string {
	if gap := w - lipgloss.Width(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

// padStart right-aligns s within w visible cells.
func padStart(s string, w int) string {
	if gap := w - lipgloss.Width(s); gap > 0 {
		return strings.Repeat(" ", gap) + s
	}
	return s
}

// joinSpread places a left segment row and right segment on one padded line.
func joinSpread(left, right string, width int) string {
	lw := lipgloss.Width(left)
	rw := lipgloss.Width(right)
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
		sw := lipgloss.Width(seg)
		if used+sw > w {
			break
		}
		b.WriteString(seg)
		used += sw
	}
	return b.String()
}

func dim(s string) string { return styleDim.Render(s) }

func clampi(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
