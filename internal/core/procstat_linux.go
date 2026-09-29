// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build linux

package core

import "strings"

// WalkProcStat hands fn each field of a /proc/PID/stat body, numbered from 1,
// stopping after field last or when fn reports it has what it came for. A
// comm may itself contain spaces and parentheses, so the walk starts past the
// last ')' rather than at the first space.
//
// The fields are walked in place: strings.Fields would allocate a slice of
// ~50 strings per process per poll, and this runs over the whole process table
// every frame.
//
// It lives here because two packages read fields out of the same body (the
// agent's process start time and the process table's CPU and RSS); one walker
// is the only way those two cannot drift apart on the comm skip.
func WalkProcStat(stat string, last int, fn func(field int, value string) bool) {
	closeP := strings.LastIndexByte(stat, ')')
	if closeP < 0 || closeP+2 > len(stat) {
		return
	}
	rest := stat[closeP+2:]
	for field, i := 3, 0; field <= last && i < len(rest); field++ {
		for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t') {
			i++
		}
		if i >= len(rest) {
			return
		}
		start := i
		for i < len(rest) && rest[i] != ' ' && rest[i] != '\t' && rest[i] != '\n' {
			i++
		}
		if !fn(field, rest[start:i]) {
			return
		}
	}
}
