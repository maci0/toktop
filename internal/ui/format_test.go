package ui

import (
	"math"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// Truncation must respect both limits at once: the visible-cell budget, and
// the character itself. A flag emoji is two regional indicators that render
// as one glyph; slicing between them prints half a flag (a lone indicator
// shows as a boxed placeholder). A ZWJ sequence sliced after the joiner
// prints a dangling U+200D. Both appeared when shorten cut per rune.
func TestShortenKeepsCharactersWhole(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
	}{
		{"flags", "\U0001F1E9\U0001F1EA\U0001F1EB\U0001F1F7", 3},
		{"zwj emoji then text", "👩‍💻 writing code", 4},
		{"combining accents", "cafe\u0301 latte", 5},
		{"mixed emoji and cjk", "你好\U0001F1E9\U0001F1EA世界", 6},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shorten(tc.in, tc.n)
			if w := lipgloss.Width(got); w > tc.n {
				t.Errorf("shorten(%q, %d) = %q, renders %d cells, want <= %d", tc.in, tc.n, got, w, tc.n)
			}
			if !strings.HasSuffix(got, "…") {
				t.Fatalf("shorten(%q, %d) = %q, want an ellipsis suffix", tc.in, tc.n, got)
			}
			// Everything before the ellipsis must be whole clusters of the
			// input: no lone regional indicators, no dangling joiners.
			head := strings.TrimSuffix(got, "…")
			if head != "" && !strings.HasPrefix(tc.in, head) {
				t.Fatalf("shorten(%q, %d) = %q: head is not a prefix of the input", tc.in, tc.n, got)
			}
			if ri := strings.IndexRune(head, 0x1F1E9); ri >= 0 {
				// Any regional indicator present must arrive paired with its
				// flag partner, i.e. as part of a two-rune cluster prefix.
				if !utf16PairsBalanced(head) {
					t.Fatalf("shorten(%q, %d) = %q: flag split mid-character", tc.in, tc.n, got)
				}
			}
			if strings.HasSuffix(head, "\u200d") {
				t.Fatalf("shorten(%q, %d) = %q: trailing zero-width joiner", tc.in, tc.n, got)
			}
		})
	}
}

func utf16PairsBalanced(s string) bool {
	count := 0
	for _, r := range s {
		if r >= 0x1F1E6 && r <= 0x1F1FF {
			count++
		}
	}
	return count%2 == 0
}

// Within budget, shortening must be the identity; the ellipsis only appears
// when something was actually cut.
func TestShortenWithinBudgetUnchanged(t *testing.T) {
	for _, s := range []string{"claude", "你好", "\U0001F1E9\U0001F1EA👩‍💻"} {
		if got := shorten(s, 40); got != s {
			t.Errorf("shorten(%q, 40) = %q, want unchanged", s, got)
		}
	}
}

func TestNorm(t *testing.T) {
	if got := norm(50, 100); got != 0.5 {
		t.Errorf("norm(50, 100) = %v, want 0.5", got)
	}
	if got := norm(150, 100); got != 1.0 {
		t.Errorf("norm(150, 100) = %v, want 1.0", got)
	}
	if got := norm(-10, 100); got != 0 {
		t.Errorf("norm(-10, 100) = %v, want 0", got)
	}
	if got := norm(50, 0); got != 0 {
		t.Errorf("norm(50, 0) = %v, want 0", got)
	}
	if got := norm(math.NaN(), 100); got != 0 {
		t.Errorf("norm(NaN, 100) = %v, want 0", got)
	}
	if got := norm(50, math.NaN()); got != 0 {
		t.Errorf("norm(50, NaN) = %v, want 0", got)
	}
}
