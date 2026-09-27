//go:build windows

package procs

import "testing"

// Win32_Process reports CPU time in 100-nanosecond units; the delta math in
// procs.go counts jiffies at clkTck. A process burning half a second of CPU
// must land on 50, not on 0 (which reads as "no CPU at all") and not on
// 5,000,000 (which would read as a runaway).
func TestWindowsCPUTicksConvertsToJiffies(t *testing.T) {
	cases := []struct {
		name         string
		kernel, user uint64
		want         uint64
	}{
		{"idle", 0, 0, 0},
		{"half a second", 0, 5e8, 50},
		{"kernel and user both count", 2e8, 3e8, 50},
		{"sub-jiffy remainder is dropped", 0, 9999, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := windowsCPUTicks(c.kernel, c.user); got != c.want {
				t.Errorf("windowsCPUTicks(%d, %d) = %d, want %d", c.kernel, c.user, got, c.want)
			}
		})
	}
}
