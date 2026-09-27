//go:build darwin

package sysmon

import (
	"encoding/binary"
	"math"

	"golang.org/x/sys/unix"

	"github.com/maci0/toktop/internal/core"
)

func init() {
	platformMemory = sampleMemoryDarwin
	platformLoad = sampleLoadDarwin
	// Temps need root powermetrics.
	platformCPUModel = func() string { s, _ := unix.Sysctl("machdep.cpu.brand_string"); return s }
}

// sampleMemoryDarwin derives RAM usage from vm.page_* sysctls. This is the
// same accounting Activity Monitor uses: wired + compressed + app (active +
// inactive) memory, excluding free, speculative and purgeable pages.
func sampleMemoryDarwin(s *core.SysSample) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return
	}
	ps := uint64(0)
	if v, err := unix.SysctlUint32("hw.pagesize"); err == nil {
		ps = uint64(v)
	}
	if ps == 0 {
		ps = 4096
	}
	page := func(name string) uint64 {
		v, err := unix.SysctlUint32(name)
		if err != nil {
			return 0
		}
		return uint64(v)
	}
	s.MemTotal = total
	// pages_compressed is already counted inside active/inactive on current
	// macOS, so the sum can exceed MemTotal. Saturate the byte conversion and
	// the total rather than reporting memory used over total.
	s.MemUsed = min(pagesToBytes(satAdd4(
		page("vm.pages_wired"), page("vm.pages_active"),
		page("vm.pages_inactive"), page("vm.pages_compressed")), ps), total)

	if tu, uu, ok := swapUsage(); ok {
		s.SwapTotal, s.SwapUsed = tu, uu
	}
}

func sampleLoadDarwin(s *core.SysSample) {
	if b, err := unix.SysctlRaw("kern.loadavg"); err == nil {
		s.Load1, s.Load5, s.Load15 = decodeLoadavg(b)
	}
}

// decodeLoadavg parses kern.loadavg: struct loadavg { int32 ldavg[3]; int32 scale }.
func decodeLoadavg(b []byte) (l1, l5, l15 float64) {
	if len(b) < 16 {
		return 0, 0, 0
	}
	scale := binary.LittleEndian.Uint32(b[12:16])
	if scale == 0 {
		return 0, 0, 0
	}
	get := func(off int) float64 {
		v := float64(int32(binary.LittleEndian.Uint32(b[off:off+4]))) / float64(scale)
		// The field is signed, so a kernel that has not filled it in (or has
		// wrapped it) yields a negative load average. Collapse it to zero the
		// way ParseLoadavg does, rather than printing "ld -0.42".
		if !(v >= 0) || math.IsInf(v, 0) {
			return 0
		}
		return v
	}
	return get(0), get(4), get(8)
}

// swapUsage reads macOS's vm.swapusage string.
func swapUsage() (total, used uint64, ok bool) {
	raw, err := unix.Sysctl("vm.swapusage")
	if err != nil {
		return 0, 0, false
	}
	total, used = parseSwapUsage(raw)
	return total, used, total > 0
}
