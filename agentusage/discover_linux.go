// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build linux

package agentusage

import (
	"cmp"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// Discover lists the agent CLIs running on this machine, in ascending pid
// order.
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
	known := knownNames()
	r := procReader{path: make([]byte, 0, 64)}

	var out []Process
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		r.pid = pid

		var comm string
		if name, err := r.read(procCommFile); err == nil {
			comm = strings.TrimSpace(string(name))
		}

		raw, err := r.read(procCmdlineFile)
		if err != nil || len(raw) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		tool := agentName(comm, args, known)
		if tool == "" {
			continue
		}
		cwd, err := os.Readlink(r.procPath(procCwdFile))
		if err != nil {
			continue // exited, or another user's process
		}
		out = append(out, Process{PID: pid, Tool: tool, Dir: cwd, Started: startedAt(pid), AllDirs: tool == "dsh" && dshHosts(args)})
	}
	// os.ReadDir sorts entry names as strings, which orders pids 1, 10, 100,
	// 11, 2. A caller listing agents would read that as a broken listing, so
	// the order is fixed here rather than left to each caller's sort.
	slices.SortFunc(out, func(a, b Process) int { return cmp.Compare(a.PID, b.PID) })
	return out
}

// The files one process contributes to a walk.
const (
	procCommFile    = "comm"
	procCmdlineFile = "cmdline"
	procCwdFile     = "cwd"
	procStatFile    = "stat"
)

// procReader carries the scratch one /proc walk needs: the path it is opening
// and the buffer that file is read into. One of each per walk, and neither
// outlives it.
//
// The buffer replaces os.ReadFile, whose size hint comes from the stat: every
// file under /proc reports st_size 0, so it cannot size a buffer from it and
// grows one from 512 bytes, reallocating and copying the whole file several
// times over. Over a walk of every process on the host that was the largest
// single source of garbage, repeated on every discovery pass.
//
// What comes back aliases the buffer, so it is consumed before the next read:
// the string it is turned into is a copy, which is what the arguments and the
// comm are cut from.
type procReader struct {
	pid  int
	path []byte
	buf  []byte
}

// procPath builds /proc/PID/<name> in the reader's own buffer. The result is
// only valid until the next call, so it is consumed by the read or the
// Readlink that follows and never retained.
func (r *procReader) procPath(name string) string {
	r.path = append(r.path[:0], "/proc"...)
	r.path = append(r.path, '/')
	r.path = strconv.AppendInt(r.path, int64(r.pid), 10)
	r.path = append(r.path, '/')
	r.path = append(r.path, name...)
	return string(r.path)
}

// read reads one of this process's files into the reader's buffer and returns
// the bytes, which alias it and are valid until the next call.
func (r *procReader) read(name string) ([]byte, error) {
	f, err := os.Open(r.procPath(name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// The buffer keeps whatever capacity the widest file so far needed, so the
	// walk grows it once instead of once per file.
	if cap(r.buf) == 0 {
		r.buf = make([]byte, procReadInit)
	}
	r.buf = r.buf[:0]
	for {
		if len(r.buf) == cap(r.buf) {
			r.buf = append(r.buf, 0)[:len(r.buf)]
		}
		n, err := f.Read(r.buf[len(r.buf):cap(r.buf)])
		r.buf = r.buf[:len(r.buf)+n]
		if err != nil {
			if errors.Is(err, io.EOF) {
				return r.buf, nil
			}
			return r.buf, err
		}
		if n == 0 {
			return r.buf, nil
		}
	}
}

// procReadInit is the first size the walk's buffer is allocated at. A comm is
// 16 bytes and a stat line a few hundred; a command line runs to kilobytes, so
// this covers the common file without a grow and the walk's widest one costs
// one.
const procReadInit = 512

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
	var ticks uint64
	var ok bool
	core.WalkProcStat(stat, 22, func(field int, value string) bool {
		if field != 22 {
			return true
		}
		n, err := strconv.ParseUint(value, 10, 64)
		ticks, ok = n, err == nil
		return false
	})
	return ticks, ok
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
