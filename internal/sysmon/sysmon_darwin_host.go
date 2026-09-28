//go:build darwin

package sysmon

import (
	"encoding/binary"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/maci0/toktop/internal/core"
)

func init() {
	platformHost = hostInfoDarwin
}

// hostInfoDarwin uses only sysctls: product version, kernel release and boot
// time. No powermetrics/root required.
func hostInfoDarwin(s *core.SysSample) {
	if v, err := unix.Sysctl("kern.osproductversion"); err == nil && v != "" {
		s.OsName = "macOS " + v
	}
	var un unix.Utsname
	if err := unix.Uname(&un); err == nil {
		s.Kernel = utsField(un.Release[:])
	}
	s.HostUptime = boottimeUptime()
	s.NPUs = appleNPUs()
}

// appleNPUs reports the Apple Neural Engine on Apple Silicon, labelled with
// the chip generation parsed from the CPU brand string ("Apple M3 Pro" ->
// "ANE M3 Pro"). The ANE has no public utilization interface without root
// (powermetrics), so this is a presence indicator.
func appleNPUs() []string {
	chip, err := unix.Sysctl("machdep.cpu.brand_string")
	if err != nil || chip == "" {
		return nil
	}
	chip = strings.TrimSpace(chip)
	if !strings.HasPrefix(chip, "Apple") {
		return nil // Intel Macs have no NPU
	}
	label := strings.TrimSpace(strings.TrimPrefix(chip, "Apple"))
	return []string{"ANE " + label}
}

func boottimeUptime() time.Duration {
	// CLOCK_MONOTONIC is time since boot including sleep and is immune to
	// NTP steps and `date` changes. kern.boottime minus time.Now() is a
	// wall-clock subtraction: a host whose clock jumps forward at NTP
	// sync (common just after boot) reports a huge uptime, and one whose
	// clock is set back reports a negative duration that fmtDur would
	// print as idle-looking garbage. Linux reads /proc/uptime and Windows
	// GetTickCount64; this is the Darwin equivalent.
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err == nil {
		if d := durationFromClock(ts.Sec, ts.Nsec); d > 0 {
			return d
		}
	}
	return boottimeWallFallback()
}

// boottimeWallFallback is the pre-CLOCK_MONOTONIC path: kern.boottime is a
// wall-clock timeval, so a stepped clock still moves the reading. Used when
// clock_gettime errors or returns a non-positive duration. Negative results
// collapse to zero.
func boottimeWallFallback() time.Duration {
	b, err := unix.SysctlRaw("kern.boottime")
	if err != nil || len(b) < 8 {
		return 0
	}
	sec := int64(binary.LittleEndian.Uint64(b[:8]))
	if sec <= 0 {
		return 0
	}
	// The injected clock, not time.Since: uptime is "now minus boot", and on a
	// run replaying on a pinned timeline the subtraction against the wall
	// clock puts the one host field the frame carries from a real host on a
	// different axis from every timestamp beside it.
	d := instant().Sub(time.Unix(sec, 0))
	if d < 0 {
		return 0
	}
	return d
}
