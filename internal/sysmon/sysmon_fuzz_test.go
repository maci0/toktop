package sysmon

import (
	"math"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/maci0/toktop/internal/core"
)

// FuzzParseVitalsText throws arbitrary bytes at the three parsers the remote
// vitals path hands text from another host to (internal/remote/stats.go).
// The values they produce land straight in the host readout, so a hostile
// remote must not be able to wrap a subtraction into a near-2^64 byte count,
// shift an absurd magnitude past the KiB-to-byte cap, or convert a non-finite
// reading into a negative Duration.
func FuzzParseVitalsText(f *testing.F) {
	for _, seed := range []string{
		meminfoFixture,
		"",
		"\n",
		"MemTotal:       32872572 kB\nMemAvailable:   18345216 kB\nSwapTotal: 8388604 kB\nSwapFree: 6291456 kB",
		"MemTotal: 18446744073709551615 kB\nMemAvailable: 1 kB",
		"MemTotal: 1 kB\nMemAvailable: 18446744073709551615 kB",
		"MemTotal:\nMemAvailable:   \n: 1\n::\nMemTotal: 0x10 kB",
		"MemTotal: 18446744073709551616 kB\nMemTotal: -1 kB\nMemTotal: +3 kB",
		"MemTotal: 1e999 kB\nMemTotal: 0777 kB\nMemTotal: 1_0 kB",
		"MemTotal: 512\tmB\nMemTotal:0\nMemTotal:   12   \n",
		"1.5 0.7 0.3 3/5123 4242",
		"nan nan nan",
		"+Inf -Inf Infinity 1.0",
		"-1 -2 -3",
		"1e300 0 0",
		"0.0000000000000000000001 0 0",
		"183729.42 17931.13 2",
		"nan",
		"1e999",
		"1e999999999999999999999",
		"1.7976931348623157e308 0 0",
		"0x1p-2 0 0",
		"1_000 0 0",
		"  \t\n  ",
		"\x00\x00\x00",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data string) {
		var s core.SysSample
		ParseMeminfo([]byte(data), &s)
		if s.MemUsed > s.MemTotal {
			t.Fatalf("mem used %d exceeds total %d from %q", s.MemUsed, s.MemTotal, data)
		}
		if s.SwapUsed > s.SwapTotal {
			t.Fatalf("swap used %d exceeds total %d from %q", s.SwapUsed, s.SwapTotal, data)
		}
		// kibBytes shifts or saturates, never wraps, so every byte count it
		// returns is a whole number of KiB or the maximum. A wrapped shift
		// would show up here as a small, plausible-looking value.
		for name, v := range map[string]uint64{
			"MemTotal": s.MemTotal, "MemUsed": s.MemUsed,
			"SwapTotal": s.SwapTotal, "SwapUsed": s.SwapUsed,
		} {
			if v%1024 != 0 && v != math.MaxUint64 {
				t.Fatalf("%s = %d from %q, want a whole KiB count or the saturated maximum", name, v, data)
			}
		}

		l1, l5, l15 := ParseLoadavg(data)
		for i, v := range []float64{l1, l5, l15} {
			if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("load[%d] = %v from %q, want a finite non-negative average", i, v, data)
			}
		}

		up := ParseUptimeSecs(data)
		if up < 0 {
			t.Fatalf("uptime %v from %q, want a non-negative duration", up, data)
		}
	})
}

// FuzzSplitSizeToken covers the Darwin swap string decoder, which is
// reachable from a remote vitals dump on a macOS host. The unit is one rune,
// so a multi-byte trailing rune must not hand ParseFloat a half-rune or
// FoldASCII an invalid byte, and an absurd magnitude must saturate rather than
// overflow to a small byte count.
func FuzzSplitSizeToken(f *testing.F) {
	for _, seed := range []string{
		"total = 2048.00M used = 512.00M free = 1536.00M",
		"total=1G used=1K free=0",
		"",
		"total = 1.5T used = 2P free = 3E",
		"total = 1.7976931348623157e308M used = 1e300G",
		"total = -1M used = +2M free = 0M",
		"total = nanM used = infM",
		"total = 1\u00b5M used = 1\u212a",
		"total = 1M\u00b5 used = 1",
		"total = .5M used = 1.M",
		"total = 1M4 used = 4M",
		"total",
		"M",
		"\xffM",
		"total = 1\x00M used = 2\x00M",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		total, used := parseSwapUsage(s)
		// A re-parse of the same text must land on the same reading: a
		// decoder that carries state between calls would show up here as
		// a value changing without the input changing.
		if gotTotal, gotUsed := parseSwapUsage(s); gotTotal != total || gotUsed != used {
			t.Fatalf("parseSwapUsage(%q) is not deterministic: %d/%d then %d/%d", s, total, used, gotTotal, gotUsed)
		}
		// parseSwapUsage splits on "=" before it decodes, so the decoder's
		// tokens come from the rewritten text, not the raw fields.
		for _, tok := range strings.Fields(strings.ReplaceAll(s, "=", " ")) {
			last, size := utf8.DecodeLastRuneInString(tok)
			if size == 0 || size == len(tok) || last <= unicode.MaxASCII {
				continue // an ASCII unit, or no unit at all
			}
			// A multi-byte trailing rune is not a unit, so a bare token
			// carrying one must contribute nothing to either field.
			if gotTotal, gotUsed := parseSwapUsage("total = " + tok + " used = 0"); gotTotal != 0 || gotUsed != 0 {
				t.Fatalf("token %q ends in a non-ASCII rune but decoded to total %d used %d", tok, gotTotal, gotUsed)
			}
		}
	})
}

// FuzzDurationFromClock pins the boot-relative clock conversion: a wall clock
// stepped backwards must saturate at zero rather than wrap to a huge
// negative-free nonsense uptime.
func FuzzDurationFromClock(f *testing.F) {
	f.Add(int64(0), int64(0))
	f.Add(int64(1), int64(999999999))
	f.Add(int64(-1), int64(-1))
	f.Add(int64(math.MaxInt64), int64(math.MaxInt64))
	f.Add(int64(math.MaxInt64), int64(1))
	f.Add(int64(math.MaxInt64/int64(time.Second)), int64(1))
	f.Add(int64(0), int64(1))
	f.Add(int64(1), int64(-1))
	f.Add(int64(math.MinInt64), int64(math.MinInt64))
	f.Fuzz(func(t *testing.T, sec, nsec int64) {
		d := durationFromClock(sec, nsec)
		if d < 0 {
			t.Fatalf("durationFromClock(%d, %d) = %v, want a non-negative duration", sec, nsec, d)
		}
	})
}
