// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build darwin

package agentusage

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// Discover lists the agent CLIs running on this machine.
//
// On macOS this asks ps(1) for the process table (there is no pure-Go BSD
// process list without cgo) and lsof(8) for each match's working directory;
// macOS exposes no /proc-style cwd symlink to readlink. Only processes that
// name a known agent pay for the second call. A nil result is not an error:
// ps failing and nothing matched both return nil, and callers should surface
// either as no local agents.
func Discover() []Process {
	out, err := psTable()
	if err != nil {
		return nil
	}
	known := knownNames()

	var found []Process
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pidField, rest, ok := strings.Cut(line, " ")
		pid, err := strconv.Atoi(strings.TrimSpace(pidField))
		if !ok || err != nil || pid <= 0 {
			continue
		}
		ucomm, cmd, _ := strings.Cut(strings.TrimLeft(rest, " "), " ")
		var argv []string
		if cmd != "" {
			argv = splitASCIISpace(cmd)
		}
		tool := agentName(ucomm, argv, known)
		if tool == "" {
			continue
		}
		dir := cwdOf(pid)
		if dir == "" {
			continue // exited, or another user's process
		}
		found = append(found, Process{PID: pid, Tool: tool, Dir: dir, AllDirs: tool == "dsh" && dshHosts(argv)})
	}
	return found
}

// psTable returns "pid ucomm command…" for every process, one per line.
// The space-joined command loses quoting, which is acceptable here: matching
// scans whole path components of the first two words only. The separator is
// the ASCII space ps wrote, and only that: a path argument may legally carry
// any other Unicode space (U+00A0, U+3000, U+2028), and splitting on those
// would tear one argument in two and hide the agent behind the fragment.
func psTable() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := commandOutput(ctx, "ps", "-axo", "pid=,ucomm=,command=")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// cwdOf reads a process's working directory out of lsof's machine-parseable
// output (-Fn prints one field per line; names carry an "n" prefix), or ""
// when it cannot be determined.
func cwdOf(pid int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := commandOutput(ctx, "lsof", "-a", "-w", "-p", strconv.Itoa(pid),
		"-d", "cwd", "-Fn")
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if path, ok := strings.CutPrefix(line, "n"); ok && len(path) > 0 {
			return path
		}
	}
	return ""
}
