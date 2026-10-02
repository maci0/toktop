// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package probe

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maci0/toktop/internal/core"
)

// A turn the model declined, or a content filter cut, arrives over HTTP 200
// carrying the shape of an ordinary completion: a choice with a little text in
// it, sometimes a usage count beside it. Read as an ordinary completion it
// produced a time to first token and a decode rate for text no model
// decoded, and the pane showed that beside a real measurement with nothing to
// tell them apart — a filtered stub reads as the fastest generation the engine
// ever ran. Both the streaming and the whole-body dialects are pinned, since
// an engine that ignores stream:true is the one that would otherwise keep
// doing this after the other is fixed.
func TestRunRefusesATurnNoModelDecoded(t *testing.T) {
	for _, tc := range []struct {
		name        string
		kind        string
		contentType string
		body        string
		wantReason  string
	}{
		{
			name:        "stream filtered after content",
			kind:        core.KindVLLM,
			contentType: "text/event-stream",
			body: "data: {\"choices\":[{\"delta\":{\"content\":\"I cannot\"}}]}\n\n" +
				"data: {\"choices\":[{\"finish_reason\":\"content_filter\",\"delta\":{}}]}\n\n" +
				"data: [DONE]\n\n",
			wantReason: "content_filter",
		},
		{
			name:        "stream refusal object",
			kind:        core.KindVLLM,
			contentType: "text/event-stream",
			body: "data: {\"choices\":[{\"finish_reason\":\"stop\",\"message\":{\"content\":null,\"refusal\":\"declined\"}}]}\n\n" +
				"data: [DONE]\n\n",
			wantReason: "declined",
		},
		{
			name:        "json filtered with usage",
			kind:        core.KindVLLM,
			contentType: "application/json",
			body:        `{"choices":[{"index":0,"finish_reason":"content_filter","message":{"content":"I cannot"}}],"usage":{"completion_tokens":3}}`,
			wantReason:  "content_filter",
		},
		{
			name:        "json refusal beside content",
			kind:        core.KindVLLM,
			contentType: "application/json",
			body:        `{"choices":[{"index":0,"finish_reason":"stop","message":{"content":"I cannot help.","refusal":"policy"}}]}`,
			// The sentence the model wrote is the reason; the one-word reason
			// field is not quoted in front of it.
			wantReason: "policy",
		},
		{
			name:        "json gemini safety through a gateway",
			kind:        core.KindVLLM,
			contentType: "application/json",
			body:        `{"choices":[{"index":0,"finish_reason":"SAFETY","message":{"content":""}}]}`,
			wantReason:  "SAFETY",
		},
		{
			// The same refusal spelled on the NDJSON frame field, from a
			// serving stack in front of an Ollama-family daemon.
			name:        "ollama done_reason",
			kind:        core.KindOllama,
			contentType: "application/x-ndjson",
			body:        `{"response":"I cannot","done":true,"done_reason":"content_filter","eval_count":2,"eval_duration":1000000}` + "\n",
			wantReason:  "content_filter",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			s := Run(context.Background(), Request{Kind: tc.kind, Base: srv.URL, Model: "m"})
			if s.OK {
				t.Fatalf("a turn no model decoded produced a measurement: %+v", s)
			}
			// A refused turn is an engine answer, not a probe fault: nothing
			// here is a retry's worth of work, so it must not ask the caller
			// to come back later and pay for it again.
			if s.RetryAfter != 0 {
				t.Errorf("RetryAfter = %v, want no backoff on a refusal", s.RetryAfter)
			}
			if s.Tokens != 0 || s.TTFTms != 0 || s.TokPS != 0 {
				t.Errorf("refusal carried a measurement: %+v", s)
			}
			if !strings.Contains(s.Err, "engine refused: "+tc.wantReason) {
				t.Errorf("err = %q, want the engine's own reason %q", s.Err, tc.wantReason)
			}
		})
	}
}

// The gate is a closed set of endings that all mean the same thing, so the
// ordinary ones must still measure. A closed list a provider could extend is
// the alternative that was rejected, and this is what keeps it from being
// extended by accident: an ending the probe has never heard of reads as an
// answer, because calling a working engine broken costs the operator a real
// measurement on every wave.
func TestRunMeasuresOrdinaryFinishReasons(t *testing.T) {
	for _, reason := range []string{"stop", "length", "tool_calls", "eos_token", "END_TURN", ""} {
		t.Run(reason, func(t *testing.T) {
			body := `{"choices":[{"index":0,"delta":{"content":"one two three"}}]}`
			if reason != "" {
				body = `{"choices":[{"index":0,"finish_reason":"` + reason + `","delta":{"content":"one two three"}}]}`
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: "+body+"\n\ndata: [DONE]\n\n")
			}))
			defer srv.Close()

			s := Run(context.Background(), Request{Kind: core.KindVLLM, Base: srv.URL, Model: "m"})
			if !s.OK {
				t.Fatalf("finish_reason %q refused an ordinary completion: %+v", reason, s)
			}
			// One delta frame is one counted token, and the point of the case
			// is only that a sample came back at all.
			if s.Tokens != 1 || s.TokPS <= 0 {
				t.Errorf("finish_reason %q measured %+v, want one token and a rate", reason, s)
			}
		})
	}
}

// The ordinary endings on the other dialect's frame field, which spells the
// stopping cause done_reason and uses "stop" and "length" for a run that did
// decode. Sharing one closed set across both dialects is what keeps a refusal
// out of a sample, and a gate that has to be re-argued per dialect is a gate
// that is eventually only argued on one of them.
func TestRunOllamaMeasuresOrdinaryDoneReasons(t *testing.T) {
	for _, reason := range []string{"stop", "length", "unload", "load"} {
		t.Run(reason, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				io.WriteString(w, `{"response":"one two","done":true,"done_reason":"`+reason+`","eval_count":2,"eval_duration":1000000000}`+"\n")
			}))
			defer srv.Close()

			s := Run(context.Background(), Request{Kind: core.KindOllama, Base: srv.URL, Model: "m"})
			if !s.OK {
				t.Fatalf("done_reason %q refused an ordinary generation: %+v", reason, s)
			}
			if s.Tokens != 2 || s.TokPS <= 0 {
				t.Errorf("done_reason %q measured %+v, want the engine's own count and a rate", reason, s)
			}
		})
	}
}
