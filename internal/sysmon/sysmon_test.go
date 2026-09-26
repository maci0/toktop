package sysmon

import (
	"math"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
)

const meminfoFixture = `MemTotal:       32872572 kB
MemFree:         4123001 kB
MemAvailable:   18345216 kB
Buffers:         1204992 kB
Cached:          8213455 kB
SwapTotal:       8388604 kB
SwapFree:        6291456 kB
HugePages_Total:       0
`

func TestParseMeminfo(t *testing.T) {
	var s core.SysSample
	ParseMeminfo([]byte(meminfoFixture), &s)

	if want := uint64(32872572) << 10; s.MemTotal != want {
		t.Errorf("MemTotal = %d, want %d", s.MemTotal, want)
	}
	wantUsed := (uint64(32872572) - 18345216) << 10
	if s.MemUsed != wantUsed {
		t.Errorf("MemUsed = %d, want %d", s.MemUsed, wantUsed)
	}
	if s.SwapTotal != uint64(8388604)<<10 {
		t.Errorf("SwapTotal = %d", s.SwapTotal)
	}
	if want := (uint64(8388604) - 6291456) << 10; s.SwapUsed != want {
		t.Errorf("SwapUsed = %d, want %d", s.SwapUsed, want)
	}
}

func TestParseMeminfoGarbage(t *testing.T) {
	var s core.SysSample
	ParseMeminfo([]byte("nonsense\n:\nMemTotal: abc kB\n"), &s)
	if s.MemTotal != 0 || s.MemUsed != 0 {
		t.Fatalf("garbage parsed as data: %+v", s)
	}
}

// Some ballooning/virtualized kernels report MemAvailable above MemTotal;
// the ssh vitals path can feed such text in from any remote. The used count
// must saturate at zero instead of wrapping to ~2^64 bytes.
func TestParseMeminfoAvailableExceedsTotal(t *testing.T) {
	var s core.SysSample
	ParseMeminfo([]byte("MemTotal:       1000000 kB\nMemAvailable:   1200000 kB\nSwapTotal:       500000 kB\nSwapFree:        600000 kB\n"), &s)
	if s.MemUsed != 0 {
		t.Errorf("MemUsed = %d, want 0 (no wrap)", s.MemUsed)
	}
	if s.SwapUsed != 0 {
		t.Errorf("SwapUsed = %d, want 0 (no wrap)", s.SwapUsed)
	}
}

// A KiB count at or past 2^54 would shift into a wrong small byte count;
// the ssh vitals path feeds parser text from another host, so it must
// saturate instead.
func TestParseMeminfoHugeValuesSaturate(t *testing.T) {
	var s core.SysSample
	ParseMeminfo([]byte("MemTotal: 18446744073709551615 kB\nMemAvailable: 1 kB\nSwapTotal: 20000000000000000 kB\nSwapFree: 0 kB\n"), &s)
	if want := ^uint64(0); s.MemTotal != want {
		t.Errorf("MemTotal = %d, want saturated max", s.MemTotal)
	}
	if s.MemUsed == 0 {
		t.Error("MemUsed saturated away with MemTotal")
	}
	if want := ^uint64(0); s.SwapTotal != want {
		t.Errorf("SwapTotal = %d, want saturated max", s.SwapTotal)
	}
}

func TestParseLoadavg(t *testing.T) {
	l1, l5, l15 := ParseLoadavg("4.98 2.71 1.03 3/5123 4242")
	if l1 != 4.98 || l5 != 2.71 || l15 != 1.03 {
		t.Errorf("got %.2f %.2f %.2f", l1, l5, l15)
	}
	if a, b, c := ParseLoadavg(""); a != 0 || b != 0 || c != 0 {
		t.Error("empty loadavg must zero")
	}
}

func TestParseLoadavgRejectsNonFinite(t *testing.T) {
	if a, b, c := ParseLoadavg("+Inf 0.5 nan"); a != 0 || b != 0.5 || c != 0 {
		t.Errorf("non-finite loadavg = %v %v %v", a, b, c)
	}
	if a, b, c := ParseLoadavg("-1 2.0 Infinity"); a != 0 || b != 2.0 || c != 0 {
		t.Errorf("negative/Inf loadavg = %v %v %v", a, b, c)
	}
}

func TestDurationFromClock(t *testing.T) {
	if got := durationFromClock(2, 500_000_000); got != 2*time.Second+500*time.Millisecond {
		t.Errorf("2.5s = %v", got)
	}
	if durationFromClock(-1, 0) != 0 || durationFromClock(0, 0) != 0 || durationFromClock(0, -1) != 0 {
		t.Error("non-positive clock readings must be zero")
	}
	const maxSec = int64(math.MaxInt64 / int64(time.Second))
	if got := durationFromClock(maxSec, 0); got != time.Duration(math.MaxInt64) {
		t.Errorf("huge clock = %v, want saturation", got)
	}
	if got := durationFromClock(maxSec-1, 2_000_000_000); got != time.Duration(math.MaxInt64) {
		t.Errorf("overflow clock = %v, want saturation", got)
	}
	const capKib = ^uint64(0) >> 10
	if got := kibBytes(capKib); got != capKib<<10 {
		t.Errorf("kibBytes(capKib) = %d, want %d", got, capKib<<10)
	}
}

func TestParseUptimeSecs(t *testing.T) {
	if got := ParseUptimeSecs("183729.42"); got != time.Duration(183729.42*float64(time.Second)) {
		t.Errorf("uptime = %v", got)
	}
	if ParseUptimeSecs("nan") != 0 || ParseUptimeSecs("+Inf") != 0 || ParseUptimeSecs("-1") != 0 {
		t.Error("non-finite or negative uptime must be zero")
	}
	// 1e12 seconds is ~31700 years: the float*ns product overflows
	// time.Duration. Saturate rather than convert out of range.
	if got := ParseUptimeSecs("1000000000000"); got != time.Duration(math.MaxInt64) {
		t.Errorf("huge uptime = %v, want saturation", got)
	}
}

func TestSplitSizeTokenSaturates(t *testing.T) {
	if got := splitSizeToken("512.00M"); got != 512<<20 {
		t.Errorf("512M = %d", got)
	}
	if got := splitSizeToken("2G"); got != 2<<30 {
		t.Errorf("2G = %d", got)
	}
	if splitSizeToken("infG") != 0 || splitSizeToken("nanM") != 0 {
		t.Error("non-finite size tokens must be zero")
	}
	if got := splitSizeToken("1e300G"); got != math.MaxUint64 {
		t.Errorf("1e300G = %d, want saturation", got)
	}
}

func TestParseSwapUsage(t *testing.T) {
	total, used := parseSwapUsage("total = 2048.00M used = 512.00M free = 1536.00M")
	if total != 2048<<20 || used != 512<<20 {
		t.Errorf("swap = %d/%d", used, total)
	}
}

func TestSample(t *testing.T) {
	s := Sample()
	if s.Drivers == nil {
		t.Fatal("Sample() must initialize Drivers map")
	}
	for _, g := range s.GPUs {
		if g.Vendor != "" && g.Driver != "" {
			if s.Drivers[g.Vendor] == "" {
				t.Errorf("Driver for vendor %q not copied to Drivers map", g.Vendor)
			}
		}
	}
}
