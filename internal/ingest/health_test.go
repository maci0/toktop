// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package ingest

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// deadWriter is a peer that stopped reading: the status line lands and the body
// does not, which is the one probe failure the endpoint used to answer with
// nothing at all.
type deadWriter struct {
	header http.Header
	status int
}

func (w *deadWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *deadWriter) Write([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

func (w *deadWriter) WriteHeader(status int) { w.status = status }

// A probe whose answer never reached the prober is the one health failure with
// no trace at all: the endpoint reports a state change, the prober sees
// nothing, and the missing line is the only evidence either side. The latch
// keeps it one line per episode rather than one per check.
func TestHealthzRecordsAnAnswerThatWasNotDelivered(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{rec: &memRecorder{}, log: slog.New(slog.NewTextHandler(&buf, nil))}
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	w := &deadWriter{}
	s.handleHealth(w, r)
	if w.status != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.status, http.StatusOK)
	}
	if got := buf.String(); !strings.Contains(got, "not delivered to the prober") ||
		!strings.Contains(got, "connection reset by peer") {
		t.Fatalf("a probe whose body was not written left no line: %q", got)
	}

	// The second check in the same episode is steady state, not news.
	buf.Reset()
	s.handleHealth(&deadWriter{}, r)
	if got := buf.String(); got != "" {
		t.Errorf("a repeated episode wrote %q; the probe runs every second", got)
	}

	// A probe that lands clears the latch, so a later outage is reported again.
	buf.Reset()
	s.handleHealth(httptest.NewRecorder(), r)
	buf.Reset()
	s.handleHealth(&deadWriter{}, r)
	if got := buf.String(); !strings.Contains(got, "not delivered to the prober") {
		t.Errorf("a second outage after a delivered answer was not recorded: %q", got)
	}
}
