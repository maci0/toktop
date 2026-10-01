// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package selfreload

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/maci0/toktop/internal/logcfg"
)

// records collects the audit lines Watch writes, so a test can read what an
// operator would see.
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

// captureAudit points the audit logger at a handler the test owns and returns
// the recorder, restoring the process logger when the test ends.
func captureAudit(t *testing.T) *records {
	t.Helper()
	r := &records{}
	auditLog.Set(func() *slog.Logger { return slog.New(r) })
	t.Cleanup(func() { auditLog.Set(nil) })
	return r
}

// A stat that keeps failing leaves nothing to compare, so the loop keeps
// ticking and never fires: a rebuild that lands in that window is missed and
// the session runs the old image. Dropped, the operator sees a dashboard that
// simply never refreshes, with no line saying the reload is off.
func TestWatchReportsAnUnreadableExecutable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := captureAudit(t)
		exe := filepath.Join(t.TempDir(), "toktop")
		// Never created: every poll stats a path that is not there.

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); Watch(ctx, exe, 5*time.Millisecond, func() {}) }()

		// Many polls: the report is per transition, not per tick.
		synctest.Wait()
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		cancel()
		<-done

		lines := rec.snapshot()
		var warns int
		for _, l := range lines {
			if strings.Contains(l, "rebuild will not be picked up") {
				warns++
			}
		}
		if warns == 0 {
			t.Fatalf("a stat failing on every poll logged nothing; lines: %v", lines)
		}
		if warns > 1 {
			t.Errorf("the same outage was reported %d times, want one line until it clears", warns)
		}
	})
}

// The outage is a state, not a moment: it clears when the image is readable
// again, and the recovery is a line of its own so an operator watching a
// silent dashboard learns the reload came back.
func TestWatchReportsTheExecutableReadingAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := captureAudit(t)
		dir := t.TempDir()
		exe := filepath.Join(dir, "toktop")

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); Watch(ctx, exe, 5*time.Millisecond, func() {}) }()
		synctest.Wait()

		writeExe(t, exe, "v1")
		time.Sleep(20 * time.Millisecond)
		synctest.Wait()
		cancel()
		<-done

		var back bool
		for _, l := range rec.snapshot() {
			if strings.Contains(l, "readable again") {
				back = true
			}
		}
		if !back {
			t.Fatalf("the recovery was not reported; lines: %v", rec.snapshot())
		}
	})
}

// The first sight of the file is the baseline, not a change, so a Watch that
// started while the image was missing must not fire when it appears: it has
// no earlier identity to compare against.
func TestWatchDoesNotFireOnTheFirstSightAfterAnOutage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		captureAudit(t)
		dir := t.TempDir()
		exe := filepath.Join(dir, "toktop")

		fired := make(chan struct{}, 1)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			Watch(ctx, exe, 5*time.Millisecond, func() { fired <- struct{}{} })
		}()
		synctest.Wait()

		writeExe(t, exe, "v1")
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()

		select {
		case <-fired:
			t.Fatal("the first sight of the image was reported as a rebuild")
		case <-done:
			t.Fatal("Watch returned without a replacement")
		default:
		}
		cancel()
		<-done
	})
}

// The line that reports an unreadable image is the one a developer pastes into
// an issue, and it names the path that failed. Two folds have to reach it.
//
// The error is a PathError, not a string: logcfg.HomeHandler rewrites the home
// directory out of string attributes only, so an error handed over as a value
// kept the account's own directory name in full on the very line that exists to
// explain the failure, while the path beside it folded to "~". And neither
// value was single-lined, so an image path carrying a newline wrote a second
// line into the audit stream and an escape ran in the operator's terminal.
//
// Driven through the real HomeHandler rather than the bare recorder, since the
// fold under test is the one that lives in the handler.
func TestWatchFoldTheExecutableAndItsStatFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		r := &records{}
		auditLog.Set(func() *slog.Logger {
			return slog.New(logcfg.HomeHandler{Handler: r})
		})
		t.Cleanup(func() { auditLog.Set(nil) })

		// Never created, so every poll fails: the file name carries an escape
		// and a newline, the two bytes a log line must not hold.
		exe := filepath.Join(home, "tok\x1b[31mred\nnewline")

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); Watch(ctx, exe, 5*time.Millisecond, func() {}) }()
		synctest.Wait()
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		cancel()
		<-done

		lines := r.snapshot()
		var warned bool
		for _, l := range lines {
			if !strings.Contains(l, "rebuild will not be picked up") {
				continue
			}
			warned = true
			if strings.Contains(l, home) {
				t.Errorf("the audit line named the home directory: %q", l)
			}
			if strings.Contains(l, "\x1b") {
				t.Errorf("the audit line carried a raw escape sequence: %q", l)
			}
			if strings.Contains(l, "\n") {
				t.Errorf("the audit line carried a raw newline: %q", l)
			}
			if !strings.Contains(l, "path=~/") {
				t.Errorf("the path attribute was not folded to ~: %q", l)
			}
		}
		if !warned {
			t.Fatalf("the unreadable image was not reported; lines: %v", lines)
		}
	})
}
