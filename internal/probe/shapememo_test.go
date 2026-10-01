// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// shapeOfBody names the shape a probe request carries, from the cap field and
// the sampling field each spelling differs on. A strict engine's refusal is a
// property of the spelling the probe chose, so the test server has to read it
// off the request rather than count the requests.
func shapeOfBody(m map[string]any) openaiShape {
	_, both := m["max_tokens"]
	_, completion := m["max_completion_tokens"]
	switch {
	case both && completion:
		return shapeBoth
	case both:
		return shapeLegacy
	default:
		if _, ok := m["temperature"]; !ok {
			return shapeReasoning
		}
		return shapeCompletion
	}
}

// strictShapeServer answers any shape at or above a threshold and 400s every
// shape below it, which is how an engine that rejects max_completion_tokens,
// or max_tokens, or a sampling value behaves. posts counts every request the
// server saw, refused ones included, and threshold is where that boundary
// sits, so a test can turn the engine's mind between waves. A threshold at
// len(openaiShapes) refuses every shape.
func strictShapeServer(t *testing.T, from openaiShape) (base string, posts *atomic.Int64, threshold *atomic.Int64) {
	t.Helper()
	var n, cur atomic.Int64
	cur.Store(int64(from))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		var m map[string]any
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			t.Errorf("decode probe request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if int64(shapeOfBody(m)) < cur.Load() {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"unknown field"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { forgetShape(srv.URL) })
	return srv.URL, &n, &cur
}

// A probe of an engine that needs shapeLegacy costs three refused POSTs on the
// first wave. Every later wave must cost one: the shape that answered is
// remembered, so a run with --probe against a strict engine pays the refusals
// once instead of once per wave for the life of the run.
func TestRunOpenAIShapeWalkIsRememberedPerEngine(t *testing.T) {
	base, posts, _ := strictShapeServer(t, shapeLegacy)

	first := Run(context.Background(), Request{Kind: core.KindVLLM, Base: base, Model: "m"})
	if !first.OK {
		t.Fatalf("first probe = %+v, want ok", first)
	}
	if got, want := posts.Load(), int64(int(shapeLegacy)+1); got != want {
		t.Fatalf("first probe POSTs = %d, want %d (the walk up to the serving shape)", got, want)
	}

	posts.Store(0)
	second := Run(context.Background(), Request{Kind: core.KindVLLM, Base: base, Model: "m"})
	if !second.OK {
		t.Fatalf("second probe = %+v, want ok", second)
	}
	if got := posts.Load(); got != 1 {
		t.Errorf("second probe POSTs = %d, want 1: the refused shapes were paid for again", got)
	}
}

// An engine whose remembered shape starts refusing again must not be written
// off: the probe walks on from there and the answer rewrites the memo.
func TestRunOpenAIMemoForgetsOnRefusal(t *testing.T) {
	base, posts, accept := strictShapeServer(t, shapeReasoning)
	if s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: base, Model: "m"}); !s.OK {
		t.Fatalf("first probe = %+v, want ok", s)
	}
	if got := posts.Load(); got != int64(len(openaiShapes)) {
		t.Fatalf("first probe POSTs = %d, want %d (the whole walk)", got, len(openaiShapes))
	}

	// The engine now refuses every shape, so this probe can only fail, and the
	// refusal has to land on the memoized shape rather than on a shorter walk.
	accept.Store(int64(len(openaiShapes)))
	posts.Store(0)
	if s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: base, Model: "m"}); s.OK {
		t.Fatalf("probe against a refusing engine = %+v, want failure", s)
	}
	if got := posts.Load(); got != 1 {
		t.Errorf("probe at the memoized shape POSTs = %d, want 1: the walk did not start where the memo left off", got)
	}
	if _, ok := memoShape(base); ok {
		t.Error("memo survived a probe refused at its own shape")
	}
}

// The memo is per engine: one strict engine's walk must not send a permissive
// engine's first probe straight at shapeLegacy.
func TestRunOpenAIMemoIsPerEngine(t *testing.T) {
	strictBase, _, _ := strictShapeServer(t, shapeLegacy)
	if s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: strictBase, Model: "m"}); !s.OK {
		t.Fatalf("strict probe = %+v, want ok", s)
	}

	var posts atomic.Int64
	permissive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(permissive.Close)
	t.Cleanup(func() { forgetShape(permissive.URL) })

	if s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: permissive.URL, Model: "m"}); !s.OK {
		t.Fatalf("permissive probe = %+v, want ok", s)
	}
	if got := posts.Load(); got != 1 {
		t.Errorf("permissive engine POSTs = %d, want 1: it answered shapeBoth, not a borrowed memo", got)
	}
}

// A refusal that is not a shape disagreement must not rewrite the memo or
// walk: 429 is a spend signal and the walk already refuses to continue on one.
func TestRunOpenAIDoesNotWalkOrMemoOn429(t *testing.T) {
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"rate limited"}`)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { forgetShape(srv.URL) })

	s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
	if s.OK || s.RetryAfter != 30*time.Second {
		t.Fatalf("429 sample = %+v, want RetryAfter 30s", s)
	}
	if got := posts.Load(); got != 1 {
		t.Errorf("429 POSTs = %d, want 1", got)
	}
	if _, ok := memoShape(srv.URL); ok {
		t.Error("a 429 recorded a shape as accepted")
	}
}
