//go:build windows

package procs

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"sync"
	"time"
)

func init() {
	platformList = listWindows
}

type cimProc struct {
	ProcessID      int    `json:"ProcessId"`
	Name           string `json:"Name"`
	CommandLine    string `json:"CommandLine"`
	WorkingSetSize uint64 `json:"WorkingSetSize"`
	KernelModeTime uint64 `json:"KernelModeTime"`
	UserModeTime   uint64 `json:"UserModeTime"`
}

// windowsTicksPerSecond converts the 100-nanosecond units Win32_Process reports
// CPU time in into the jiffies clkTck counts, so the delta math in procs.go
// works the same on every platform.
const windowsTicksPerJiffy = 1e4 / clkTck

// windowsCPUTicks is a process's cumulative kernel plus user time in jiffies.
// Without it every process reports 0% CPU on windows while linux and darwin
// report the real figure, which the engine panels print per process.
func windowsCPUTicks(kernel, user uint64) uint64 {
	return (kernel + user) / windowsTicksPerJiffy
}

// windowsShell is the PowerShell CIM runs under, resolved once. See pickShell
// for why neither implementation can be named outright.
var windowsShell = sync.OnceValue(func() string { return pickShell(exec.LookPath, "pwsh", "powershell") })

// listWindows queries Win32_Process via PowerShell CIM once per poll.
// There is no unprivileged pure-Go window into the NT process table with
// command lines; CIM is the documented interface and needs no vendor libs.
func listWindows() ([]raw, error) {
	shell := windowsShell()
	if shell == "" {
		return nil, errNoShell
	}
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	// PowerShell 5.1 otherwise writes redirected stdout in the OEM code
	// page, which json.Unmarshal rejects for command lines that are not ASCII.
	// PowerShell 7 is UTF-8 already; the assignment is a no-op there.
	cmd := exec.CommandContext(ctx, shell, "-NoProfile", "-Command",
		`$OutputEncoding = [Console]::OutputEncoding = [System.Text.Encoding]::UTF8; Get-CimInstance Win32_Process | Select-Object ProcessId,Name,CommandLine,WorkingSetSize,KernelModeTime,UserModeTime | ConvertTo-Json -Compress`)
	cmd.WaitDelay = listPipeGrace
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var procs []cimProc
	if json.Unmarshal(out, &procs) != nil {
		var single cimProc
		if json.Unmarshal(out, &single) != nil {
			return nil, errBadJSON
		}
		procs = []cimProc{single}
	}
	list := make([]raw, 0, len(procs))
	for _, p := range procs {
		if p.ProcessID <= 0 {
			continue
		}
		args := splitWindowsArgs(p.CommandLine)
		r := raw{
			pid:   p.ProcessID,
			name:  p.Name,
			args:  append([]string{p.Name}, args...),
			rss:   p.WorkingSetSize,
			ticks: windowsCPUTicks(p.KernelModeTime, p.UserModeTime),
		}
		annotate(&r)
		list = append(list, r)
	}
	return list, nil
}

// splitWindowsArgs does a light quote-aware split of a Windows command line.
func splitWindowsArgs(cmd string) []string {
	var (
		args []string
		cur  strings.Builder
		inQ  bool
	)
	flush := func() {
		if cur.Len() > 0 {
			args = append(args, cur.String())
			cur.Reset()
		}
	}
	for _, r := range cmd {
		switch {
		case r == '"':
			inQ = !inQ
		case r == ' ' && !inQ:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return args
}

var errBadJSON = jsonError{}

type jsonError struct{}

func (jsonError) Error() string { return "unexpected powershell output" }

// errNoShell reports that the image carries neither PowerShell, so there is no
// documented way to read Win32_Process. It names the way out rather than
// surfacing the exec lookup failure, which reads as a missing file.
var errNoShell = noShellError{}

type noShellError struct{}

func (noShellError) Error() string {
	return "neither pwsh nor powershell is installed, so Win32_Process cannot be read"
}

func init() {
	// CIM enumeration costs seconds; serve a cached list between refreshes.
	defaultSamplerRefresh = 3 * time.Second
}
