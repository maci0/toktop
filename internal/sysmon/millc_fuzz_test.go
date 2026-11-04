//go:build linux

package sysmon

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/maci0/toktop/internal/core"
)

// FuzzParseMilliC throws arbitrary bytes at the hwmon thermal sensor
// reader. A sensor file is kernel-written but lives on a mount toktop may
// not own, and its value lands straight in a temperature reading, so a
// hostile or broken file must not panic, accept a non-integer as a
// temperature, or answer differently on a second read. The existing
// FuzzParseVitalsText covers meminfo/loadavg, not the per-sensor integer.
func FuzzParseMilliC(f *testing.F) {
	for _, seed := range []string{
		"45000\n", "0", "-5000\n", "  66000  ", "99999999999999999999",
		"45.5", "45\x00", "0x10", "1_000", "1e3", "", "  ", "\n",
		"45 000", "\xff\xfe", "9223372036854775807", "-9223372036854775808",
		"000123", "+123", "123\r\n", "12\x1b[31m",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		v, ok := parseMilliC(b)
		if ok {
			// Atoi on the trimmed text must round-trip: the accepted
			// value is a plain decimal integer, never a float, hex,
			// or decorated spelling.
			trimmed := strings.TrimSpace(string(b))
			if trimmed == "" {
				t.Fatalf("parseMilliC(%q) accepted empty input", b)
			}
			if v2, ok2 := parseMilliC([]byte(trimmed)); !ok2 || v2 != v {
				t.Fatalf("parseMilliC(%q) = %d, trimmed re-read = %d/%v", b, v, v2, ok2)
			}
		} else if v != 0 {
			t.Fatalf("parseMilliC(%q) rejected but returned %d", b, v)
		}
		if v2, ok2 := parseMilliC(b); ok2 != ok || v2 != v {
			t.Fatalf("parseMilliC(%q) is not deterministic: %d/%v then %d/%v", b, v, ok, v2, ok2)
		}
	})
}

// FuzzSensorLabel drives the hwmon name/label normalizer with arbitrary
// text. The result decides gpu-vs-cpu classification by substring match and
// is rendered in the system panel, so it must be one sanitized, ASCII-folded
// line: no escape, control, or bidi character survives, and a Kelvin sign
// must not satisfy an ASCII needle the producer never wrote.
func FuzzSensorLabel(f *testing.F) {
	for _, seed := range []string{
		"coretemp\n", "amdgpu 1\n", "k10temp\n", "KELVIN\u212A\n", "gpu\n",
		"  edge  \n", "\x1b[31mred\x1b[0m", "café\n", "", "  ", "a\x00b",
		"\u202e spoof", "junction\tTJ\n", strings.Repeat("x", 500),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got := sensorLabel(raw)
		if !utf8.ValidString(got) {
			t.Fatalf("sensorLabel(%q) = %q, want valid UTF-8", raw, got)
		}
		if got != core.SanitizeText(got) {
			t.Fatalf("sensorLabel(%q) = %q, want sanitized text", raw, got)
		}
		if strings.TrimSpace(got) != got {
			t.Fatalf("sensorLabel(%q) = %q, want trimmed", raw, got)
		}
		// The fold is one application: folding again must not change it.
		if again := sensorLabel(got); again != got {
			t.Fatalf("sensorLabel(%q) = %q, refold = %q", raw, got, again)
		}
		if again := sensorLabel(raw); again != got {
			t.Fatalf("sensorLabel(%q) is not deterministic: %q then %q", raw, got, again)
		}
		// A Kelvin sign in the input must not become a 'k' the label
		// matcher reads as a needle; FoldASCII already guarantees this,
		// so a folded output containing 'k' from a Kelvin source would
		// show here as a mismatch with the refolded form.
		if strings.Contains(raw, "\u212A") && strings.Contains(got, "k") {
			// Only a failure if the 'k' came from folding; a literal
			// ASCII k elsewhere in the input is legitimate.
			without := strings.ReplaceAll(raw, "\u212A", "")
			if !strings.Contains(without, "k") && !strings.Contains(without, "K") {
				t.Fatalf("sensorLabel(%q) = %q, Kelvin sign folded to k", raw, got)
			}
		}
	})
}
