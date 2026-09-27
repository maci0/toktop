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
