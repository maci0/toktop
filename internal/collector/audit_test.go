package collector

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/provider"
)

// captureAudit points the package logger at a buffer and restores it after,
// so the lines an operator would find in a journal can be asserted.
func captureAudit(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := audit
	audit = func() *slog.Logger {
		return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	t.Cleanup(func() { audit = old })
	return &buf
}

func countLines(buf *bytes.Buffer, sub string) int {
	n := 0
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// An engine that stops answering is a dependency failure, and the dashboard
// only shows it for the frame it happens to be drawn on. The audit log gets
// the outage once, not once per poll, and one line when it comes back.
func TestEngineHealthTransitionsAreAuditedOnce(t *testing.T) {
	fp := &fakeProvider{label: "engine", m: &provider.Metrics{OutTotal: 10}}
	c := New([]provider.Provider{fp.asProvider()}, time.Second)
	c.SetNow(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() })
	c.procFn = nil
	logs := captureAudit(t)
	ch := make(chan core.Snapshot, 1)

	c.emit(context.Background(), ch)
	<-ch
	if got := countLines(logs, "engine not answering"); got != 0 {
		t.Fatalf("healthy engine produced %d failure lines", got)
	}

	fp.err = errors.New("connection refused")
	for range 3 {
		c.emit(context.Background(), ch)
		<-ch
	}
	if got := countLines(logs, "engine not answering"); got != 1 {
		t.Fatalf("failure lines = %d, want 1 for three failed polls:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), "reason=") || !strings.Contains(logs.String(), "connection refused") {
		t.Errorf("failure line does not carry the reason:\n%s", logs.String())
	}

	fp.err, fp.m = nil, &provider.Metrics{OutTotal: 20}
	for range 2 {
		c.emit(context.Background(), ch)
		<-ch
	}
	if got := countLines(logs, "engine answering again"); got != 1 {
		t.Fatalf("recovery lines = %d, want 1:\n%s", got, logs.String())
	}
}

// shrinkSlowPoll drops the latency threshold to a value a test can cross
// without sleeping for the real one.
func shrinkSlowPoll(t *testing.T, d time.Duration) {
	t.Helper()
	old := slowPollThreshold
	slowPollThreshold = d
	t.Cleanup(func() { slowPollThreshold = old })
}

// An engine that answers slowly is the same green on the dashboard as one
// that answers quickly, so a degradation between "fine" and "not answering"
// is invisible without a line. The run is latched like an outage: one line
// when it starts, one when it ends, however many polls in between.
func TestSlowEnginePollsAreAuditedOnce(t *testing.T) {
	shrinkSlowPoll(t, 5*time.Millisecond)
	fp := &fakeProvider{label: "engine", m: &provider.Metrics{OutTotal: 10}}
	c := New([]provider.Provider{fp.asProvider()}, time.Second)
	c.SetNow(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() })
	c.procFn = nil
	logs := captureAudit(t)
	ch := make(chan core.Snapshot, 1)

	fp.delay = 40 * time.Millisecond
	for range 3 {
		c.emit(context.Background(), ch)
		<-ch
	}
	if got := countLines(logs, "engine poll slow"); got != 1 {
		t.Fatalf("slow lines = %d, want 1 for three slow polls:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), "duration=") {
		t.Errorf("slow line does not carry the poll duration:\n%s", logs.String())
	}
	if got := countLines(logs, "engine not answering"); got != 0 {
		t.Errorf("a slow engine produced %d outage lines:\n%s", got, logs.String())
	}

	fp.delay = 0
	for range 2 {
		c.emit(context.Background(), ch)
		<-ch
	}
	if got := countLines(logs, "engine poll back to normal"); got != 1 {
		t.Fatalf("latency recovery lines = %d, want 1:\n%s", got, logs.String())
	}
}

// An outage supersedes a slow run: once the engine stops answering, the
// outage line is the signal, and the next answer is measured fresh. A latch
// left behind would report a slowdown that ended before the outage began.
func TestEngineOutageClearsTheSlowRun(t *testing.T) {
	shrinkSlowPoll(t, 5*time.Millisecond)
	fp := &fakeProvider{label: "engine", m: &provider.Metrics{OutTotal: 10}}
	c := New([]provider.Provider{fp.asProvider()}, time.Second)
	c.SetNow(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() })
	c.procFn = nil
	logs := captureAudit(t)
	ch := make(chan core.Snapshot, 1)

	fp.delay = 40 * time.Millisecond
	c.emit(context.Background(), ch)
	<-ch

	fp.delay, fp.err = 0, errors.New("connection refused")
	c.emit(context.Background(), ch)
	<-ch

	fp.err = nil
	c.emit(context.Background(), ch)
	<-ch
	if got := countLines(logs, "engine poll back to normal"); got != 0 {
		t.Fatalf("latency recovery lines = %d, want 0 after an outage:\n%s", got, logs.String())
	}
}
