// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package provider

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// connCounter records how many distinct TCP connections a server accepted. The
// idle pool returns a connection only when its body was closed at EOF, so the
// count is how a test sees whether a probe left its connection reusable.
type connCounter struct {
	mu    sync.Mutex
	conns map[string]struct{}
	n     int
}

func (c *connCounter) new(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conns == nil {
		c.conns = map[string]struct{}{}
	}
	if _, seen := c.conns[r.RemoteAddr]; seen {
		return
	}
	c.conns[r.RemoteAddr] = struct{}{}
	c.n++
}

func (c *connCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// paddedJSONSrv serves a listing followed by trailing bytes, flushed separately,
// so a decode that stops at the end of the JSON value leaves the transfer
// unfinished. That is the shape every engine listing has: a document the probe
// reads, and whatever the server wrote after it.
func paddedJSONSrv(t *testing.T, cc *connCounter) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cc.new(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		w.(http.Flusher).Flush()
		// The tail arrives only after the client has had time to decode and
		// close, so the transfer is provably unfinished when the decode
		// returns rather than unfinished only by luck of packet timing.
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(strings.Repeat(" ", 512)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestScanProbesReuseConnections pins the drain: net/http hands a connection
// back to the idle pool only when its body is closed at EOF, and a discovery
// probe stops reading at the end of the JSON value. Without the drain every
// probe on a discovery pass costs a fresh dial and leaves a socket in
// TIME_WAIT, for the dozen requests identify makes against each candidate
// port.
func TestScanProbesReuseConnections(t *testing.T) {
	cc := &connCounter{}
	srv := paddedJSONSrv(t, cc)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for range 3 {
		if mr := getOpenAIModels(ctx, srv.URL); mr == nil || len(mr.Data) != 1 {
			t.Fatalf("getOpenAIModels = %+v, want one model", mr)
		}
	}
	if n := cc.count(); n != 1 {
		t.Errorf("probes opened %d connections, want 1 reused connection", n)
	}
}

// TestDrainAndCloseStopsAtCap keeps the drain bounded: the bytes go to
// io.Discard, so an endpoint that keeps streaming must cost the cap, not the
// caller's timeout.
func TestDrainAndCloseStopsAtCap(t *testing.T) {
	done := make(chan struct{})
	cc := &connCounter{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cc.new(r)
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		w.(http.Flusher).Flush()
		<-done
	}))
	t.Cleanup(func() {
		close(done)
		srv.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		getOpenAIModels(ctx, srv.URL)
	}()
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("probe on an endless body did not return")
	}
}

// errorBodySrv serves a failing status with a body the client reads only a
// snippet of, padded afterwards so the transfer is provably unfinished when
// the client closes. A gateway in trouble answers this way on every poll, so
// the connection has to come back to the pool here too.
//
// The padding has to clear 4*core.SnippetCap, which is where bearer.StatusError
// stops reading. A body shorter than that is consumed whole by the read that builds
// the message, reaches EOF on its own, and would be reusable with or without
// a drain, so it pins nothing.
func errorBodySrv(t *testing.T, cc *connCounter) *httptest.Server {
	t.Helper()
	pad := strings.Repeat("x", 4*core.SnippetCap+512)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cc.new(r)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("engine is loading the model " + pad))
		w.(http.Flusher).Flush()
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(strings.Repeat(" ", 512)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestFailingPollsReuseConnections pins the same drain on the error path.
// The status is turned into an error message, so nothing here parses a body,
// but the snippet is read to build that message and the transfer stops there.
// Closing the body at that point sends the connection to TIME_WAIT, which is
// the one path a poll takes whenever the engine is down: a flat retry rate
// against a failing engine then costs a handshake and a socket per tick for
// as long as it stays down.
func TestFailingPollsReuseConnections(t *testing.T) {
	cc := &connCounter{}
	srv := errorBodySrv(t, cc)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var listing struct {
		Data []struct{ ID string }
	}
	for range 3 {
		err := getJSON(ctx, srv.URL+"/v1/models", &listing)
		if err == nil {
			t.Fatal("503 accepted as a listing")
		}
		// The message still has to carry the engine's own words, since that
		// is the part that says what went wrong.
		if !strings.Contains(err.Error(), "engine is loading the model") {
			t.Errorf("error dropped the response body: %v", err)
		}
	}
	if n := cc.count(); n != 1 {
		t.Errorf("failing polls opened %d connections, want 1 reused connection", n)
	}
}

// TestGetJSONStopsAtCap pins the same bound on the poll path: getJSON decodes
// one JSON value and then drains, so an engine that answers and keeps the body
// open must cost the cap, not the caller's timeout.
func TestGetJSONStopsAtCap(t *testing.T) {
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ready":true}`))
		w.(http.Flusher).Flush()
		for {
			select {
			case <-done:
				return
			case <-r.Context().Done():
				return
			default:
			}
			// As fast as the socket takes it, which is the shape the cap
			// bounds: an engine answering correctly behind something that
			// never ends the body.
			_, _ = w.Write(bytes.Repeat([]byte("x"), 8<<10))
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(func() {
		close(done)
		srv.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var out struct {
		Ready bool `json:"ready"`
	}
	got := make(chan error, 1)
	start := time.Now()
	go func() { got <- getJSON(ctx, srv.URL, &out) }()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("getJSON: %v", err)
		}
		if !out.Ready {
			t.Error("getJSON decoded ready=false from the served value")
		}
		// The unbounded copy is not a hang: the shared client's timeout ends
		// it. It is that whole timeout added to every poll of an engine that
		// answers correctly, so the bound is on the elapsed time and not just
		// on the call returning.
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("getJSON took %s to drain a tail the cap covers, want the cap not the client timeout", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("getJSON drained an endless body to the caller's timeout, not the cap")
	}
}
