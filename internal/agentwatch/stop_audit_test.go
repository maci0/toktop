// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentwatch

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maci0/toktop/agentusage"
	"github.com/maci0/toktop/internal/logcfg"
)

// records collects the audit lines this package writes, so a test can read
// what an operator would see.
type records struct {
	mu    sync.Mutex
	lines []string
}

func (r *records) Handle(_ context.Context, rec slog.Record) error {
	var b strings.Builder
	b.WriteString(rec.Level.String())
	b.WriteString(" ")
	b.WriteString(rec.Message)
	b.WriteString(" ")
	rec.Attrs(func(a slog.Attr) bool {
		b.WriteString(a.Key)
		b.WriteString("=")
		b.WriteString(a.Value.String())
		b.WriteString(" ")
		return true
	})
	r.mu.Lock()
	r.lines = append(r.lines, b.String())
	r.mu.Unlock()
	return nil
}

func (*records) Enabled(context.Context, slog.Level) bool { return true }
func (r *records) WithAttrs([]slog.Attr) slog.Handler     { return r }
func (r *records) WithGroup(string) slog.Handler          { return r }

func (r *records) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

func linesWith(lines []string, sub string) []string {
	var out []string
	for _, l := range lines {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return out
}

// captureAudit points the audit logger at a handler the test owns, restoring
// the process logger when the test ends.
func captureAudit(t *testing.T) *records {
	t.Helper()
	r := &records{}
	auditLog.Set(func() *slog.Logger { return slog.New(r) })
	t.Cleanup(func() { auditLog.Set(nil) })
	return r
}

// A tracker whose read loop does not unwind inside stopWait loses the final
// poll that carries the agent's tail into the feed, and nothing reads those
// transcripts again once the tracker is gone. The drop used to be silent: the
// agent's total stops short of what its transcripts hold, with a healthy
// dashboard and no line naming the stall.
func TestStopTimedOutTrackerIsAudited(t *testing.T) {
	logs := captureAudit(t)
	w := New(&recorder{}, nil)

	// done never closes, standing in for a read loop parked in the kernel on a
	// mount that stopped answering. watch is nil so the timeout branch is the
	// only thing that could produce a line.
	tr := &tracked{
		proc:   agentusage.Process{PID: 4242, Tool: "claude", Dir: "/tmp/hung"},
		done:   make(chan struct{}),
		cancel: func() {},
	}

	w.stopOne(tr)

	lines := linesWith(logs.snapshot(), "did not stop in time")
	if len(lines) != 1 {
		t.Fatalf("audit lines for one tracker that timed out = %d, want 1: %v", len(lines), lines)
	}
	// The line has to name the agent and the store it was tailing: a stall is
	// almost always the filesystem under one transcript store, and the pid
	// alone does not lead an operator there.
	for _, want := range []string{"agent=claude", "pid=4242", "store=/tmp/hung"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("audit line = %q, want it to carry %q", lines[0], want)
		}
	}
	if !strings.Contains(lines[0], "WARN") {
		t.Errorf("audit line = %q, want it at WARN", lines[0])
	}
}

// A tracker that stops in time is the normal case: the wait returned, and the
// line above would be a false alarm naming an agent that is simply gone.
func TestStopInTimeTrackerIsNotAudited(t *testing.T) {
	logs := captureAudit(t)
	w := New(&recorder{}, nil)

	done := make(chan struct{})
	close(done)
	tr := &tracked{
		proc:   agentusage.Process{PID: 7, Tool: "codex", Dir: "/tmp/ok"},
		done:   done,
		cancel: func() {},
	}

	w.stopOne(tr)

	if lines := linesWith(logs.snapshot(), "did not stop in time"); len(lines) != 0 {
		t.Fatalf("a tracker that stopped in time was audited: %v", lines)
	}
}

// The store field is the operator's route from the line to the filesystem to
// check, and it is a path under the watcher's own home on a real run, so it
// goes through the same folds every other audit attribute does. Asserted
// through the handler the package uses in production, since that handler is
// what applies them.
func TestStopTimeoutLineFoldsTheHomeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads this one on Windows

	r := &records{}
	auditLog.Set(func() *slog.Logger {
		return slog.New(logcfg.HomeHandler{Handler: r})
	})
	t.Cleanup(func() { auditLog.Set(nil) })

	w := New(&recorder{}, nil)
	tr := &tracked{
		proc:   agentusage.Process{PID: 9, Tool: "claude", Dir: home + "/.claude/projects/x"},
		done:   make(chan struct{}),
		cancel: func() {},
	}

	w.stopOne(tr)

	lines := linesWith(r.snapshot(), "did not stop in time")
	if len(lines) != 1 {
		t.Fatalf("audit lines for one tracker that timed out = %d, want 1: %v", len(lines), lines)
	}
	if strings.Contains(lines[0], home) {
		t.Errorf("audit line carries the home directory: %q", lines[0])
	}
}

// stopWait must stay bounded: the wait exists so shutdown cannot hang on a
// stalled mount, and a test that pins the value guards the ceiling the audit
// line above reports.
func TestStopWaitIsBounded(t *testing.T) {
	if stopWait <= 0 || stopWait > 30*time.Second {
		t.Fatalf("stopWait = %s, want a positive bound no longer than 30s", stopWait)
	}
}
