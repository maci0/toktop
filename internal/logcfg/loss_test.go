// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package logcfg

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A stderr that accepts nothing is the failure this exists for: the audit log
// is the only record that a store backup could not be written, a store was
// read back from its copy, or a rename could not be made durable, and slog
// discards whatever Handle returns, so before this the lines were lost with
// nothing anywhere saying so. Three refusals must leave a line naming three
// lost lines, not silence.
func TestALostAuditLineIsReportedOnTheNextOneThatLands(t *testing.T) {
	w := &stubWriter{refuse: 3}
	lw := &lossWriter{fixed: w}
	lg := slog.New(LossHandler{w: lw, inner: slog.NewTextHandler(lw, nil)})

	for i := range 3 {
		lg.Warn("toktop: host key store backup not rewritten", "attempt", i)
	}
	lg.Warn("toktop: ingest listening")

	got := w.buf.String()
	if !strings.Contains(got, "3 audit line(s) could not be written to stderr") {
		t.Fatalf("the loss was not reported: %s", got)
	}
	// The reason is carried, so an operator reads a full disk and a closed
	// pipe as the two different things they are. Naming the count alone
	// leaves them guessing which happened.
	if !strings.Contains(got, stubReason) {
		t.Errorf("the report does not name why stderr refused: %s", got)
	}
	// The line that reported the loss is a line of its own. A reader parsing
	// the log attributes one line to one event, and a notice prefixed onto
	// the next line reads as that event having failed.
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.Contains(line, "could not be written") && strings.Contains(line, "ingest listening") {
			t.Errorf("the loss notice is sharing a line with the record it followed: %s", line)
		}
	}
}

// The count is said once. A stderr that is failing is exactly the channel that
// cannot take a warning per refused line, so repeating it turns one full disk
// into a flood and buries the records that do get through.
func TestTheLossIsReportedOnceAndNotRepeated(t *testing.T) {
	w := &stubWriter{refuse: 2}
	lw := &lossWriter{fixed: w}
	lg := slog.New(LossHandler{w: lw, inner: slog.NewTextHandler(lw, nil)})

	lg.Warn("first")
	lg.Warn("second")
	lg.Warn("third")
	lg.Warn("fourth")

	if n := strings.Count(w.buf.String(), "could not be written"); n != 1 {
		t.Fatalf("the loss was reported %d times, want 1: %s", n, w.buf.String())
	}
	// The lines that landed are still lines: a handler that spent its record
	// on the notice would have lost the events it was built to record.
	if !strings.Contains(w.buf.String(), "third") || !strings.Contains(w.buf.String(), "fourth") {
		t.Errorf("a line that landed was not written: %s", w.buf.String())
	}
}

// The first reason is the one that explains the rest. A stderr that refused
// once for one reason and again for another is the same channel breaking two
// ways, and the second reason is the one that came out of a channel already
// failing, so the first is what an operator acts on.
func TestTheFirstReasonIsTheOneKeptForTheNextReport(t *testing.T) {
	w := &stubWriter{refuse: 2, firstErr: errors.New("no space left on device"), thenErr: errors.New("broken pipe")}
	lw := &lossWriter{fixed: w}
	lg := slog.New(LossHandler{w: lw, inner: slog.NewTextHandler(lw, nil)})

	lg.Warn("lost one")
	lg.Warn("lost two")
	lg.Warn("lands")

	got := w.buf.String()
	if !strings.Contains(got, "no space left on device") {
		t.Errorf("the first reason is the one that explains the rest, and it is not the one reported: %s", got)
	}
	if strings.Contains(got, "broken pipe") {
		t.Errorf("a later reason overwrote the first: %s", got)
	}
}

// The notice goes through the same fold every other line does. It is the one
// line written without a record to draw a path from, but a count reported
// beside a line that had one is still a line in the log, and these are pasted
// into issues; the fold is the only thing that keeps a home directory out of
// them.
func TestTheLossNoticeIsFoldedLikeEveryOtherLine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows
	w := &stubWriter{refuse: 1}
	lw := &lossWriter{fixed: w}
	lg := slog.New(HomeHandler{Handler: LossHandler{w: lw, inner: slog.NewTextHandler(lw, nil)}})

	lg.Warn("cannot read " + filepath.Join(home, "toktop", "known_hosts"))
	lg.Warn("ingest listening")

	if got := w.buf.String(); strings.Contains(got, home) {
		t.Fatalf("the log kept the home directory: %s", got)
	}
}

// A short write is a loss as surely as a failed one, and the writer that
// reported progress before the error must not let a caller read the line as
// whole. A half-written record is a line an operator cannot parse, which is
// worse than one that never arrived, because it looks like it did.
func TestAShortWriteIsCountedAsALostLine(t *testing.T) {
	w := &stubWriter{short: 1}
	lw := &lossWriter{fixed: w}
	lg := slog.New(LossHandler{w: lw, inner: slog.NewTextHandler(lw, nil)})

	lg.Warn("truncated line")
	lg.Warn("next line")

	if !strings.Contains(w.buf.String(), "1 audit line(s) could not be written") {
		t.Fatalf("a short write was not reported as a lost line: %s", w.buf.String())
	}
}

// A healthy run is the case every run takes and the only one that must cost
// nothing: no counter drift into the log, and a refused line that has already
// been reported stays reported rather than being counted again.
func TestAHealthyRunReportsNothing(t *testing.T) {
	w := &stubWriter{}
	lw := &lossWriter{fixed: w}
	lg := slog.New(LossHandler{w: lw, inner: slog.NewTextHandler(lw, nil)})

	lg.Info("toktop: ingest listening", "addr", "127.0.0.1:0")
	lg.Warn("toktop: engine went down", "target", "local")

	if strings.Contains(w.buf.String(), "could not be written") {
		t.Errorf("a run that lost nothing reported a loss: %s", w.buf.String())
	}
	if !strings.Contains(w.buf.String(), "ingest listening") {
		t.Errorf("the handler dropped a line that landed: %s", w.buf.String())
	}
}

// Two loggers, one loss. Every auditing package holds Logger as a func value
// and calls it per line, so a loss counted against the logger that refused the
// line is only reported at all if some *other* logger can see the count. While
// each call built its own counter, a full disk took one subsystem's line and
// nothing anywhere said so: the log read as a run in which the store backup
// that failed never happened, which is the silence the whole handler is here
// to prevent. Pinned across two Logger calls, because a same-logger test
// passes either way and would not have caught it.
func TestALossOnOneLoggerIsReportedByAnother(t *testing.T) {
	w := &stubWriter{refuse: 1}
	old := stderrLoss
	stderrLoss = &lossWriter{fixed: w}
	t.Cleanup(func() { stderrLoss = old })

	first := Logger()
	second := Logger()
	first.Warn("toktop: host key store backup not written", "path", "~/toktop/known_hosts")
	second.Info("toktop: ingest listening", "addr", "127.0.0.1:0")

	got := w.buf.String()
	if !strings.Contains(got, "1 audit line(s) could not be written") {
		t.Fatalf("a line one logger lost was not reported by another: %s", got)
	}
	// Reported once, on the first line that lands (here the second logger's),
	// and as a record of its own, so a reader parsing the log does not
	// attribute it to the record it followed.
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.Contains(line, "could not be written") && strings.Contains(line, "ingest listening") {
			t.Errorf("the loss notice shares a line with the record it followed: %s", line)
		}
	}
	if n := strings.Count(got, "could not be written"); n != 1 {
		t.Errorf("the loss was reported %d times, want 1: %s", n, got)
	}
}

// stubWriter is a stderr that can be made to fail, so the loss path is driven
// from a test rather than from a real full disk.
//
// refuse is how many of the next writes are lost, and short how many are
// written truncated; firstErr and thenErr are the reasons the two kinds carry,
// so a report can be shown to name the one that explains the rest.
type stubWriter struct {
	refuse, short int
	firstErr      error
	thenErr       error

	buf bytes.Buffer
}

// The sink follows os.Stderr as it is now, not as it was when the process
// sink was built. Sharing one counter across loggers is only safe if the sink
// itself is still read per write: a process that redirects stderr after the
// first audit line -- a supervisor capturing a long-running dashboard, a test
// swapping in a pipe -- would otherwise keep writing to the descriptor it
// inherited, so the capture would show a run that logged nothing while the
// terminal it was meant to replace filled with the lines. Sharing the counter
// is what made this worth pinning: the writer stopped being built per call,
// and with it the read of its destination at construction time.
func TestARedirectedStderrIsFollowedByTheSharedSink(t *testing.T) {
	// The shipped sink, not a fresh one: this is a property of how
	// stderrLoss is built, so replacing it would test the replacement.
	if stderrLoss.fixed != nil {
		t.Fatal("the process sink has a fixed destination, so it cannot follow a replaced os.Stderr")
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	realStderr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() {
		os.Stderr = realStderr
		r.Close()
		w.Close()
	})

	// Both loggers are built after the replacement and both have to reach
	// it: the point is that the shared sink resolves os.Stderr per write
	// rather than holding the descriptor it saw when it was built.
	first := Logger()
	second := Logger()
	first.Info("toktop: ingest listening", "addr", "127.0.0.1:0")
	second.Warn("toktop: host key store backup not written")

	w.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{"ingest listening", "host key store backup not written"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q did not reach the redirected stderr, so the audit log went somewhere "+
				"the operator is not reading: %s", want, got)
		}
	}
}

const stubReason = "audit sink is closed"

func (s *stubWriter) Write(p []byte) (int, error) {
	if s.refuse > 0 {
		s.refuse--
		// The first of a run of refusals carries firstErr and the rest carry
		// thenErr, so the branch that keeps the first reason has two
		// distinguishable reasons to choose between rather than the same one
		// twice. A run of one takes the later, since there is no earlier.
		if s.firstErr != nil && s.refuse > 0 {
			return 0, s.firstErr
		}
		if s.thenErr != nil {
			return 0, s.thenErr
		}
		return 0, errors.New(stubReason)
	}
	if s.short > 0 {
		s.short--
		return s.buf.Write(p[:1])
	}
	return s.buf.Write(p)
}
