// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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

// paddedJSON serves a listing followed by trailing bytes, flushed separately,
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
