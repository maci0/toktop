//go:build windows

package sysmon

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/maci0/toktop/internal/core"
)

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

// memoryStatusEx mirrors MEMORYSTATUSEX (memoryapi.h).
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func init() {
	platformMemory = sampleMemoryWindows
	// No loadavg; WMI thermal zones are rarely populated by vendors.
	platformCPUModel = func() string { return os.Getenv("PROCESSOR_IDENTIFIER") }
}

// sampleMemoryWindows uses GlobalMemoryStatusEx, which also reports
// commit-limit page-file accounting used here as swap.
func sampleMemoryWindows(s *core.SysSample) {
	var ms memoryStatusEx
	ms.Length = uint32(unsafe.Sizeof(ms))
	r1, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms)))
	if r1 == 0 {
		// Latched like a Linux procfs read: the host strip shows zero memory
		// for the rest of the run otherwise, which is what an idle machine
		// shows too.
		noteSourceFailure("GlobalMemoryStatusEx", syscallErr("GlobalMemoryStatusEx", err))
		return
	}
	noteSourceOK("GlobalMemoryStatusEx")
	s.MemTotal = ms.TotalPhys
	s.MemUsed = satSub(ms.TotalPhys, ms.AvailPhys)
	s.SwapTotal = ms.TotalPageFile
	s.SwapUsed = satSub(ms.TotalPageFile, ms.AvailPageFile)
}

// syscallErr names the entry point a call failed at, which a bare errno does
// not. A LazyProc.Call returns a nil error alongside a zero result when the
// call itself succeeded, so this is only reached on a failure and falls back to
// a reason when the loader supplied none.
func syscallErr(name string, err error) error {
	if err == nil {
		err = windows.ERROR_FUNCTION_FAILED
	}
	return fmt.Errorf("%s: %w", name, err)
}
