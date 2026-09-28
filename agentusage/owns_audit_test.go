// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package agentusage

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureAudit points the package logger at a buffer for the duration of the
// test, so an audit line can be asserted on rather than merely assumed.
func captureAudit(t *testing.T) *strings.Builder {
	t.Helper()
	lines := &strings.Builder{}
	SetLogger(slog.New(slog.NewTextHandler(lines, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { SetLogger(nil) })
	return lines
}

// An undecided file is retried on every poll and never advances an offset, so
// the whole session's usage goes uncounted. The retry is deliberate, but with
// the failure dropped there was no line anywhere: the agent simply never
// appears on the dashboard, which is indistinguishable from an agent that
// never ran.
func TestUnattributableTranscriptIsReported(t *testing.T) {
	store := withStore(t, "dsh")
	work := t.TempDir()
	lines := captureAudit(t)

	w := Watch("dsh", work, time.Now())
	// A directory where a transcript belongs: it opens, and the header scan
	// then fails on the read rather than reaching a verdict. No verdict, no
	// offset, retried for the life of the watcher.
	path := filepath.Join(store, "session.jsonl")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}

	if mine, decided := w.owns(path); decided {
		t.Fatalf("a directory read as an attributed transcript (mine=%v), so the failure below proves nothing", mine)
	}
	if !w.ownsFailed[path] {
		t.Fatal("the failure left no latch, so the reporting below proves nothing")
	}
	if !strings.Contains(lines.String(), "could not be attributed") {
		t.Errorf("an undecidable transcript produced no line; got:\n%s", lines.String())
	}
	if !strings.Contains(lines.String(), path) {
		t.Errorf("the line does not name the transcript; got:\n%s", lines.String())
	}
}

// The same file fails on every poll, and a line per poll would bury the one
// that matters. The name is latched until the file resolves, exactly as the
// read path's latch is.
func TestUnattributableTranscriptIsReportedOnce(t *testing.T) {
	store := withStore(t, "dsh")
	work := t.TempDir()
	lines := captureAudit(t)

	w := Watch("dsh", work, time.Now())
	path := filepath.Join(store, "session.jsonl")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}

	for range 5 {
		if _, decided := w.owns(path); decided {
			t.Fatal("the transcript resolved, so the latch below proves nothing")
		}
	}
	if got := strings.Count(lines.String(), "could not be attributed"); got != 1 {
		t.Errorf("five polls produced %d lines, want 1; got:\n%s", got, lines.String())
	}
}

// The attribution latch is per-file bookkeeping, and a transcript that never
// resolved is in none of the maps the age-out sweep walks: it has no stamp
// and no offset. Without this, a long run leaves one entry per file it ever
// failed to attribute, for the rest of the run.
func TestOwnsFailureLatchAgesOut(t *testing.T) {
	store := withStore(t, "dsh")
	work := t.TempDir()
	captureAudit(t)

	w := Watch("dsh", work, time.Now())
	gone := filepath.Join(store, "undecidable.jsonl")
	w.ownsFailed[gone] = true
	// A live transcript alongside it: ageing one out must not disturb the other.
	live := filepath.Join(store, "session.jsonl")
	append_(t, live, dshHeader(work)+"\n")

	w.forgetIdle([]string{live})
	if _, latched := w.ownsFailed[gone]; latched {
		t.Fatal("the attribution latch outlived the transcript it named")
	}
}
