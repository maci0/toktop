// Package sysmon samples host vitals: RAM, swap, load, temperatures, CPU
// model, OS and kernel identity, driver versions, GPUs and NPUs. Platform
// implementations live in sysmon_<goos>.go files selected by build tags.
package sysmon

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/gpu"
)

// gpuBudget bounds the whole vendor-tool sweep per poll cycle.
const gpuBudget = 3 * time.Second

// Hooks implemented by each platform file.
var (
	platformMemory   func(*core.SysSample)
	platformLoad     func(*core.SysSample)
	platformTemps    func() []core.TempReading
	platformCPUModel func() string
	platformHost     func(*core.SysSample) // os name, kernel, uptime, drivers
)

// Sample collects a best-effort snapshot of host vitals; missing sources are
// simply absent from the result. Never returns nil.
func Sample() core.SysSample {
	var s core.SysSample
	if platformMemory != nil {
		platformMemory(&s)
	}
	if platformLoad != nil {
		platformLoad(&s)
	}
	if platformTemps != nil {
		s.Temps = platformTemps()
	}
	if platformHost != nil {
		platformHost(&s)
	}
	ctx, cancel := context.WithTimeout(context.Background(), gpuBudget)
	defer cancel()
	s.GPUs = gpu.Sample(ctx)
	if s.Drivers == nil {
		s.Drivers = map[string]string{}
	}
	for _, g := range s.GPUs { // vendor tools often know their own driver
		if g.Vendor == "nvidia" && g.Driver != "" && s.Drivers["nvidia"] == "" {
			s.Drivers["nvidia"] = g.Driver
		}
	}
	if platformCPUModel != nil {
		s.CPUModel = platformCPUModel()
	}
	return s
}

// ParseMeminfo fills memory fields from the Linux /proc/meminfo format.
func ParseMeminfo(b []byte, s *core.SysSample) {
	vals := map[string]uint64{}
	for line := range strings.SplitSeq(string(b), "\n") {
		k, v, ok := cutMeminfoLine(line)
		if ok {
			vals[k] = v
		}
	}
	total := vals["MemTotal"]
	s.MemTotal = kibBytes(total)
	s.MemUsed = kibBytes(satSub(total, vals["MemAvailable"]))
	s.SwapTotal = kibBytes(vals["SwapTotal"])
	s.SwapUsed = kibBytes(satSub(vals["SwapTotal"], vals["SwapFree"]))
}

// kibBytes converts a meminfo KiB count to bytes. The remote vitals path
// feeds this parser text from another host, so an absurd magnitude must
// saturate rather than wrap to a small byte count in the shift.
func kibBytes(kib uint64) uint64 {
	const capKib = ^uint64(0) >> 10
	if kib > capKib {
		return ^uint64(0)
	}
	return kib << 10
}

// satSub subtracts saturating at zero: some ballooning/virtualized kernels
// transiently report MemAvailable above MemTotal, and the remote vitals path
// feeds this parser text from another host, so the difference must never
// wrap to a near-2^64 byte count.
func satSub(a, b uint64) uint64 {
	if b >= a {
		return 0
	}
	return a - b
}

func cutMeminfoLine(line string) (string, uint64, bool) {
	k, rest, ok := strings.Cut(line, ":")
	if !ok {
		return "", 0, false
	}
	f := strings.Fields(rest)
	if len(f) == 0 {
		return "", 0, false
	}
	v, err := strconv.ParseUint(f[0], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return strings.TrimSpace(k), v, true
}

// ParseLoadavg reads "1.5 0.7 0.3 extra..." into three load averages.
// The remote vitals path feeds this parser text from another host, so
// NaN, ±Inf and negatives (ParseFloat accepts all of them) collapse to
// zero rather than reaching the load readout.
func ParseLoadavg(s string) (l1, l5, l15 float64) {
	f := strings.Fields(s)
	at := func(i int) float64 {
		if i >= len(f) {
			return 0
		}
		v, err := strconv.ParseFloat(f[i], 64)
		if err != nil || !(v >= 0) || math.IsInf(v, 0) {
			return 0
		}
		return v
	}
	return at(0), at(1), at(2)
}

// ParseUptimeSecs converts a /proc/uptime first field (seconds, possibly
// fractional) into a duration. The remote vitals path feeds this parser
// text from another host: a non-finite or out-of-range value must not
// convert to a wrapped or negative Duration (time.Duration(+Inf) is
// implementation-defined, often MinInt64 on amd64).
func ParseUptimeSecs(s string) time.Duration {
	secs, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return durationFromSecs(secs)
}

func durationFromSecs(secs float64) time.Duration {
	if !(secs > 0) || math.IsInf(secs, 0) {
		return 0
	}
	const maxSecs = float64(math.MaxInt64 / int64(time.Second))
	if secs >= maxSecs {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(secs * float64(time.Second))
}

// durationFromClock converts a clock_gettime-style (sec, nsec) pair into a
// Duration. A stepped or set-back wall clock can make boot-relative math
// negative; saturate at zero and MaxInt64 the way durationFromSecs does for
// /proc/uptime text, so a bad reading never renders as a wrapped uptime.
func durationFromClock(sec, nsec int64) time.Duration {
	if sec < 0 || (sec == 0 && nsec <= 0) {
		return 0
	}
	if nsec < 0 {
		nsec = 0
	}
	const maxSec = int64(math.MaxInt64 / int64(time.Second))
	if sec >= maxSec {
		return time.Duration(math.MaxInt64)
	}
	d := time.Duration(sec) * time.Second
	rem := time.Duration(math.MaxInt64) - d
	if time.Duration(nsec) >= rem {
		return time.Duration(math.MaxInt64)
	}
	return d + time.Duration(nsec)
}

// parseSwapUsage decodes a vm.swapusage string into bytes:
// "total = 2048.00M used = 512.00M free = 1536.00M".
func parseSwapUsage(s string) (total, used uint64) {
	last := ""
	for tok := range strings.FieldsSeq(strings.ReplaceAll(s, "=", " ")) {
		switch strings.ToLower(tok) {
		case "total":
			last = "total"
			continue
		case "used":
			last = "used"
			continue
		case "free":
			last = ""
			continue
		}
		v := splitSizeToken(tok)
		if v == 0 {
			continue
		}
		switch last {
		case "total":
			total = v
		case "used":
			used = v
		}
	}
	return total, used
}

// splitSizeToken splits "512.00M" into bytes. Darwin vm.swapusage is
// kernel-produced, but the same parser is reachable from tests and must
// saturate on absurd magnitudes the way kibBytes does for meminfo: a
// float product that overflows to +Inf converts to a platform-defined
// integer, often zero, which would read as "no swap" instead of "full".
func splitSizeToken(tok string) uint64 {
	if len(tok) < 2 {
		return 0
	}
	unit := strings.ToUpper(string(tok[len(tok)-1]))
	num, err := strconv.ParseFloat(tok[:len(tok)-1], 64)
	if err != nil || !(num > 0) || math.IsInf(num, 0) {
		return 0
	}
	var mult float64
	switch unit {
	case "G":
		mult = 1 << 30
	case "M":
		mult = 1 << 20
	case "K":
		mult = 1 << 10
	default:
		return 0
	}
	scaled := num * mult
	if math.IsInf(scaled, 0) || scaled >= float64(math.MaxUint64) {
		return math.MaxUint64
	}
	return uint64(scaled)
}
