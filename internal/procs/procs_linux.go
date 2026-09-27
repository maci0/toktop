//go:build linux

package procs

import (
	"os"
	"strconv"
	"strings"

	"github.com/maci0/toktop/internal/core"
)

func init() {
	platformList = listLinux
}

// listLinux walks /proc: everything from plain files, zero subprocesses.
func listLinux() ([]raw, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	var out []raw
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue // not a process dir
		}
		base := procPath(e.Name())
		cmdlineB, err := os.ReadFile(base + "/cmdline")
		if err != nil {
			continue // vanished or kernel thread
		}
		args := strings.Split(strings.TrimRight(string(cmdlineB), "\x00"), "\x00")
		if len(args) == 0 || args[0] == "" {
			continue
		}

		r := raw{pid: pid, name: baseName(args[0]), args: args}
		annotate(&r)
		if r.engine == "" && r.port == 0 {
			continue // skip /proc/PID/stat for firefox and friends
		}

		// One stat read yields both CPU ticks and RSS; a second read of
		// status would double the per-PID syscalls on every poll.
		if stat, err := os.ReadFile(base + "/stat"); err == nil {
			r.ticks, r.rss = procStatCPUAndRSS(string(stat))
		}
		out = append(out, r)
	}
	return out, nil
}

// procStatCPUAndRSS sums utime+stime jiffies (fields 14,15) and reads the
// resident set in pages (field 24) from one /proc/PID/stat body, respecting
// the comm parens (a comm may contain spaces).
func procStatCPUAndRSS(stat string) (ticks uint64, rssBytes uint64) {
	open := strings.LastIndexByte(stat, '(')
	closeP := strings.LastIndexByte(stat, ')')
	if open < 0 || closeP < 0 || closeP+2 > len(stat) {
		return 0, 0
	}
	// Field 3 onward, walked in place: strings.Fields would allocate a
	// slice of ~50 strings per process per poll.
	rest := stat[closeP+2:]
	var utime, stime, pages uint64
	field := 3
	i := 0
	for field <= 24 && i < len(rest) {
		for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t') {
			i++
		}
		if i >= len(rest) {
			break
		}
		start := i
		for i < len(rest) && rest[i] != ' ' && rest[i] != '\t' && rest[i] != '\n' {
			i++
		}
		switch field {
		case 14:
			utime, _ = strconv.ParseUint(rest[start:i], 10, 64)
		case 15:
			stime, _ = strconv.ParseUint(rest[start:i], 10, 64)
		case 24:
			pages, _ = strconv.ParseUint(rest[start:i], 10, 64)
			return core.SatAddU64(utime, stime), core.MulSatU64(pages, uint64(os.Getpagesize()))
		}
		field++
	}
	return core.SatAddU64(utime, stime), core.MulSatU64(pages, uint64(os.Getpagesize()))
}
