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

// procCmdlineFile and procStatFile are the two files one process contributes
// to a sweep.
const (
	procCmdlineFile = "cmdline"
	procStatFile    = "stat"
)

// procWalk carries the scratch one /proc sweep needs: the path it is opening
// and the buffer that file is read into. One of each per sweep, and neither
// outlives it.
//
// The buffer replaces os.ReadFile, whose size hint comes from the stat: every
// file under /proc reports st_size 0, so it cannot size a buffer from it and
// grows one from 512 bytes, reallocating and copying the whole file several
// times on the way to a command line that is usually a few hundred bytes.
// That was the largest single source of garbage in a sweep, repeated for
// every process on the host on every poll.
type procWalk struct {
	path []byte
	buf  []byte
	// args is the scratch the command-line split is built in, reused for
	// every process on the host. It holds headers only: the strings are
	// windows onto the line they were cut from, and a process the walk keeps
	// gets a detached copy of them (see cloneArgs).
	args []string
}

// procPath builds procRoot/PID/<name> in the walk's own buffer. The result
// is only valid until the next call, so it is consumed by the open that
// follows and never retained: one short copy per open, against the joined
// path and the concatenated name a build from the directory entry would
// allocate per process.
func (w *procWalk) procPath(pid int, name string) string {
	w.path = append(w.path[:0], procRoot...)
	w.path = append(w.path, '/')
	w.path = strconv.AppendInt(w.path, int64(pid), 10)
	w.path = append(w.path, '/')
	w.path = append(w.path, name...)
	return string(w.path)
}

// read opens name under pid and reads it into the walk's buffer, reporting
// the bytes read. A file that does not fit is read up to the buffer's size
// and reported truncated, so a caller that cannot accept a partial file
// (procStatCPUAndRSS reads a field by position) falls back to a full read
// rather than parse a line whose tail is missing.
func (w *procWalk) read(pid int, name string) (b []byte, truncated bool, err error) {
	f, err := os.Open(w.procPath(pid, name))
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	w.buf = w.buf[:cap(w.buf)]
	n := 0
	// truncated stays true until a read reports the end of the file: a file
	// whose length is exactly the buffer's filled it, and the next read would
	// have returned 0 bytes rather than more, so the fallback the flag buys
	// re-reads a file that was not cut.
	truncated = true
	for n < len(w.buf) {
		r, rerr := f.Read(w.buf[n:])
		n += r
		if rerr != nil || r == 0 {
			truncated = false
			break
		}
	}
	return w.buf[:n], truncated, nil
}

// listLinux walks /proc: everything from plain files, zero subprocesses.
func listLinux() ([]raw, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	w := procWalk{
		// One cmdline's worth, which is the most a sweep can keep: ClipArgs
		// drops every byte past CmdlinePrefix, and a browser or an Electron
		// app carries hundreds of kilobytes of flags that the walk read on
		// every poll only to discard them. The bound is the read, and the
		// arguments a process keeps are the same either way.
		path: make([]byte, 0, 64),
		buf:  make([]byte, CmdlinePrefix),
		// The widest argv a bounded command line can hold: one empty
		// argument per byte, and the separators between them. The split
		// stops there, so the scratch cannot grow past it either.
		args: make([]string, 0, CmdlinePrefix/2+1),
	}
	var out []raw
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue // not a process dir
		}
		cmdlineB, _, err := w.read(pid, procCmdlineFile)
		if err != nil {
			continue // vanished or kernel thread
		}
		// One copy of the line, and the arguments are windows onto it.
		// Splitting it was the largest source of garbage in a sweep: every
		// process on the host paid a string header per argument, twice
		// (the split and the clip), for a listing that keeps a handful of
		// processes and none of their tails.
		line := strings.TrimRight(string(cmdlineB), "\x00")
		args := splitCmdline(line, w.args)
		if len(args) == 0 || args[0] == "" {
			continue
		}

		r := raw{pid: pid, name: baseName(args[0])}
		annotateClipped(&r, args)
		if r.engine == "" && r.port == 0 {
			// Not an engine, so nothing past this line is read and
			// nothing above is retained: the next read overwrites the
			// buffer these arguments are windows onto.
			continue // skip /proc/PID/stat for firefox and friends
		}
		r.args = cloneArgs(args)

		// One stat read yields both CPU ticks and RSS; a second read of
		// status would double the per-PID syscalls on every poll. A stat
		// line longer than the buffer is a process with an unusually long
		// name, and its fields are read by position, so it is read whole
		// rather than parsed from a line missing its tail.
		if stat, truncated, err := w.read(pid, procStatFile); err == nil {
			if truncated {
				stat = readProcFile(w.procPath(pid, procStatFile))
			}
			r.ticks, r.rss = procStatCPUAndRSS(string(stat))
		}
		out = append(out, r)
	}
	return out, nil
}

// readProcFile reads a whole file the walk's buffer could not hold. Only a
// stat line past CmdlinePrefix bytes reaches it, so the path is rare and the
// read is not worth the buffer growth every other file would pay.
func readProcFile(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

// procStatCPUAndRSS sums utime+stime jiffies (fields 14,15) and reads the
// resident set in pages (field 24) from one /proc/PID/stat body, respecting
// the comm parens (a comm may contain spaces).
func procStatCPUAndRSS(stat string) (ticks uint64, rssBytes uint64) {
	var utime, stime, pages uint64
	core.WalkProcStat(stat, 24, func(field int, value string) bool {
		switch field {
		case 14:
			utime, _ = strconv.ParseUint(value, 10, 64)
		case 15:
			stime, _ = strconv.ParseUint(value, 10, 64)
		case 24:
			pages, _ = strconv.ParseUint(value, 10, 64)
		}
		return true
	})
	return core.SatAddU64(utime, stime), core.MulSatU64(pages, uint64(os.Getpagesize()))
}
