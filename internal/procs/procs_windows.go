//go:build windows

package procs

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
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
}

// listWindows queries Win32_Process via PowerShell CIM once per poll.
// There is no unprivileged pure-Go window into the NT process table with
// command lines; CIM is the documented interface and needs no vendor libs.
func listWindows() ([]raw, error) {
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	// PowerShell 5.1 otherwise writes redirected stdout in the OEM code
	// page, which json.Unmarshal rejects for command lines that are not ASCII.
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command",
		`$OutputEncoding = [Console]::OutputEncoding = [System.Text.Encoding]::UTF8; Get-CimInstance Win32_Process | Select-Object ProcessId,Name,CommandLine,WorkingSetSize | ConvertTo-Json -Compress`)
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
		name := p.Name
		r := raw{
			pid:  p.ProcessID,
			name: name,
			args: append([]string{name}, args...),
			rss:  p.WorkingSetSize,
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

func init() {
	// CIM enumeration costs seconds; serve a cached list between refreshes.
	defaultSamplerRefresh = 3 * time.Second
}
