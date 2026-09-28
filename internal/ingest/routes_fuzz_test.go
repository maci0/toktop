package ingest

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rivo/uniseg"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/logcfg"
)

// FuzzRoute drives the whole served chain, the one a network peer actually
// reaches: wrap's security headers, request id and 404/405 guards in front of
// the endpoint table. The method, the path, both caller-supplied headers and
// the body are fuzzed together, so the routing decisions are checked against
// the same bytes a hostile sender controls. The path is set on the request
// rather than parsed from a target, so unnormalized and non-UTF-8 spellings
// reach lookupEndpoint instead of being rejected by the URL parser.
func FuzzRoute(f *testing.F) {
	for _, seed := range []struct {
		method, path, header, key string
		body                      []byte
	}{
		{http.MethodPost, eventsPath, "", "", []byte(`{"agent":"coder","output_tokens":10}`)},
		{http.MethodGet, healthPath, "", "", nil},
		{http.MethodHead, healthPath, "", "", nil},
		{http.MethodPost, healthPath, "", "", nil},
		{http.MethodGet, eventsPath, "", "", nil},
		{http.MethodPost, eventsPath + "/", "", "", nil},
		{http.MethodPost, eventsPath, "1", "k1", []byte(`{"agent":"a"}`)},
		{http.MethodPost, eventsPath, strings.Repeat("r", 500) + "\u0007\u001b[2J", "k2", []byte(`{"agent":"b"}`)},
		{http.MethodPost, eventsPath, "caf\u00e9", strings.Repeat("\U0001F1E9", 200), nil},
		{http.MethodPost, eventsPath, "", "", []byte("\xef\xbb\xbf{\"agent\":\"bom\"}")},
		{"PATCH", eventsPath, "", "", []byte(`{"agent":"c"}`)},
		{"", eventsPath, "", "", nil},
		{http.MethodPost, "", "", "", nil},
		{http.MethodPost, "/*", "", "", nil},
		{http.MethodPost, "/v1/../healthz", "", "", nil},
		{http.MethodPost, "/v1/events/", "", "", []byte(`{"agent":"d"}`)},
		{http.MethodPost, "/V1/EVENTS", "", "", nil},
		{http.MethodPost, "/healthz\x00", "", "", nil},
		{http.MethodPost, eventsPath, "", "", []byte("{bad")},
		{http.MethodPost, eventsPath, "", "", nil},
	} {
		f.Add(seed.method, seed.path, seed.header, seed.key, seed.body)
	}
	f.Fuzz(func(t *testing.T, method, path, header, key string, body []byte) {
		rec := &memRecorder{}
		s := &Server{rec: rec, log: slog.New(slog.DiscardHandler)}

		r := httptest.NewRequest(http.MethodPost, "http://localhost/v1/events", bytes.NewReader(body))
		r.Method = method
		r.URL = &url.URL{Path: path}
		r.Header.Set("X-Request-Id", header)
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)

		for name, want := range map[string]string{
			"X-Content-Type-Options":       "nosniff",
			"X-Frame-Options":              "DENY",
			"Cross-Origin-Resource-Policy": "same-origin",
			"Cache-Control":                "no-store",
			"Referrer-Policy":              "no-referrer",
		} {
			if got := w.Header().Get(name); got != want {
				t.Errorf("%s = %q, want %q (status %d)", name, got, want, w.Code)
			}
		}
		for _, name := range []string{"Content-Security-Policy", "X-Request-Id"} {
			if w.Header().Get(name) == "" {
				t.Errorf("%s missing on status %d", name, w.Code)
			}
		}

		// The echoed id is the caller's header after the same sanitization the
		// audit line applies, so an escape or a newline cannot ride back out.
		reqID := w.Header().Get("X-Request-Id")
		if want := logcfg.Field(header, 64); want != "" {
			if reqID != want {
				t.Errorf("X-Request-Id = %q, want %q", reqID, want)
			}
		} else if logcfg.Field(reqID, 64) != reqID {
			t.Errorf("minted X-Request-Id %q is not already sanitized", reqID)
		}

		respBody := w.Body.String()
		for _, leaked := range []string{"agentEventWire", "Go struct field", "Go value of type", "2006-01-02T15:04:05", "stack"} {
			if strings.Contains(respBody, leaked) {
				t.Fatalf("response leaked internals %q: %q", leaked, respBody)
			}
		}

		switch w.Code {
		case http.StatusNotFound:
			// An unknown path names every served endpoint, so a sender can act
			// on the answer. An empty Allow header here would mean the 404
			// came from the mux and the endpoint list drifted out of the table.
			if w.Header().Get("Allow") != "" {
				t.Errorf("404 carries Allow %q", w.Header().Get("Allow"))
			}
			for _, e := range ingestEndpoints {
				if !strings.Contains(respBody, e.path+" ("+e.allow()+")") {
					t.Fatalf("404 for %q omits endpoint %s (%s): %q", path, e.path, e.allow(), respBody)
				}
			}
			// The list has to come apart, or a sender reading it has to guess
			// where one endpoint's methods stop and the next path starts.
			if got, want := advertisedEndpoints(respBody), len(ingestEndpoints); len(got) != want {
				t.Fatalf("404 for %q splits into %d endpoints %q, want %d: %q", path, len(got), got, want, respBody)
			}
			for _, e := range ingestEndpoints {
				methods, listed := advertisedEndpoints(respBody)[e.path]
				if !listed {
					t.Fatalf("404 for %q does not list %s: %q", path, e.path, respBody)
				}
				if methods != e.allow() {
					t.Fatalf("404 for %q lists %s as %q, want %q", path, e.path, methods, e.allow())
				}
			}
			if len(rec.evs) != 0 {
				t.Fatalf("404 recorded %d events", len(rec.evs))
			}
		case http.StatusMethodNotAllowed:
			e, known := lookupEndpoint(path)
			if !known {
				t.Fatalf("405 for unknown path %q", path)
			}
			if got, want := w.Header().Get("Allow"), e.allow(); got != want {
				t.Errorf("Allow = %q, want %q", got, want)
			}
			if !strings.Contains(respBody, e.path) {
				t.Errorf("405 for %q omits the path it serves: %q", path, respBody)
			}
			if len(rec.evs) != 0 {
				t.Fatalf("405 recorded %d events", len(rec.evs))
			}
		case http.StatusForbidden:
			// Only the Origin guard answers 403, and only for a POST: the
			// fuzzer sends no Origin, so anything else here is a hole.
			if path != eventsPath || method != http.MethodPost {
				t.Fatalf("403 for %s %q without an Origin header", method, path)
			}
			if len(rec.evs) != 0 {
				t.Fatalf("403 recorded %d events", len(rec.evs))
			}
		case http.StatusOK:
			if path != healthPath {
				t.Fatalf("200 for %q", path)
			}
			// A HEAD carries the GET's headers and no body (RFC 9110), and
			// states the length it withheld, so the recorder sees an empty
			// body and the GET's Content-Length rather than the line itself.
			if method == http.MethodHead {
				if respBody != "" {
					t.Errorf("HEAD health body = %q, want empty", respBody)
				}
				if got, want := w.Header().Get("Content-Length"), strconv.Itoa(len(healthOK)); got != want {
					t.Errorf("HEAD health Content-Length = %q, want %q", got, want)
				}
				break
			}
			if respBody != healthOK {
				t.Errorf("health body = %q, want %q", respBody, healthOK)
			}
		case http.StatusAccepted:
			if path != eventsPath || method != http.MethodPost {
				t.Fatalf("202 for %s %q", method, path)
			}
			if len(rec.evs) == 0 {
				t.Fatal("202 accepted but no event recorded")
			}
			var ack, stored int
			if n, _ := fmt.Sscanf(respBody, `{"accepted":%d,"stored":%d}`, &ack, &stored); n != 2 || ack != len(rec.evs) || stored != len(rec.evs) {
				t.Fatalf("ack %q vs %d recorded events", respBody, len(rec.evs))
			}
		case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusRequestTimeout, http.StatusServiceUnavailable:
			// A rejection keeps the events decoded before it; each one still
			// has to satisfy the boundary checks below.
		default:
			t.Fatalf("unexpected status %d for %s %q body %q", w.Code, method, path, body)
		}

		for i, ev := range rec.evs {
			if ev.Agent == "" || ev.Kind == "" || ev.At.IsZero() {
				t.Errorf("event %d: defaults not applied: %+v", i, ev)
			}
			if ev.PromptTokens < 0 || ev.OutputTokens < 0 || ev.ThinkingTokens < 0 {
				t.Errorf("event %d: negative token counts retained: %+v", i, ev)
			}
			for _, c := range []struct {
				field, s string
				cap      int
			}{
				{"agent", ev.Agent, 64},
				{"model", ev.Model, 128},
				{"note", ev.Note, 512},
				{"kind", ev.Kind, 24},
				{"id", ev.ID, 128},
				{"via_engine", ev.ViaEngine, 128},
			} {
				if !utf8.ValidString(c.s) {
					t.Errorf("event %d: %s is not valid UTF-8: %q", i, c.field, c.s)
				}
				if n := uniseg.GraphemeClusterCount(c.s); n > c.cap {
					t.Errorf("event %d: %s = %d characters, cap %d", i, c.field, n, c.cap)
				}
				assertRenderSafe(t, i, c.field, c.s)
			}
		}
	})
}

// FuzzIdempotentReplay pins the pair assertion across the persistence
// boundary: the same body under the same Idempotency-Key must produce the
// same event ids on a fresh feed, since a derived id is the key plus the
// line's 1-based index and a replay lands on keys the feed already holds.
func FuzzIdempotentReplay(f *testing.F) {
	for _, seed := range []struct {
		key, body string
	}{
		{"k1", `{"agent":"a","output_tokens":1}`},
		{"k1", `{"agent":"a"}` + "\n" + `{"agent":"b"}`},
		{"", `{"agent":"a"}`},
		{"caf\u00e9", `{"agent":"a","note":"x"}` + "\n" + `{"agent":"b","note":"y"}` + "\nnot json"},
		{strings.Repeat("\U0001F1E9", 200), `{"agent":"a"}`},
		{"k2", `{"id":"own","agent":"a"}` + "\n" + `{"agent":"b"}`},
	} {
		f.Add(seed.key, seed.body)
	}
	f.Fuzz(func(t *testing.T, key, body string) {
		ids := make([][]string, 2)
		var codes [2]int
		for run := range ids {
			rec := &memRecorder{}
			s := &Server{rec: rec, log: slog.New(slog.DiscardHandler)}
			r := httptest.NewRequest(http.MethodPost, "http://localhost/v1/events", strings.NewReader(body))
			r.Header.Set("Idempotency-Key", key)
			w := httptest.NewRecorder()
			s.routes().ServeHTTP(w, r)
			codes[run] = w.Code
			for _, ev := range rec.evs {
				ids[run] = append(ids[run], ev.ID)
			}
		}
		if codes[0] != codes[1] {
			t.Fatalf("replays answered %d and %d for %q", codes[0], codes[1], body)
		}
		if len(ids[0]) != len(ids[1]) {
			t.Fatalf("replays recorded %d and %d events", len(ids[0]), len(ids[1]))
		}
		for i := range ids[0] {
			if ids[0][i] != ids[1][i] {
				t.Fatalf("event %d id %q on the first send, %q on the replay", i, ids[0][i], ids[1][i])
			}
		}
	})
}

// advertisedEndpoints reads a 404 body the way a client reading the endpoint
// list has to: everything after the "endpoints: " prefix, one entry per
// semicolon, each a path and a parenthesised method list. It maps path to the
// methods that path was advertised with, so a test can compare the answer
// against the table rather than against a substring of it.
func advertisedEndpoints(body string) map[string]string {
	_, list, ok := strings.Cut(body, "endpoints: ")
	if !ok {
		return nil
	}
	out := map[string]string{}
	for entry := range strings.SplitSeq(strings.TrimRight(list, "\n"), "; ") {
		path, rest, ok := strings.Cut(entry, " (")
		if !ok {
			// An entry with no method list is one the client cannot read a
			// method off. Recording it under the empty string is enough for the
			// comparison to fail, and keeps the parse from inventing a path.
			out[entry] = ""
			continue
		}
		out[path] = strings.TrimSuffix(rest, ")")
	}
	return out
}

// FuzzEventBoundary documents the caps core.SanitizeText and the per-field
// clamps are read through, so a change to either shows up as a fuzz failure
// here rather than as a terminal escape in the feed.
func FuzzEventBoundary(f *testing.F) {
	for _, seed := range []string{
		"", " ", "\x00", "\u001b[31m", "\U0001F1E9\U0001F1EA", "caf\u00e9", "cafe\u0301", "cl\u200bd",
		strings.Repeat("a", 4096), strings.Repeat("\u001b", 512), "\xff\xfe",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := core.SanitizeText(s)
		if twice := core.SanitizeText(got); twice != got {
			t.Errorf("SanitizeText is not idempotent: %q -> %q -> %q", s, got, twice)
		}
		if !utf8.ValidString(got) {
			t.Errorf("SanitizeText(%q) = %q is not valid UTF-8", s, got)
		}
		field := logcfg.Field(s, 64)
		if !utf8.ValidString(field) {
			t.Errorf("Field(%q) = %q is not valid UTF-8", s, field)
		}
		if n := uniseg.GraphemeClusterCount(field); n > 64 {
			t.Errorf("Field kept %d characters, cap 64", n)
		}
		// One log attribute, one line: nothing that can break the audit line
		// apart may survive, escapes included.
		for _, r := range field {
			if r < 0x20 || r == 0x7f {
				t.Errorf("Field(%q) = %q keeps control character %q", s, field, r)
			}
		}
	})
}
