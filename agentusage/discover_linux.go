// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build linux

package agentusage

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Discover lists the agent CLIs running on this machine.
//
// On Linux this is a /proc walk: comm, cmdline, and cwd per process, plus
// starttime for matches; no subprocess. Every other user's processes simply
// fail the permission check. A nil result is not an error: /proc unreadable
// and nothing matched both return nil, and callers should surface either as
// no local agents.
func Discover() []Process {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	known := map[string]bool{}
	for _, a := range Agents() {
		known[a] = true
	}

	var out []Process
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join("/proc", strconv.Itoa(pid))

		var comm string
		if name, err := os.ReadFile(filepath.Join(dir, "comm")); err == nil {
			comm = strings.TrimSpace(string(name))
		}

		raw, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		tool := agentName(comm, args, known)
		if tool == "" {
			continue
		}
		cwd, err := os.Readlink(filepath.Join(dir, "cwd"))
		if err != nil {
			continue // exited, or another user's process
		}
		out = append(out, Process{PID: pid, Tool: tool, Dir: cwd, Started: startedAt(pid)})
	}
	return out
}

// linuxClkTck is USER_HZ. The Linux ABI fixes it at 100; /proc/PID/stat
// starttime is in these ticks since boot.
const linuxClkTck = 100

// startedAt reads a process's start time from /proc/PID/stat field 22
// (clock ticks since boot) plus /proc/stat btime. The /proc/PID directory
// mtime is the proc inode's birth, which is first lookup, and a later
// lookup after that inode is reclaimed gets a new mtime. sameProcess
// treats a changed Started as PID reuse, which would stop the watcher and
// drop the attach baseline. starttime+btime is fixed for the process
// lifetime and does not move when the wall clock steps.
func startedAt(pid int) time.Time {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return time.Time{}
	}
	ticks, ok := procStartTicks(string(b))
	if !ok {
		return time.Time{}
	}
	boot := linuxBootTime()
	if boot.IsZero() {
		return time.Time{}
	}
	d, ok := ticksSinceBoot(ticks)
	if !ok {
		return time.Time{}
	}
	return boot.Add(d)
}

// procStartTicks returns /proc/PID/stat field 22 (starttime), skipping the
// comm field whose parentheses may contain spaces.
func procStartTicks(stat string) (uint64, bool) {
	closeP := strings.LastIndexByte(stat, ')')
	if closeP < 0 || closeP+2 > len(stat) {
		return 0, false
	}
	rest := stat[closeP+2:]
	field := 3
	i := 0
	for field <= 22 && i < len(rest) {
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
		if field == 22 {
			n, err := strconv.ParseUint(rest[start:i], 10, 64)
			return n, err == nil
		}
		field++
	}
	return 0, false
}

func ticksSinceBoot(ticks uint64) (time.Duration, bool) {
	const nsPerTick = int64(time.Second) / linuxClkTck
	if ticks > uint64(math.MaxInt64/nsPerTick) {
		return 0, false
	}
	return time.Duration(ticks) * time.Duration(nsPerTick), true
}

var (
	bootTimeMu  sync.Mutex
	bootTimeVal time.Time
)

func linuxBootTime() time.Time {
	bootTimeMu.Lock()
	defer bootTimeMu.Unlock()
	if !bootTimeVal.IsZero() {
		return bootTimeVal
	}
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		secStr, ok := strings.CutPrefix(line, "btime ")
		if !ok {
			continue
		}
		sec, err := strconv.ParseInt(strings.TrimSpace(secStr), 10, 64)
		if err != nil || sec <= 0 {
			return time.Time{}
		}
		bootTimeVal = time.Unix(sec, 0)
		return bootTimeVal
	}
	return time.Time{}
}
