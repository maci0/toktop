// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build linux

package agentusage

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Discover must find a real process whose argv names an agent even when the
// kernel calls it something else (the node-script case), and attribute it to
// its working directory.
func TestDiscoverFindsProcessByArgvName(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("sleep", "30")
	cmd.Dir = dir
	cmd.Args[0] = "claude" // /proc/PID/cmdline then reads "claude\030"
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn sleep: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()

	var found *Process
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range Discover() {
			if p.PID == cmd.Process.Pid {
				found = &p
			}
		}
		if found != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if found == nil {
		t.Fatal("Discover did not report a process renamed to claude")
	}
	if found.Tool != "claude" {
		t.Errorf("Tool = %q, want claude", found.Tool)
	}
	if found.Dir == "" {
		t.Fatal("Dir is empty; attribution would be impossible")
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != found.Dir {
		t.Errorf("Dir = %q, want %q", found.Dir, resolved)
	}
	if found.Started.IsZero() {
		t.Fatal("Started is zero; PID reuse would fall back to tool+dir")
	}
	again := startedAt(cmd.Process.Pid)
	if !again.Equal(found.Started) {
		t.Errorf("Started moved from %v to %v; sameProcess would treat this as PID reuse", found.Started, again)
	}
	if skew := time.Since(found.Started); skew < 0 || skew > time.Minute {
		t.Errorf("Started = %v, %v from now; want a recent process start", found.Started, skew)
	}
}

func TestProcStartTicks(t *testing.T) {
	// 18 zeros after state fill fields 4–21 so 12345 is field 22 (starttime).
	const zeros = " 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 "
	cases := []struct {
		stat string
		want uint64
		ok   bool
	}{
		{"1 (systemd) S" + zeros + "12345 0 0", 12345, true},
		{"42 (foo bar) S" + zeros + "99 0", 99, true},
		{"7 (x(y)) S" + zeros + "1 0", 1, true},
		{"1 (x) S 0 0", 0, false},
		{"no close paren", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := procStartTicks(tc.stat)
		if ok != tc.ok || got != tc.want {
			t.Errorf("procStartTicks(%q) = %d, %v; want %d, %v", tc.stat, got, ok, tc.want, tc.ok)
		}
	}
}

func TestTicksSinceBootSaturates(t *testing.T) {
	d, ok := ticksSinceBoot(0)
	if !ok || d != 0 {
		t.Errorf("0 ticks = %v, %v; want 0, true", d, ok)
	}
	d, ok = ticksSinceBoot(linuxClkTck)
	if !ok || d != time.Second {
		t.Errorf("1s of ticks = %v, %v; want 1s, true", d, ok)
	}
	if _, ok := ticksSinceBoot(^uint64(0)); ok {
		t.Error("overflowing tick count must not convert")
	}
}

func TestLinuxBootTimeRetriesFailure(t *testing.T) {
	bootTimeMu.Lock()
	saved := bootTimeVal
	bootTimeVal = time.Time{}
	bootTimeMu.Unlock()
	t.Cleanup(func() {
		bootTimeMu.Lock()
		bootTimeVal = saved
		bootTimeMu.Unlock()
	})

	bt := linuxBootTime()
	if bt.IsZero() {
		t.Fatal("linuxBootTime returned zero time on Linux host")
	}
	bt2 := linuxBootTime()
	if !bt2.Equal(bt) {
		t.Fatalf("linuxBootTime changed across calls: %v vs %v", bt, bt2)
	}
}
