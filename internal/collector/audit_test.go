package collector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
	auditLog.Set(func() *slog.Logger {
		return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	t.Cleanup(func() { auditLog.Set(nil) })
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
	t.Cleanup(func() { c.SetNow(nil) })
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
	t.Cleanup(func() { c.SetNow(nil) })
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
	t.Cleanup(func() { c.SetNow(nil) })
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

// probeBackend is a generation endpoint a test drives: it answers 500 until
// broken is cleared, then one streaming answer probe.Run can read.
type probeBackend struct {
	broken atomic.Bool
}

func (b *probeBackend) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	if b.broken.Load() {
		http.Error(w, "model unloaded", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	io.WriteString(w, `{"response":"one","done":false}`+"\n"+
		`{"response":"two","done":true,"eval_count":2,"eval_duration":1000000}`+"\n")
}

// An unattended --probe tick on an engine that will not generate is a
// dependency failure nothing else records: the PROBES pane shows it for the
// frame it is drawn on and the engine keeps answering its polls. The audit log
// gets the start of the run once, and its end once.
// A probe that keeps answering is the steady state, so the transition latch
// says nothing about it: the audit log holds no record of the throughput a
// model was measured at, and a regression the next wave recovered from is gone
// with it. Every probe is recorded at debug, with the model it measured, so
// the numbers are attributable without a line per tick under the default floor.
func TestProbeMeasurementsAreAudited(t *testing.T) {
	oldGap := probeWaveGap
	probeWaveGap = 0
	t.Cleanup(func() { probeWaveGap = oldGap })

	srv := httptest.NewServer(&probeBackend{})
	defer srv.Close()

	c := New([]provider.Provider{(&fakeProvider{label: "engine", addr: srv.URL}).asProvider()}, time.Second)
	c.lastModel[srv.URL] = "m"
	c.SetNow(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() })
	t.Cleanup(func() { c.SetNow(nil) })
	logs := captureAudit(t)

	for range 2 {
		c.ProbeAll()
		waitFor(t, func() bool {
			c.probeMu.Lock()
			defer c.probeMu.Unlock()
			return len(c.probeInflight) == 0
		}, "probe never cleared")
	}

	if got := countLines(logs, "probe ok"); got != 2 {
		t.Fatalf("probe lines = %d, want one per wave:\n%s", got, logs.String())
	}
	// One line per probe is the point, but the transitions must not start
	// re-reporting with it: an engine that never failed has no run to end.
	if got := countLines(logs, "probe answering again"); got != 0 {
		t.Errorf("recovery lines = %d, want none for an engine that never failed:\n%s", got, logs.String())
	}
	for _, want := range []string{"engine=engine", "model=m", "ttft_ms=", "tok_per_s=", "tokens=2"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("probe line does not carry %s:\n%s", want, logs.String())
		}
	}
}

func TestProbeFailuresAreAuditedOnce(t *testing.T) {
	oldGap := probeWaveGap
	probeWaveGap = 0
	t.Cleanup(func() { probeWaveGap = oldGap })

	backend := &probeBackend{}
	backend.broken.Store(true)
	srv := httptest.NewServer(backend)
	defer srv.Close()

	c := New([]provider.Provider{(&fakeProvider{label: "engine", addr: srv.URL}).asProvider()}, time.Second)
	c.lastModel[srv.URL] = "m"
	// The latch ages on the collector clock, so a run replaying on a pinned
	// one reports the same down_for the frames carry.
	clock := time.Unix(1_700_000_000, 0).UTC()
	c.SetNow(func() time.Time { return clock })
	t.Cleanup(func() { c.SetNow(nil) })
	logs := captureAudit(t)

	wave := func() {
		c.ProbeAll()
		waitFor(t, func() bool {
			c.probeMu.Lock()
			defer c.probeMu.Unlock()
			return len(c.probeInflight) == 0
		}, "probe never cleared")
	}
	for range 3 {
		wave()
	}
	if got := countLines(logs, "probe failed"); got != 1 {
		t.Fatalf("failure lines = %d, want 1 for three failing waves:\n%s", got, logs.String())
	}
	for _, want := range []string{"engine=engine", "model=m", "duration=", "reason=", "model unloaded"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("failure line does not carry %s:\n%s", want, logs.String())
		}
	}

	backend.broken.Store(false)
	clock = clock.Add(90 * time.Second)
	// The 503 above armed the Retry-After backoff, and a wave inside it is
	// skipped without reaching the engine. The test waits the backout out
	// rather than the 15 seconds it names.
	c.probeMu.Lock()
	c.probeBackoff = nil
	c.probeMu.Unlock()
	for range 2 {
		wave()
	}
	if got := countLines(logs, "probe answering again"); got != 1 {
		t.Fatalf("recovery lines = %d, want 1:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), "down_for=") {
		t.Errorf("recovery line does not carry how long the failures ran:\n%s", logs.String())
	}
	// The 90 seconds the clock was stepped by, and not the wall time the test
	// spent waiting out the waves.
	if !strings.Contains(logs.String(), "down_for=1m30s") {
		t.Errorf("down_for is not the collector clock's span:\n%s", logs.String())
	}
	// A wave that keeps answering is the steady state, not a transition: a
	// line per --probe tick is the noise this latch exists to prevent.
	if got := countLines(logs, "probe failed"); got != 1 {
		t.Fatalf("failure lines = %d, want the recovery not to re-arm the latch:\n%s", got, logs.String())
	}
}

// A sender whose events all sort behind the retained window is dropped
// silently: its own POST says stored under accepted, which is the same answer
// a replay gets, and the agent list goes empty with nothing on it to say why.
// The audit log gets the run once, not once per interval, and one line when an
// event lands again.
func TestWindowRefusalsAreAuditedOnce(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	now := base
	c := New(nil, time.Second)
	c.SetNow(func() time.Time { return now })
	t.Cleanup(func() { c.SetNow(nil) })
	c.procFn = nil
	logs := captureAudit(t)
	ch := make(chan core.Snapshot, 1)
	emit := func() {
		c.emit(context.Background(), ch)
		<-ch
	}

	for i := range core.AgentHistoryLen {
		now = base.Add(time.Duration(i) * time.Second)
		c.RecordAgent(core.AgentEvent{At: now, ID: fmt.Sprintf("n%d", i), Agent: "a"})
	}
	oldest := c.agents[0].At
	if c.RecordAgent(core.AgentEvent{At: oldest.Add(-time.Second), ID: "stale", Agent: "lagging"}) {
		t.Fatal("an event older than the retained window reported as stored")
	}
	// Two more refusals and three frames: the run is one line, not one per
	// event or per poll.
	for range 2 {
		now = now.Add(time.Second)
		c.RecordAgent(core.AgentEvent{At: oldest.Add(-time.Second), ID: "stale2", Agent: "lagging"})
	}
	for range 3 {
		emit()
	}
	if got := countLines(logs, "agent events refused"); got != 1 {
		t.Fatalf("refusal lines = %d, want 1:\n%s", got, logs.String())
	}
	for _, want := range []string{"agent=lagging", "refused=3", "reason="} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("refusal line does not carry %s:\n%s", want, logs.String())
		}
	}
	if got := countLines(logs, "agent events stored again"); got != 0 {
		t.Fatalf("recovery logged with no stored event yet:\n%s", logs.String())
	}

	// A replay is a sender retrying its own POST, not a sender being dropped,
	// so it must not arm the latch: an endpoint under a retry storm would
	// otherwise report an outage it has none of.
	now = base.Add((core.AgentHistoryLen + 1) * time.Second)
	if !c.RecordAgent(core.AgentEvent{At: now, ID: "fresh", Agent: "a"}) {
		t.Fatal("a newer event was refused")
	}
	now = now.Add(time.Second)
	if c.RecordAgent(core.AgentEvent{At: now, ID: "fresh", Agent: "a"}) {
		t.Fatal("a duplicate id was stored twice")
	}
	emit()
	if got := countLines(logs, "agent events refused"); got != 1 {
		t.Fatalf("a duplicate re-armed the latch: %d refusal lines\n%s", got, logs.String())
	}
	if got := countLines(logs, "agent events stored again"); got != 1 {
		t.Fatalf("recovery lines = %d, want 1:\n%s", got, logs.String())
	}
	emit()
	if got := countLines(logs, "agent events stored again"); got != 1 {
		t.Fatalf("recovery logged again with no new refusals: %d lines\n%s", got, logs.String())
	}
}

// The recovery line closes a run of refusals, so it closes the count with it.
// A count left standing would be carried into the next run and its opening
// line would report refusals that run never had.
func TestWindowRefusalCountResetsAfterRecovery(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	now := base
	c := New(nil, time.Second)
	c.SetNow(func() time.Time { return now })
	t.Cleanup(func() { c.SetNow(nil) })
	c.procFn = nil
	logs := captureAudit(t)
	ch := make(chan core.Snapshot, 1)
	emit := func() {
		c.emit(context.Background(), ch)
		<-ch
	}

	for i := range core.AgentHistoryLen {
		now = base.Add(time.Duration(i) * time.Second)
		c.RecordAgent(core.AgentEvent{At: now, ID: fmt.Sprintf("n%d", i), Agent: "a"})
	}
	oldest := c.agents[0].At
	refuse := func(id string) {
		now = now.Add(time.Second)
		if c.RecordAgent(core.AgentEvent{At: oldest.Add(-time.Second), ID: id, Agent: "lagging"}) {
			t.Fatalf("%s was stored, so there is no run to close", id)
		}
	}

	refuse("stale1")
	refuse("stale2")
	emit() // opening line: refused=2
	refuse("stale3")
	now = base.Add((core.AgentHistoryLen + 1) * time.Second)
	if !c.RecordAgent(core.AgentEvent{At: now, ID: "fresh", Agent: "a"}) {
		t.Fatal("a newer event was refused")
	}
	emit() // recovery line, which closes the run
	refuse("stale4")
	emit() // opening line of the next run: refused=1

	lines := logs.String()
	if got := countLines(logs, "refused=1"); got != 1 {
		t.Fatalf("lines reporting refused=1 = %d, want only the new run's:\n%s", got, lines)
	}
	if got := countLines(logs, "refused=3"); got != 1 {
		return // the new run wrongly inherited the closed run's two refusals
	}
	t.Errorf("the closed run's count carried into the next run:\n%s", lines)
}
