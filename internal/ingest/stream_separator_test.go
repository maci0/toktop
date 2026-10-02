// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package ingest

import (
	"net/http"
	"testing"
)

// The operation prose in docs/openapi.yaml and the README both promise the
// decoder is looser than NDJSON: between two objects any JSON whitespace will
// do, and no separator is required at all, so `{...} {...}` and `{...}{...}`
// are two-event bodies exactly like two lines. Each of those forms is driven
// against the real server here and its accepted count is pinned, so the
// promise is checked against the decoder rather than asserted in a comment: a
// decoder that grew a line-bound would refuse the space- and tab-separated
// bodies below, and the operation description would be describing a server
// that no longer answers that way.
func TestDocumentedStreamSeparatorsAreAccepted(t *testing.T) {
	for _, tc := range []struct {
		name      string
		separator string
	}{
		{"newline, the NDJSON form", "\n"},
		{"a space", " "},
		{"a tab", "\t"},
		{"a carriage return", "\r\n"},
		{"no separator at all", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &memRecorder{}
			s := startIngest(t, rec)
			body := ndjsonEvent("a") + tc.separator + ndjsonEvent("b")
			if got := post(t, "http://"+s.Addr()+eventsPath, body); got != http.StatusAccepted {
				t.Fatalf("POST %q = %d, want 202", body, got)
			}
			awaitEvents(t, rec, 2)
			if len(rec.evs) != 2 {
				t.Errorf("recorded %d events for %q, want 2", len(rec.evs), body)
			}
		})
	}
}

// The other half of that promise is the refusal: a non-whitespace,
// non-object byte where a separator could go is a 400, and it lands after the
// events before it were recorded rather than instead of them. A comma between
// two objects is the byte a sender joining them by hand actually produces, so
// it is the one this pins.
func TestDocumentedStreamRefusalKeepsTheEventsBeforeIt(t *testing.T) {
	rec := &memRecorder{}
	s := startIngest(t, rec)
	body := ndjsonEvent("a") + "," + ndjsonEvent("b")
	if got := post(t, "http://"+s.Addr()+eventsPath, body); got != http.StatusBadRequest {
		t.Fatalf("POST %q = %d, want 400", body, got)
	}
	awaitEvents(t, rec, 1)
	if len(rec.evs) != 1 {
		t.Errorf("recorded %d events for %q, want the 1 that decoded before the comma", len(rec.evs), body)
	}
}
