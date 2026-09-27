//go:build windows

package sysmon

import (
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/maci0/toktop/internal/core"
)

var (
	ntdll             = windows.NewLazySystemDLL("ntdll.dll")
	procRtlGetVersion = ntdll.NewProc("RtlGetVersion")
	kernel32Tick      = kernel32.NewProc("GetTickCount64")
)

// osVersionInfoW mirrors RTL_OSVERSIONINFOW.
type osVersionInfoW struct {
	DwOSVersionInfoSize uint32
	DwMajorVersion      uint32
	DwMinorVersion      uint32
	DwBuildNumber       uint32
	DwPlatformID        uint32
	SZCSDVersion        [128]uint16
}

func init() {
	platformHost = hostInfoWindows
}

func hostInfoWindows(s *core.SysSample) {
	s.OsName = "Windows " + windowsVersion()
	s.HostUptime = tickUptime()
}

func windowsVersion() string {
	var vi osVersionInfoW
	vi.DwOSVersionInfoSize = uint32(unsafe.Sizeof(vi))
	if r1, _, _ := procRtlGetVersion.Call(uintptr(unsafe.Pointer(&vi))); r1 != 0 {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d", vi.DwMajorVersion, vi.DwMinorVersion, vi.DwBuildNumber)
}

func tickUptime() time.Duration {
	r1, _, _ := kernel32Tick.Call()
	return time.Duration(r1) * time.Millisecond
}
