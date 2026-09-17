package ingest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"
	"unicode/utf8"

	"github.com/rivo/uniseg"

	"github.com/maci0/toktop/internal/core"
)

type memRecorder struct{ evs []core.AgentEvent }

func (m *memRecorder) RecordAgent(ev core.AgentEvent) { m.evs = append(m.evs, ev) }

// post sends body and returns the status code, draining the response so
// keep-alive connections are reusable.
func post(t *testing.T, url, body string) int {
	t.Helper()
	code, _ := postBody(t, url, body)
	return code
}

// postBody sends body and returns the status code plus the response text,
// draining the response so keep-alive connections are reusable.
func postBody(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

// startIngest serves on an ephemeral port backed by rec and closes it at
// cleanup.
func startIngest(t *testing.T, rec core.AgentRecorder) *Server {
	t.Helper()
	return startIngestLog(t, rec, slog.New(slog.DiscardHandler))
}

// awaitEvents waits up to a second for rec to hold n events, failing if they
// never arrive.
func awaitEvents(t *testing.T, rec *memRecorder, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(rec.evs) < n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(rec.evs) != n {
		t.Fatalf("events = %d, want %d", len(rec.evs), n)
	}
}

func TestIngestAcceptsEvents(t *testing.T) {
	rec := &memRecorder{}
	s := startIngest(t, rec)

	resp := post(t, "http://"+s.Addr()+"/v1/events",
		`{"agent":"coder","kind":"tool","prompt_tokens":100,"output_tokens":5,"thinking_tokens":2}`+"\n"+
			`{"agent":"coder","kind":"turn","output_tokens":50}`+"\n")
	if resp != http.StatusAccepted {
		t.Fatalf("status = %d", resp)
	}
	awaitEvents(t, rec, 2)
	ev0 := rec.evs[0]
	if ev0.Agent != "coder" || ev0.Kind != "tool" ||
		ev0.PromptTokens != 100 || ev0.OutputTokens != 5 || ev0.ThinkingTokens != 2 {
		t.Errorf("event 0 = %+v, want coder/tool 100/5/2", ev0)
	}
	ev1 := rec.evs[1]
	if ev1.Agent != "coder" || ev1.Kind != "turn" || ev1.OutputTokens != 50 ||
		ev1.PromptTokens != 0 || ev1.ThinkingTokens != 0 {
		t.Errorf("event 1 = %+v, want coder/turn 0/50/0", ev1)
	}
	if !ev1.At.After(time.Time{}) {
		t.Error("server must stamp missing timestamps")
	}
}

// An id is a caller-chosen key for retries: it must survive ingest as sent
// (after sanitization) so the collector can ignore a replay of the same event.
func TestIngestForwardsId(t *testing.T) {
	rec := &memRecorder{}
	s := startIngest(t, rec)

	resp := post(t, "http://"+s.Addr()+"/v1/events",
		`{"id":"turn-1","agent":"coder","output_tokens":50}`)
	if resp != http.StatusAccepted {
		t.Fatalf("status = %d", resp)
	}
	awaitEvents(t, rec, 1)
	if rec.evs[0].ID != "turn-1" {
		t.Errorf("id = %q, want turn-1", rec.evs[0].ID)
	}
}

// onceRecorder matches the collector's id window: a non-empty id already
// seen is ignored. Used to prove ingest's derived ids make a retried POST
// a no-op the way a body id already does.
type onceRecorder struct{ evs []core.AgentEvent }

func (m *onceRecorder) RecordAgent(ev core.AgentEvent) {
	if core.HasAgentID(m.evs, ev.ID) {
		return
	}
	m.evs = append(m.evs, ev)
}

func postWithHeader(t *testing.T, url, body, header, value string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if header != "" {
		req.Header.Set(header, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// A retried POST (lost 202) with no per-event id still collapses when the
// sender supplies Idempotency-Key: ingest fills the id the collector ignores.
func TestIngestIdempotencyKeyFillsMissingID(t *testing.T) {
	rec := &onceRecorder{}
	s := startIngest(t, rec)
	url := "http://" + s.Addr() + "/v1/events"
	body := `{"agent":"coder","output_tokens":50}` + "\n" +
		`{"agent":"coder","output_tokens":10}`

	if code := postWithHeader(t, url, body, "Idempotency-Key", "harness-batch-7"); code != http.StatusAccepted {
		t.Fatalf("first status = %d", code)
	}
	if code := postWithHeader(t, url, body, "Idempotency-Key", "harness-batch-7"); code != http.StatusAccepted {
		t.Fatalf("retry status = %d", code)
	}
	if len(rec.evs) != 2 {
		t.Fatalf("events = %d, want 2 (one per line, retry ignored)", len(rec.evs))
	}
	if rec.evs[0].ID == "" || rec.evs[1].ID == "" ||
		rec.evs[0].ID != rec.evs[0].ID[:len(rec.evs[0].ID)-2]+":1" ||
		rec.evs[1].ID != rec.evs[1].ID[:len(rec.evs[1].ID)-2]+":2" {
		t.Errorf("ids = %q, %q", rec.evs[0].ID, rec.evs[1].ID)
	}
	if rec.evs[0].OutputTokens != 50 || rec.evs[1].OutputTokens != 10 {
		t.Errorf("tokens = %+v %+v", rec.evs[0], rec.evs[1])
	}

	// A different key is a different POST.
	if code := postWithHeader(t, url, body, "Idempotency-Key", "harness-batch-8"); code != http.StatusAccepted {
		t.Fatalf("other key status = %d", code)
	}
	if len(rec.evs) != 4 {
		t.Fatalf("events after a new key = %d, want 4", len(rec.evs))
	}
}

// Body id is the event's own identity; the POST-level key must not replace it.
func TestIngestBodyIDWinsOverIdempotencyKey(t *testing.T) {
	rec := &memRecorder{}
	s := startIngest(t, rec)

	code := postWithHeader(t, "http://"+s.Addr()+"/v1/events",
		`{"id":"turn-9","agent":"coder","output_tokens":1}`,
		"Idempotency-Key", "post-1")
	if code != http.StatusAccepted {
		t.Fatalf("status = %d", code)
	}
	awaitEvents(t, rec, 1)
	if rec.evs[0].ID != "turn-9" {
		t.Errorf("id = %q, want turn-9", rec.evs[0].ID)
	}
}

// A server-minted X-Request-Id is per attempt. Using it as an event id would
// make every retry look new, and a harness that reuses one correlation id
// across turns would collapse them.
func TestIngestDoesNotMintEventIDFromRequestID(t *testing.T) {
	rec := &memRecorder{}
	s := startIngest(t, rec)

	if code := post(t, "http://"+s.Addr()+"/v1/events", `{"agent":"x","output_tokens":1}`); code != http.StatusAccepted {
		t.Fatalf("status = %d", code)
	}
	awaitEvents(t, rec, 1)
	if rec.evs[0].ID != "" {
		t.Fatalf("server-minted request id became event id %q", rec.evs[0].ID)
	}

	code := postWithHeader(t, "http://"+s.Addr()+"/v1/events",
		`{"agent":"x","output_tokens":2}`, "X-Request-Id", "always-the-same")
	if code != http.StatusAccepted {
		t.Fatalf("status = %d", code)
	}
	awaitEvents(t, rec, 2)
	if rec.evs[1].ID != "" {
		t.Fatalf("X-Request-Id became event id %q", rec.evs[1].ID)
	}
}

func TestIngestDistinctIdempotencyKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys [2]string
	}{
		{"suffix space", [2]string{strings.Repeat("k", 126) + "a", strings.Repeat("k", 126) + "b"}},
		{"header cap", [2]string{strings.Repeat("k", 128) + "a", strings.Repeat("k", 128) + "b"}},
		{"whitespace", [2]string{"batch  one", "batch one"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &onceRecorder{}
			s := &Server{rec: rec}
			for attempt := range 2 {
				for _, key := range tc.keys {
					req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(`{"agent":"coder","output_tokens":50}`))
					req.Header.Set("Idempotency-Key", key)
					w := httptest.NewRecorder()
					s.handlePost(w, req)
					if w.Code != http.StatusAccepted {
						t.Fatalf("attempt %d status = %d", attempt, w.Code)
					}
				}
				if len(rec.evs) != 2 {
					t.Fatalf("attempt %d events = %d, want 2", attempt, len(rec.evs))
				}
			}
		})
	}
}

func TestDerivedEventIDFitsCap(t *testing.T) {
	key := strings.Repeat("k", 200)
	id := derivedEventID(key, 1)
	if n := uniseg.GraphemeClusterCount(id); n > 128 {
		t.Fatalf("id = %d characters, cap 128", n)
	}
	if !strings.HasSuffix(id, ":1") {
		t.Fatalf("id %q lost its sequence suffix", id)
	}
	if derivedEventID("", 1) != "" || derivedEventID("k", 0) != "" {
		t.Fatal("empty key or non-positive seq must not mint an id")
	}
	// A flag is two runes, one character. Counting runes would reject a
	// legal id that clampField kept as 126 flags plus ":1".
	flags := strings.Repeat("\U0001F1E9\U0001F1EA", 80)
	id = derivedEventID(flags, 1)
	if n := uniseg.GraphemeClusterCount(id); n > 128 {
		t.Fatalf("flag id = %d characters, cap 128", n)
	}
	if !utf8.ValidString(id) || !strings.HasSuffix(id, ":1") {
		t.Fatalf("flag id %q is invalid UTF-8 or lost its suffix", id)
	}
}

// via_engine is the same attribution agentwatch stamps when an agent is
// generating through a monitored engine: without it, a harness POST would
// double-count those tokens in header and chart totals.
func TestIngestForwardsViaEngine(t *testing.T) {
	rec := &memRecorder{}
	s := startIngest(t, rec)

	resp := post(t, "http://"+s.Addr()+"/v1/events",
		`{"agent":"coder","output_tokens":50,"via_engine":"127.0.0.1:11434"}`)
	if resp != http.StatusAccepted {
		t.Fatalf("status = %d", resp)
	}
	awaitEvents(t, rec, 1)
	if rec.evs[0].ViaEngine != "127.0.0.1:11434" {
		t.Errorf("via_engine = %q, want 127.0.0.1:11434", rec.evs[0].ViaEngine)
	}
}

// A ts that is not RFC 3339 stays a hard error so sender bugs surface
// instead of silently becoming "now".
func TestIngestRejectsGarbageTimestamp(t *testing.T) {
	rec := &memRecorder{}
	s := startIngest(t, rec)

	code, body := postBody(t, "http://"+s.Addr()+"/v1/events", `{"agent":"x","ts":"yesterday"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("garbage ts status = %d, want 400", code)
	}
	if len(rec.evs) != 0 {
		t.Fatal("garbage-timestamped event recorded")
	}
	if !strings.Contains(body, "ts") || strings.Contains(body, "2006-01-02") {
		t.Errorf("ts error should name the field, not the parser layout, got %q", body)
	}
}

// Type mistakes must name the JSON field (or the expected shape) so a harness
// can fix the payload. encoding/json's default text names the Go type.
func TestIngestJSONTypeErrorsNameTheField(t *testing.T) {
	s := startIngest(t, &memRecorder{})
	base := "http://" + s.Addr() + "/v1/events"
	cases := []struct {
		body string
		want []string
	}{
		{`[{"agent":"a"}]`, []string{"object", "NDJSON", "array"}},
		{`null`, []string{"object", "NDJSON", "null"}},
		{`true`, []string{"object", "NDJSON"}},
		{`123`, []string{"object", "NDJSON"}},
		{`{"prompt_tokens":"100"}`, []string{"prompt_tokens", "integer"}},
		{`{"output_tokens":true}`, []string{"output_tokens", "integer"}},
		{`{"thinking_tokens":[1]}`, []string{"thinking_tokens", "integer"}},
		{`{"agent":["x"]}`, []string{"agent", "string"}},
		{`{"prompt_tokens":100.5}`, []string{"prompt_tokens", "integer"}},
		{`{"agent":"x","ts":123}`, []string{"ts"}},
	}
	for _, tc := range cases {
		code, body := postBody(t, base, tc.body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc.body, code)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s: body %q missing %q", tc.body, body, w)
			}
		}
		for _, leaked := range []string{"agentEventWire", "Go struct field", "Go value of type", "2006-01-02"} {
			if strings.Contains(body, leaked) {
				t.Errorf("%s: body %q leaked %q", tc.body, body, leaked)
			}
		}
	}
}

// Python json.dumps of a float emits 100.0; scientific notation is valid JSON.
// Both are whole numbers and must record as integers, not 400.
func TestIngestAcceptsWholeJSONNumberTokenCounts(t *testing.T) {
	rec := &memRecorder{}
	s := startIngest(t, rec)

	resp := post(t, "http://"+s.Addr()+"/v1/events",
		`{"agent":"py","prompt_tokens":100.0,"output_tokens":1e2,"thinking_tokens":3}`)
	if resp != http.StatusAccepted {
		t.Fatalf("status = %d", resp)
	}
	awaitEvents(t, rec, 1)
	if rec.evs[0].PromptTokens != 100 || rec.evs[0].OutputTokens != 100 || rec.evs[0].ThinkingTokens != 3 {
		t.Errorf("tokens = %+v", rec.evs[0])
	}
}

func TestIngestRejectsTokenCountOverflow(t *testing.T) {
	for _, value := range []string{"9223372036854775808", "9223372036854775808.0", "9.223372036854776e18"} {
		t.Run(value, func(t *testing.T) {
			for _, field := range []string{"prompt_tokens", "output_tokens", "thinking_tokens"} {
				t.Run(field, func(t *testing.T) {
					s := startIngest(t, &memRecorder{})
					resp := post(t, "http://"+s.Addr()+"/v1/events",
						`{"agent":"overflow","`+field+`":`+value+`}`)
					if resp != http.StatusBadRequest {
						t.Fatalf("status = %d, want %d", resp, http.StatusBadRequest)
					}
				})
			}
		})
	}
}

func TestIngestMethodNotAllowedSetsAllow(t *testing.T) {
	s := startIngest(t, &memRecorder{})

	cases := []struct {
		method, path string
		want         []string
	}{
		{http.MethodPut, "/v1/events", []string{http.MethodPost}},
		{http.MethodPost, "/healthz", []string{http.MethodGet, http.MethodHead}},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(tc.method, "http://"+s.Addr()+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s status = %d, want 405", tc.method, tc.path, resp.StatusCode)
			continue
		}
		allow := resp.Header.Get("Allow")
		for _, m := range tc.want {
			if !strings.Contains(allow, m) {
				t.Errorf("%s %s Allow = %q, missing %s", tc.method, tc.path, allow, m)
			}
		}
	}
}

func TestIngestDefaultsAndBadJSON(t *testing.T) {
	rec := &memRecorder{}
	s := startIngest(t, rec)
	base := "http://" + s.Addr() + "/v1/events"

	resp := post(t, base, `{"prompt_tokens":1}`)
	if resp != http.StatusAccepted {
		t.Fatalf("anonymous event rejected: %d", resp)
	}
	awaitEvents(t, rec, 1)
	if rec.evs[0].Agent != "anonymous" || rec.evs[0].Kind != core.AgentKindTurn || rec.evs[0].PromptTokens != 1 {
		t.Errorf("defaults not applied: %+v", rec.evs[0])
	}

	resp = post(t, base, `{not json`)
	if resp != http.StatusBadRequest {
		t.Fatalf("bad json status = %d", resp)
	}
}

// An empty body carries no event at all; the error must say so instead of
// the cryptic decode-level "bad json: EOF".
func TestIngestEmptyBodyRejected(t *testing.T) {
	s := startIngest(t, &memRecorder{})

	code, body := postBody(t, "http://"+s.Addr()+"/v1/events", "")
	if code != http.StatusBadRequest {
		t.Fatalf("empty body status = %d, want 400", code)
	}
	if !strings.Contains(body, "empty body") {
		t.Errorf("body should name the problem, got %q", body)
	}
}

// Streams record incrementally: when a later line fails, events before it
// stay recorded and the error must say how many, so senders resume instead
// of replaying the whole stream and duplicating what was kept.
func TestIngestPartialStreamReportsRecordedCount(t *testing.T) {
	rec := &memRecorder{}
	s := startIngest(t, rec)

	code, body := postBody(t, "http://"+s.Addr()+"/v1/events",
		`{"agent":"kept"}`+"\n"+`{"agent":"dropped","ts":"yesterday"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if !strings.Contains(body, "; 1 earlier event in this stream was recorded") {
		t.Errorf("error should state the recorded count, got %q", body)
	}
	if len(rec.evs) != 1 || rec.evs[0].Agent != "kept" {
		t.Fatalf("events = %+v, want only the first line kept", rec.evs)
	}
}

// The recorded-count note rides along on every stream-level failure,
// including the size cap.
func TestIngestOversizedAfterEventsReportsRecordedCount(t *testing.T) {
	rec := &memRecorder{}
	s := &Server{rec: rec}

	body := `{"agent":"kept"}` + "\n" + `{"agent":"` + strings.Repeat("a", maxEventBody) + `"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handlePost(w, r)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413", w.Code)
	}
	if !strings.Contains(w.Body.String(), "; 1 earlier event in this stream was recorded") {
		t.Errorf("error should state the recorded count, got %q", w.Body.String())
	}
	if len(rec.evs) != 1 {
		t.Fatalf("events = %d, want 1", len(rec.evs))
	}
}

// A body beyond the size cap is a volume problem, not a JSON problem: it
// must answer 413 so senders can tell "trim the payload" from "fix the
// encoding", instead of reading `bad json` on a perfectly encoded stream.
func TestIngestRejectsOversizedBody(t *testing.T) {
	rec := &memRecorder{}
	s := &Server{rec: rec}

	// The body must stay well formed up to the cap, or decoding fails with a
	// syntax error before the limit is ever reached: one event whose string
	// field runs past maxEventBody trips the size cap mid-value instead.
	body := `{"agent":"` + strings.Repeat("a", maxEventBody) + `"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handlePost(w, r)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413", w.Code)
	}
	if !strings.Contains(w.Body.String(), "exceeds") {
		t.Errorf("body should state the cap, got %q", w.Body.String())
	}
	if len(rec.evs) != 0 {
		t.Fatal("event from oversized body recorded")
	}
}

// A POST with an Origin header is browser-driven, and the only way a browser
// reaches this endpoint is a drive-by write from a page the operator is
// visiting (no preflight: text/plain is a simple request). Such requests are
// refused outright; documented senders never set Origin.
func TestIngestRejectsBrowserOriginatedPost(t *testing.T) {
	rec := &memRecorder{}
	s := startIngest(t, rec)

	req, err := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/v1/events",
		strings.NewReader(`{"agent":"forger","kind":"turn","output_tokens":9999}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if len(rec.evs) != 0 {
		t.Fatalf("events = %d, want none from a browser-originated post", len(rec.evs))
	}
}

// Token counts are unsigned quantities; a sender pushing negatives must not
// plant junk values in the retained feed.
func TestIngestClampsNegativeTokenCounts(t *testing.T) {
	rec := &memRecorder{}
	s := startIngest(t, rec)

	resp := post(t, "http://"+s.Addr()+"/v1/events",
		`{"agent":"buggy","prompt_tokens":-500,"output_tokens":-99999999999,"thinking_tokens":-3}`)
	if resp != http.StatusAccepted {
		t.Fatalf("status = %d", resp)
	}
	awaitEvents(t, rec, 1)
	if rec.evs[0].PromptTokens != 0 || rec.evs[0].OutputTokens != 0 || rec.evs[0].ThinkingTokens != 0 {
		t.Errorf("negative token counts retained: %+v", rec.evs[0])
	}
}

// A sender claiming MaxInt64 tokens would wrap the agent totals when two
// such events are summed. Anything past maxEventTokens is junk, same as
// a negative.
func TestIngestDropsAbsurdTokenCounts(t *testing.T) {
	rec := &memRecorder{}
	s := startIngest(t, rec)

	resp := post(t, "http://"+s.Addr()+"/v1/events",
		`{"agent":"buggy","prompt_tokens":1099511627777,"output_tokens":9223372036854775807,"thinking_tokens":1}`)
	if resp != http.StatusAccepted {
		t.Fatalf("status = %d", resp)
	}
	awaitEvents(t, rec, 1)
	if rec.evs[0].PromptTokens != 0 || rec.evs[0].OutputTokens != 0 || rec.evs[0].ThinkingTokens != 1 {
		t.Errorf("absurd token counts retained: %+v", rec.evs[0])
	}
}

// A claimed event timestamp far ahead of arrival is a wrong clock or a
// forgery; it must not enter the retained feed as a future instant, where
// it would pin the UI's "live" marker and render a future wall-clock time.
// Modest skew stays honored.
func TestIngestClampsFarFutureTimestamps(t *testing.T) {
	rec := &memRecorder{}
	s, err := newServer("127.0.0.1:0", rec, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	frozen := time.Unix(1_700_000_000, 0).UTC()
	s.SetNow(func() time.Time { return frozen })
	go s.Serve()
	t.Cleanup(func() { s.Close() })

	farFuture := frozen.Add(time.Hour).Format(time.RFC3339)
	nearFuture := frozen.Add(10 * time.Second).Format(time.RFC3339)
	resp := post(t, "http://"+s.Addr()+"/v1/events",
		`{"agent":"skewed","ts":"`+farFuture+`"}`+"\n"+
			`{"agent":"skewed","ts":"`+nearFuture+`"}`)
	if resp != http.StatusAccepted {
		t.Fatalf("status = %d", resp)
	}
	awaitEvents(t, rec, 2)
	if got := rec.evs[0].At; !got.Equal(frozen) {
		t.Errorf("far-future stamp retained: %v, want clamped to %v", got, frozen)
	}
	want := frozen.Add(10 * time.Second)
	if got := rec.evs[1].At; !got.Equal(want) {
		t.Errorf("modest skew not honored: %v, want %v", got, want)
	}
}

// An empty ts means "absent": every other event field defaults when empty,
// so an empty string must not abort the stream with 400 while null and a
// missing field both decode to "stamp on arrival".
func TestIngestTreatsEmptyTimestampAsAbsent(t *testing.T) {
	rec := &memRecorder{}
	s := startIngest(t, rec)

	resp := post(t, "http://"+s.Addr()+"/v1/events",
		`{"agent":"a","ts":""}`+"\n"+`{"agent":"b","ts":"   "}`)
	if resp != http.StatusAccepted {
		t.Fatalf("status = %d", resp)
	}
	awaitEvents(t, rec, 2)
	for i, ev := range rec.evs {
		if ev.At.IsZero() {
			t.Errorf("event %d: empty ts not stamped", i)
		}
	}
}

// Event stamps that ingest fills in (missing ts, far-future clamp) follow
// an injected clock so a demo recorder's simulated instant is what lands
// in the feed, not a second wall-clock read.
func TestIngestStampsWithInjectedClock(t *testing.T) {
	rec := &memRecorder{}
	s, err := newServer("127.0.0.1:0", rec, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	frozen := time.Unix(1_700_000_000, 0).UTC()
	s.SetNow(func() time.Time { return frozen })
	go s.Serve()
	t.Cleanup(func() { s.Close() })

	farFuture := frozen.Add(time.Hour).Format(time.RFC3339)
	resp := post(t, "http://"+s.Addr()+"/v1/events",
		`{"agent":"a"}`+"\n"+
			`{"agent":"b","ts":"`+farFuture+`"}`)
	if resp != http.StatusAccepted {
		t.Fatalf("status = %d", resp)
	}
	awaitEvents(t, rec, 2)
	for i, ev := range rec.evs {
		if !ev.At.Equal(frozen) {
			t.Errorf("event %d At = %v, want injected %v", i, ev.At, frozen)
		}
	}
}

// The 202 acknowledgment carries a JSON body, so it must advertise
// application/json like every other JSON response from this server,
// leaving clients nothing to sniff.
func TestIngestAckContentType(t *testing.T) {
	s := startIngest(t, &memRecorder{})

	resp, err := http.Post("http://"+s.Addr()+"/v1/events", "application/json",
		strings.NewReader(`{"agent":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("ack content-type = %q, want application/json", got)
	}
}

func TestHealthz(t *testing.T) {
	s := startIngest(t, &memRecorder{})
	resp, err := http.Get("http://" + s.Addr() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("healthz = %d %q, want 200 ok", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("healthz content-type = %q, want text/plain; charset=utf-8", got)
	}
}

func TestHealthzHEADHasNoBody(t *testing.T) {
	s := startIngest(t, &memRecorder{})
	req, err := http.NewRequest(http.MethodHead, "http://"+s.Addr()+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD /healthz = %d, want 200", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Errorf("HEAD /healthz body = %q, want empty", body)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("HEAD /healthz content-type = %q, want text/plain; charset=utf-8", got)
	}
}

func TestIngestSecurityHeaders(t *testing.T) {
	s := startIngest(t, &memRecorder{})
	want := map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Content-Security-Policy":      "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Cache-Control":                "no-store",
		"Referrer-Policy":              "no-referrer",
	}
	for _, path := range []string{"/healthz", "/v1/events"} {
		resp, err := http.Get("http://" + s.Addr() + path)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		for k, v := range want {
			if got := resp.Header.Get(k); got != v {
				t.Errorf("GET %s %s = %q, want %q", path, k, got, v)
			}
		}
	}
	resp, err := http.Post("http://"+s.Addr()+"/v1/events", "application/json",
		strings.NewReader(`{"agent":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("POST /v1/events %s = %q, want %q", k, got, v)
		}
	}
}

func TestIngestClampsOversizedFields(t *testing.T) {
	rec := &memRecorder{}
	s := startIngest(t, rec)

	huge := strings.Repeat("x", 10_000)
	body := fmt.Sprintf(`{"id":%q,"agent":%q,"model":%q,"note":%q,"via_engine":%q,"kind":%q}`, huge, huge, huge, huge, huge, huge)
	resp := post(t, "http://"+s.Addr()+"/v1/events", body)
	if resp != http.StatusAccepted {
		t.Fatalf("status = %d", resp)
	}
	awaitEvents(t, rec, 1)
	ev := rec.evs[0]
	for _, field := range []struct {
		name string
		got  string
		cap  int
	}{
		{"id", ev.ID, 128},
		{"agent", ev.Agent, 64},
		{"model", ev.Model, 128},
		{"note", ev.Note, 512},
		{"via_engine", ev.ViaEngine, 128},
		{"kind", ev.Kind, 24},
	} {
		if want := strings.Repeat("x", field.cap); field.got != want {
			t.Errorf("%s = %q, want %q", field.name, field.got, want)
		}
	}
}

// A retained field cut mid-character renders garbage downstream: half a flag
// emoji is a lone regional indicator, a cut after U+200D leaves a dangling
// joiner. Caps are counted in characters, so the cut must land between them.
func TestIngestClampKeepsCharactersWhole(t *testing.T) {
	rec := &memRecorder{}
	s := startIngest(t, rec)

	for _, tc := range []struct {
		name                  string
		agents, notes         int
		wantAgents, wantNotes int
	}{
		{"below cap", 63, 511, 63, 511},
		{"at cap", 64, 512, 64, 512},
		{"above cap", 65, 513, 64, 512},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := strings.Repeat("\U0001F1E9\U0001F1EA", tc.agents)
			note := strings.Repeat("\U0001F469\u200d\U0001F4BB", tc.notes)
			before := len(rec.evs)
			body := fmt.Sprintf(`{"agent":%q,"note":%q}`, agent, note)
			resp := post(t, "http://"+s.Addr()+"/v1/events", body)
			if resp != http.StatusAccepted {
				t.Fatalf("status = %d", resp)
			}
			awaitEvents(t, rec, before+1)
			ev := rec.evs[before]
			if want := strings.Repeat("\U0001F1E9\U0001F1EA", tc.wantAgents); ev.Agent != want {
				t.Errorf("agent = %q, want %q", ev.Agent, want)
			}
			if want := strings.Repeat("\U0001F469\u200d\U0001F4BB", tc.wantNotes); ev.Note != want {
				t.Errorf("note = %q, want %q", ev.Note, want)
			}
		})
	}
}

// Keep-alive connections idle between requests must be reaped; otherwise
// vanished peers hold an fd and a goroutine each for the process lifetime.
func TestIdleKeepAliveConnsReaped(t *testing.T) {
	old := idleTimeout
	idleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { idleTimeout = old })

	s := startIngest(t, &memRecorder{})

	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /healthz HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive\r\n\r\n", s.Addr())

	buf := make([]byte, 4096)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, err = conn.Read(buf)
		if err == io.EOF {
			return // server closed the now-idle connection
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatal("idle keep-alive connection still open after idleTimeout")
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
}

// kind is attacker-shaped text like every other event field: escape
// sequences must be stripped before the value enters the retained feed.
func TestIngestNormalizesAgentToNFC(t *testing.T) {
	rec := &memRecorder{}
	s := startIngest(t, rec)

	// NFD "café" (e + combining acute) and NFC "café" must land as one
	// identity: otherwise a macOS-typed name and a JSON-NFC name split
	// the agent list and duplicate-id check.
	resp := post(t, "http://"+s.Addr()+"/v1/events",
		`{"id":"cafe\u0301","agent":"cafe\u0301","note":"cafe\u0301"}`)
	if resp != http.StatusAccepted {
		t.Fatalf("status = %d", resp)
	}
	awaitEvents(t, rec, 1)
	want := "caf\u00e9"
	ev := rec.evs[0]
	if ev.ID != want || ev.Agent != want || ev.Note != want {
		t.Errorf("id/agent/note = %q %q %q, want NFC %q", ev.ID, ev.Agent, ev.Note, want)
	}
}

func TestIngestStripsBidiFromAgent(t *testing.T) {
	rec := &memRecorder{}
	s := startIngest(t, rec)

	resp := post(t, "http://"+s.Addr()+"/v1/events",
		`{"agent":"clau\u200bde\u202e"}`)
	if resp != http.StatusAccepted {
		t.Fatalf("status = %d", resp)
	}
	awaitEvents(t, rec, 1)
	if rec.evs[0].Agent != "claude" {
		t.Errorf("agent = %q, want claude with bidi/zwsp stripped", rec.evs[0].Agent)
	}
}

func TestIngestRejectsMixedScriptAgentName(t *testing.T) {
	rec := &memRecorder{}
	s := startIngest(t, rec)

	resp := post(t, "http://"+s.Addr()+"/v1/events",
		`{"agent":"\u0441laude","output_tokens":1}`)
	if resp != http.StatusAccepted {
		t.Fatalf("status = %d", resp)
	}
	awaitEvents(t, rec, 1)
	if rec.evs[0].Agent != "anonymous" {
		t.Errorf("agent = %q, want anonymous for mixed-script spoof of claude", rec.evs[0].Agent)
	}
}

func TestIngestStripsTagCharsFromAgent(t *testing.T) {
	rec := &memRecorder{}
	s := startIngest(t, rec)

	// TAG LATIN SMALL LETTER D is invisible; "clau" + TAG-d + "e" must not
	// be a second agent that looks like "claue".
	resp := post(t, "http://"+s.Addr()+"/v1/events",
		"{\"agent\":\"clau\U000E0064e\"}")
	if resp != http.StatusAccepted {
		t.Fatalf("status = %d", resp)
	}
	awaitEvents(t, rec, 1)
	if rec.evs[0].Agent != "claue" {
		t.Errorf("agent = %q, want claue with tag character stripped", rec.evs[0].Agent)
	}
}

func TestIngestSanitizesCustomKind(t *testing.T) {
	rec := &memRecorder{}
	s := startIngest(t, rec)

	resp := post(t, "http://"+s.Addr()+"/v1/events",
		`{"agent":"x","kind":"\u001b]0;pwned\u0007weird"}`)
	if resp != http.StatusAccepted {
		t.Fatalf("status = %d", resp)
	}
	awaitEvents(t, rec, 1)
	if got := rec.evs[0].Kind; strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, 0x07) {
		t.Errorf("kind retained escape sequences: %q", got)
	}
}

// startPost opens a POST whose body framing allows slow streaming: chunked
// encoding, terminated by a zero chunk.
func startPost(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "POST /v1/events HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n", addr)
	return conn
}

func sendChunk(t *testing.T, conn net.Conn, data string) {
	t.Helper()
	fmt.Fprintf(conn, "%x\r\n%s\r\n", len(data), data)
}

// readResponse drains until the status line plus any following lines arrive
// or the deadline expires, returning everything received.
func readResponse(t *testing.T, conn net.Conn, within time.Duration) string {
	t.Helper()
	buf := make([]byte, 8192)
	var out []byte
	conn.SetReadDeadline(time.Now().Add(within))
	for !bytes.Contains(out, []byte("\r\n")) {
		n, err := conn.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			break
		}
	}
	return string(out)
}

// A sender that stalls mid-body must not pin the connection: the idle
// deadline reaps it with a 408 instead of holding fd and goroutine forever.
func TestIngestReapsStalledBody(t *testing.T) {
	oldLife, oldIdle := maxEventLifetime, bodyIdleTimeout
	maxEventLifetime, bodyIdleTimeout = time.Minute, 150*time.Millisecond
	t.Cleanup(func() { maxEventLifetime, bodyIdleTimeout = oldLife, oldIdle })

	s := startIngest(t, &memRecorder{})

	conn := startPost(t, s.Addr())
	defer conn.Close()
	sendChunk(t, conn, `{"agent":"slow"`) // valid prefix, then silence
	resp := readResponse(t, conn, 5*time.Second)
	if !strings.HasPrefix(resp, "HTTP/1.1 408") {
		t.Fatalf("stalled body response = %q, want 408", resp)
	}
}

// A slow but progressing NDJSON stream stays under the idle deadline and
// must be accepted in full.
func TestIngestAcceptsSlowProgressingStream(t *testing.T) {
	oldLife, oldIdle := maxEventLifetime, bodyIdleTimeout
	maxEventLifetime, bodyIdleTimeout = 30*time.Second, 500*time.Millisecond
	t.Cleanup(func() { maxEventLifetime, bodyIdleTimeout = oldLife, oldIdle })

	rec := &memRecorder{}
	s := startIngest(t, rec)

	conn := startPost(t, s.Addr())
	defer conn.Close()
	for _, ev := range []string{
		`{"agent":"drip","prompt_tokens":1}` + "\n",
		`{"agent":"drip","output_tokens":2}` + "\n",
	} {
		sendChunk(t, conn, ev)
		time.Sleep(100 * time.Millisecond) // well inside the idle window
	}
	fmt.Fprint(conn, "0\r\n\r\n") // end of chunks
	resp := readResponse(t, conn, 5*time.Second)
	if !strings.HasPrefix(resp, "HTTP/1.1 202") {
		t.Fatalf("progressing stream response = %q, want 202", resp)
	}
	awaitEvents(t, rec, 2)
}

// Progress alone must not extend a POST forever: past the absolute lifetime
// the connection is cut even while bytes keep trickling in.
func TestIngestCutsBodyPastAbsoluteLifetime(t *testing.T) {
	oldLife, oldIdle := maxEventLifetime, bodyIdleTimeout
	maxEventLifetime, bodyIdleTimeout = 300*time.Millisecond, time.Minute // idle longer than life
	t.Cleanup(func() { maxEventLifetime, bodyIdleTimeout = oldLife, oldIdle })

	s := startIngest(t, &memRecorder{})

	conn := startPost(t, s.Addr())
	defer conn.Close()
	done := make(chan string, 1)
	go func() {
		done <- readResponse(t, conn, 10*time.Second)
	}()
	keepalive := time.NewTicker(50 * time.Millisecond) // steady progress
	defer keepalive.Stop()
	timeout := time.After(8 * time.Second)
	for {
		select {
		case resp := <-done:
			if !strings.Contains(resp, "408") {
				t.Fatalf("lifetime-capped body response = %q, want 408", resp)
			}
			return
		case <-timeout:
			t.Fatal("absolute lifetime did not bound a progressing body")
		case <-keepalive.C:
			sendChunk(t, conn, "\n")
		}
	}
}

func captureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, nil))
	return lg, &buf
}

func startIngestLog(t *testing.T, rec core.AgentRecorder, lg *slog.Logger) *Server {
	t.Helper()
	s, err := newServer("127.0.0.1:0", rec, lg)
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve()
	t.Cleanup(func() { s.Close() })
	return s
}

func countLogLines(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

// POST /v1/events is the request path an operator cannot see on the
// dashboard when it fails: a structured stderr line has to answer whether it
// succeeded, how long it took, and why it was refused. Wrong-method and
// unknown-path requests share that line. Event bodies stay off the log;
// they are attacker-shaped and the retained feed already holds them.
func TestIngestLogsPostOutcome(t *testing.T) {
	lg, buf := captureLogger()
	rec := &memRecorder{}
	s := startIngestLog(t, rec, lg)

	resp, err := http.Post("http://"+s.Addr()+"/v1/events", "application/json",
		strings.NewReader(`{"agent":"coder","note":"secret-note-value","output_tokens":7}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	reqID := resp.Header.Get("X-Request-Id")
	if reqID == "" {
		t.Fatal("missing X-Request-Id on 202")
	}

	got := buf.String()
	if countLogLines(got) != 1 {
		t.Fatalf("success log lines = %d (%q), want 1", countLogLines(got), got)
	}
	for _, want := range []string{
		`msg="toktop: ingest"`,
		"level=INFO",
		"req=" + reqID,
		"method=POST",
		"path=/v1/events",
		"status=202",
		"accepted=1",
		"duration=",
		"remote=",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("success log missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "secret-note-value") || strings.Contains(got, "coder") {
		t.Errorf("log leaked event body: %s", got)
	}
	if strings.Contains(got, "127.0.0.1") || strings.Contains(got, "::1") {
		t.Errorf("log leaked loopback IP: %s", got)
	}

	buf.Reset()
	code, _ := postBody(t, "http://"+s.Addr()+"/v1/events", `{not json`)
	if code != http.StatusBadRequest {
		t.Fatalf("bad json status = %d", code)
	}
	got = buf.String()
	if countLogLines(got) != 1 {
		t.Fatalf("failure log lines = %d (%q), want 1", countLogLines(got), got)
	}
	for _, want := range []string{
		"level=WARN",
		"status=400",
		"accepted=0",
		`error="`,
		"bad json",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("failure log missing %q: %s", want, got)
		}
	}
}

func TestRedactLogAddrs(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"http: panic serving 203.0.113.9:54321: boom", "http: panic serving remote: boom"},
		{"http: panic serving 127.0.0.1:9999: boom", "http: panic serving loopback:9999: boom"},
		{"http: panic serving [::1]:80: boom", "http: panic serving loopback:80: boom"},
		{"http: TLS handshake error from [2001:db8::1]:443: EOF", "http: TLS handshake error from remote: EOF"},
		{"no address here", "no address here"},
	}
	for _, tc := range cases {
		if got := redactLogAddrs(tc.in); got != tc.want {
			t.Errorf("redactLogAddrs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHTTPErrorLogRedactsPeerAddress(t *testing.T) {
	lg, buf := captureLogger()
	s, err := newServer("127.0.0.1:0", &memRecorder{}, lg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	s.srv.ErrorLog.Printf("http: panic serving %s: boom", "203.0.113.9:54321")
	got := buf.String()
	if strings.Contains(got, "203.0.113.9") {
		t.Errorf("ErrorLog leaked peer IP: %s", got)
	}
	if !strings.Contains(got, "remote") {
		t.Errorf("ErrorLog missing redacted remote: %s", got)
	}

	for _, zone := range []string{"eth0", "enp0s3", "veth-peer.42", "Ethernet 2", "3"} {
		t.Run(zone, func(t *testing.T) {
			buf.Reset()
			peer := &net.TCPAddr{IP: net.ParseIP("fe80::1234"), Port: 54321, Zone: zone}
			s.srv.ErrorLog.Printf("http: TLS handshake error from %s: EOF", peer)
			got := buf.String()
			if strings.Contains(got, "fe80::1234") || strings.Contains(got, "%"+zone) {
				t.Errorf("ErrorLog leaked peer address: %s", got)
			}
			if !strings.Contains(got, "http: TLS handshake error from remote: EOF") {
				t.Errorf("ErrorLog missing redacted remote: %s", got)
			}
		})
	}

	buf.Reset()
	s.srv.ErrorLog.Printf("http: panic serving %s: boom", "127.0.0.1:9999")
	got = buf.String()
	if strings.Contains(got, "127.0.0.1") {
		t.Errorf("ErrorLog leaked loopback IP: %s", got)
	}
	if !strings.Contains(got, "loopback:9999") {
		t.Errorf("ErrorLog missing loopback port: %s", got)
	}
}

func TestIngestReadErrorOmitsPeerAddress(t *testing.T) {
	for _, prefix := range []string{"", "{\"agent\":\"coder\"}\n"} {
		t.Run(fmt.Sprintf("prefix=%d", len(prefix)), func(t *testing.T) {
			lg, buf := captureLogger()
			rec := &memRecorder{}
			s := &Server{rec: rec, log: lg}
			readErr := &net.OpError{
				Op:   "read",
				Net:  "tcp",
				Addr: &net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 54321},
				Err:  io.ErrClosedPipe,
			}
			body := io.MultiReader(strings.NewReader(prefix), iotest.ErrReader(readErr))
			r := httptest.NewRequest(http.MethodPost, "/v1/events", body)
			r.RemoteAddr = readErr.Addr.String()
			r.Header.Set("X-Request-Id", "read-error-test")
			w := httptest.NewRecorder()
			s.wrap(http.HandlerFunc(s.handlePost)).ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
			for _, output := range []string{buf.String(), w.Body.String()} {
				if strings.Contains(output, "203.0.113.9") {
					t.Errorf("read error leaked peer IP: %s", output)
				}
				if !strings.Contains(output, "request body read failed") {
					t.Errorf("missing read failure: %s", output)
				}
			}
			accepted := 0
			if prefix != "" {
				accepted = 1
				if !strings.Contains(w.Body.String(), "1 earlier event in this stream was recorded") {
					t.Errorf("missing partial acceptance: %s", w.Body.String())
				}
			}
			if len(rec.evs) != accepted {
				t.Errorf("events = %d, want %d", len(rec.evs), accepted)
			}
			for _, field := range []string{"level=WARN", "status=400", "req=read-error-test", fmt.Sprintf("accepted=%d", accepted)} {
				if !strings.Contains(buf.String(), field) {
					t.Errorf("missing log field %s: %s", field, buf.String())
				}
			}
		})
	}
}

func TestIngestLogRedactsPeerAddress(t *testing.T) {
	lg, buf := captureLogger()
	s := &Server{rec: &memRecorder{}, log: lg}

	r := httptest.NewRequest(http.MethodPost, "/v1/events",
		strings.NewReader(`{"agent":"x"}`))
	r.RemoteAddr = "203.0.113.9:54321"
	w := httptest.NewRecorder()
	s.handlePost(w, r)

	got := buf.String()
	if strings.Contains(got, "203.0.113.9") {
		t.Errorf("logged peer IP: %s", got)
	}
	if !strings.Contains(got, "remote=remote") {
		t.Errorf("want redacted remote, got %s", got)
	}

	buf.Reset()
	r = httptest.NewRequest(http.MethodPost, "/v1/events",
		strings.NewReader(`{"agent":"x"}`))
	r.RemoteAddr = "127.0.0.1:9999"
	w = httptest.NewRecorder()
	s.handlePost(w, r)
	got = buf.String()
	if strings.Contains(got, "127.0.0.1") {
		t.Errorf("logged loopback IP: %s", got)
	}
	if !strings.Contains(got, "remote=loopback:9999") {
		t.Errorf("want loopback port, got %s", got)
	}
}

func TestIngestEchoesRequestID(t *testing.T) {
	lg, buf := captureLogger()
	s := startIngestLog(t, &memRecorder{}, lg)

	req, err := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/v1/events",
		strings.NewReader(`{"agent":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Request-Id", "harness-turn-9")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if got := resp.Header.Get("X-Request-Id"); got != "harness-turn-9" {
		t.Errorf("X-Request-Id = %q, want harness-turn-9", got)
	}
	if !strings.Contains(buf.String(), "req=harness-turn-9") {
		t.Errorf("log missing echoed req id: %s", buf.String())
	}
}

// A request-id header is attacker-shaped: newlines would split the audit line
// and let a sender forge a second slog record. net/http rejects CR/LF on the
// wire, so this drives the handler directly the way fuzz does.
func TestIngestRequestIDStaysOneLogLine(t *testing.T) {
	lg, buf := captureLogger()
	s := &Server{rec: &memRecorder{}, log: lg}

	r := httptest.NewRequest(http.MethodPost, "/v1/events",
		strings.NewReader(`{"agent":"x"}`))
	r.Header["X-Request-Id"] = []string{"id-1\nlevel=INFO forged"}
	w := httptest.NewRecorder()
	s.handlePost(w, r)

	got := buf.String()
	if countLogLines(got) != 1 {
		t.Fatalf("injected request-id split the log: %q", got)
	}
	if strings.Contains(got, "\nlevel=INFO forged") {
		t.Errorf("newline survived into log: %q", got)
	}
	echo := w.Header().Get("X-Request-Id")
	if strings.ContainsAny(echo, "\r\n") {
		t.Errorf("X-Request-Id echoed a newline: %q", echo)
	}
}

func TestHealthzIsNotLogged(t *testing.T) {
	lg, buf := captureLogger()
	s := startIngestLog(t, &memRecorder{}, lg)

	resp, err := http.Get("http://" + s.Addr() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("healthz missing X-Request-Id")
	}
	req, err := http.NewRequest(http.MethodHead, "http://"+s.Addr()+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	head, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, head.Body)
	head.Body.Close()
	if head.StatusCode != http.StatusOK {
		t.Fatalf("HEAD /healthz = %d", head.StatusCode)
	}
	if buf.Len() != 0 {
		t.Errorf("healthz must not log, got %q", buf.String())
	}
}

func TestIngestUnknownPathNamesEndpoints(t *testing.T) {
	s := startIngest(t, &memRecorder{})

	resp, err := http.Post("http://"+s.Addr()+"/events", "application/json",
		strings.NewReader(`{"agent":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	got := string(body)
	for _, want := range []string{"/v1/events", "/healthz", "POST"} {
		if !strings.Contains(got, want) {
			t.Errorf("404 body %q missing %q", got, want)
		}
	}
}

func TestIngestLogsUnknownPath(t *testing.T) {
	lg, buf := captureLogger()
	s := startIngestLog(t, &memRecorder{}, lg)

	resp, err := http.Post("http://"+s.Addr()+"/events", "application/json",
		strings.NewReader(`{"agent":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	reqID := resp.Header.Get("X-Request-Id")
	got := buf.String()
	if countLogLines(got) != 1 {
		t.Fatalf("404 log lines = %d (%q), want 1", countLogLines(got), got)
	}
	for _, want := range []string{
		"level=WARN",
		"method=POST",
		"path=/events",
		"status=404",
		"accepted=0",
		`error="not found"`,
		"req=" + reqID,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("404 log missing %q: %s", want, got)
		}
	}
}

func TestIngestLogsWrongMethod(t *testing.T) {
	lg, buf := captureLogger()
	s := startIngestLog(t, &memRecorder{}, lg)

	req, err := http.NewRequest(http.MethodPut, "http://"+s.Addr()+"/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	got := buf.String()
	if countLogLines(got) != 1 {
		t.Fatalf("405 log lines = %d (%q), want 1", countLogLines(got), got)
	}
	for _, want := range []string{
		"level=WARN",
		"method=PUT",
		"path=/v1/events",
		"status=405",
		`error="method not allowed"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("405 log missing %q: %s", want, got)
		}
	}
}

type panicRecorder struct{}

func (panicRecorder) RecordAgent(core.AgentEvent) { panic("recorder boom") }

func TestIngestLogsHandlerPanic(t *testing.T) {
	lg, buf := captureLogger()
	s, err := newServer("127.0.0.1:0", panicRecorder{}, lg)
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve()
	t.Cleanup(func() { s.Close() })

	resp, err := http.Post("http://"+s.Addr()+"/v1/events", "application/json",
		strings.NewReader(`{"agent":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if strings.Contains(string(body), "recorder boom") {
		t.Errorf("panic leaked to client: %q", body)
	}
	got := buf.String()
	if countLogLines(got) != 1 {
		t.Fatalf("panic log lines = %d (%q), want 1", countLogLines(got), got)
	}
	for _, want := range []string{
		"level=ERROR",
		"method=POST",
		"path=/v1/events",
		"status=500",
		`error="panic: recorder boom"`,
		"stack=",
		"req=" + resp.Header.Get("X-Request-Id"),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("panic log missing %q: %s", want, got)
		}
	}
}

type partialPanicRecorder struct{ memRecorder }

func (m *partialPanicRecorder) RecordAgent(ev core.AgentEvent) {
	if len(m.evs) == 1 {
		panic("recorder boom")
	}
	m.memRecorder.RecordAgent(ev)
}

func TestIngestPanicLogsPartialProgress(t *testing.T) {
	lg, buf := captureLogger()
	rec := &partialPanicRecorder{}
	s := &Server{rec: rec, log: lg}
	r := httptest.NewRequest(http.MethodPost, "/v1/events",
		strings.NewReader("{\"agent\":\"first\"}\n{\"agent\":\"second\"}"))
	r.Header.Set("X-Request-Id", "partial-stream")
	w := httptest.NewRecorder()
	s.wrap(http.HandlerFunc(s.handlePost)).ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError || len(rec.evs) != 1 {
		t.Fatalf("status = %d, recorded = %d; want 500, 1", w.Code, len(rec.evs))
	}
	got := buf.String()
	if countLogLines(got) != 1 {
		t.Fatalf("panic log lines = %d (%q), want 1", countLogLines(got), got)
	}
	for _, want := range []string{
		"level=ERROR", "status=500", "accepted=1", "req=partial-stream",
		"duration=", "stack=", `error="panic: recorder boom"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("partial panic log missing %q: %s", want, got)
		}
	}
}

type failWriter struct{ http.ResponseWriter }

func (failWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type netErrBody struct{ err error }

func (b netErrBody) Read([]byte) (int, error) { return 0, b.err }

func TestIngestLogsBodyReadFailure(t *testing.T) {
	lg, buf := captureLogger()
	s := &Server{rec: &memRecorder{}, log: lg}

	peer := &net.OpError{Op: "read", Net: "tcp", Err: io.ErrClosedPipe}
	r := httptest.NewRequest(http.MethodPost, "/v1/events", netErrBody{err: peer})
	w := httptest.NewRecorder()
	s.handlePost(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	got := buf.String()
	if countLogLines(got) != 1 {
		t.Fatalf("read-fail log lines = %d (%q), want 1", countLogLines(got), got)
	}
	for _, want := range []string{
		"level=WARN",
		"status=400",
		"accepted=0",
		`error="request body read failed"`,
		`body_error="read tcp: io: read/write on closed pipe"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("read-fail log missing %q: %s", want, got)
		}
	}
}

func TestIngestLogsResponseWriteFailure(t *testing.T) {
	lg, buf := captureLogger()
	s := &Server{rec: &memRecorder{}, log: lg}

	r := httptest.NewRequest(http.MethodPost, "/v1/events",
		strings.NewReader(`{"agent":"x"}`))
	w := httptest.NewRecorder()
	s.handlePost(failWriter{w}, r)

	got := buf.String()
	if countLogLines(got) != 1 {
		t.Fatalf("write-fail log lines = %d (%q), want 1", countLogLines(got), got)
	}
	for _, want := range []string{
		"level=WARN",
		"status=202",
		"accepted=1",
		`error="response write failed: ` + io.ErrClosedPipe.Error() + `"`,
		"method=POST",
		"path=/v1/events",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("write-fail log missing %q: %s", want, got)
		}
	}
}

func TestIngestLogsRejectedResponseWriteFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		origin   string
		status   int
		accepted int
		reason   string
	}{
		{"empty", "", "", http.StatusBadRequest, 0, "empty body"},
		{"partial", "{\"agent\":\"first\"}\nnull", "", http.StatusBadRequest, 1, "bad json"},
		{"origin", `{"agent":"first"}`, "https://example.test", http.StatusForbidden, 0, "browser-originated requests"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lg, buf := captureLogger()
			rec := &memRecorder{}
			s := &Server{rec: rec, log: lg}
			r := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(tc.body))
			r.Header.Set("X-Request-Id", "failed-rejection")
			r.Header.Set("Origin", tc.origin)
			w := httptest.NewRecorder()
			s.wrap(http.HandlerFunc(s.handlePost)).ServeHTTP(failWriter{w}, r)
			if w.Code != tc.status || len(rec.evs) != tc.accepted {
				t.Fatalf("status = %d, recorded = %d; want %d, %d", w.Code, len(rec.evs), tc.status, tc.accepted)
			}
			got := buf.String()
			if countLogLines(got) != 1 {
				t.Fatalf("log lines = %d (%q), want 1", countLogLines(got), got)
			}
			for _, want := range []string{
				"level=WARN", "req=failed-rejection", "duration=", "method=POST", "path=/v1/events",
				fmt.Sprintf("status=%d", tc.status), fmt.Sprintf("accepted=%d", tc.accepted),
				`error="` + tc.reason, `response_error="` + io.ErrClosedPipe.Error() + `"`,
			} {
				if !strings.Contains(got, want) {
					t.Errorf("rejection log missing %q: %s", want, got)
				}
			}
		})
	}
}

type networkErrorWriter struct {
	http.ResponseWriter
	err error
}

func (w networkErrorWriter) Write([]byte) (int, error) { return 0, w.err }

func TestIngestResponseWriteErrorsRedactPeerAddresses(t *testing.T) {
	for _, peer := range []struct {
		addr string
		want string
	}{
		{"203.0.113.17:49152", "remote"},
		{"[2001:db8::17]:49152", "remote"},
		{"[fe80::17%eth0]:49152", "remote"},
		{"127.0.0.1:49152", "loopback:49152"},
	} {
		for _, tc := range []struct {
			name     string
			body     string
			status   int
			accepted int
			attr     string
		}{
			{"accepted", `{"agent":"x"}`, http.StatusAccepted, 1, `error="response write failed: `},
			{"rejected", "", http.StatusBadRequest, 0, `response_error="`},
			{"partial", "{\"agent\":\"x\"}\nnull", http.StatusBadRequest, 1, `response_error="`},
		} {
			t.Run(peer.addr+"/"+tc.name, func(t *testing.T) {
				addr, err := net.ResolveTCPAddr("tcp", peer.addr)
				if err != nil {
					t.Fatal(err)
				}
				writeErr := &net.OpError{
					Op: "write", Net: "tcp",
					Source: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8420},
					Addr:   addr, Err: io.ErrClosedPipe,
				}
				lg, buf := captureLogger()
				rec := &memRecorder{}
				s := &Server{rec: rec, log: lg}
				r := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(tc.body))
				r.RemoteAddr = peer.addr
				r.Header.Set("X-Request-Id", "write-failure")
				w := httptest.NewRecorder()
				s.wrap(http.HandlerFunc(s.handlePost)).ServeHTTP(networkErrorWriter{w, writeErr}, r)
				if w.Code != tc.status || len(rec.evs) != tc.accepted {
					t.Fatalf("status = %d, recorded = %d; want %d, %d", w.Code, len(rec.evs), tc.status, tc.accepted)
				}
				got := buf.String()
				if countLogLines(got) != 1 || strings.Contains(got, addr.IP.String()) {
					t.Fatalf("peer address leaked or log split: %q", got)
				}
				for _, want := range []string{
					"level=WARN", "req=write-failure", "method=POST", "path=/v1/events", "duration=",
					fmt.Sprintf("status=%d", tc.status), fmt.Sprintf("accepted=%d", tc.accepted),
					tc.attr + "write tcp loopback:8420->" + peer.want + ": " + io.ErrClosedPipe.Error() + `"`,
				} {
					if !strings.Contains(got, want) {
						t.Errorf("write-failure log missing %q: %s", want, got)
					}
				}
			})
		}
	}
}

func TestIngestUnknownPathStaysOneLogLine(t *testing.T) {
	lg, buf := captureLogger()
	s, err := newServer("127.0.0.1:0", &memRecorder{}, lg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.URL.Path = "/x\nlevel=INFO forged"
	w := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(w, r)

	got := buf.String()
	if countLogLines(got) != 1 {
		t.Fatalf("injected path split the log: %q", got)
	}
	if strings.Contains(got, "\nlevel=INFO forged") {
		t.Errorf("newline survived into log: %q", got)
	}
}

func TestIngestLogsBrowserOriginRefusal(t *testing.T) {
	lg, buf := captureLogger()
	s := startIngestLog(t, &memRecorder{}, lg)

	req, err := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/v1/events",
		strings.NewReader(`{"agent":"forger"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	got := buf.String()
	if !strings.Contains(got, "status=403") || !strings.Contains(got, "level=WARN") {
		t.Errorf("403 not logged as warn: %s", got)
	}
}

// Close must release the listen socket even when Serve has not tracked it
// yet. New binds immediately so Addr can report the port; a Close that only
// talks to http.Server would leave the fd bound until process exit.
func TestCloseReleasesListenerWithoutServe(t *testing.T) {
	s, err := New("127.0.0.1:0", &memRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	addr := s.Addr()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listener still bound after Close without Serve: %v", err)
	}
	ln.Close()
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{in: "", want: slog.LevelInfo},
		{in: "info", want: slog.LevelInfo},
		{in: "INFO", want: slog.LevelInfo},
		{in: " debug ", want: slog.LevelDebug},
		{in: "warn", want: slog.LevelWarn},
		{in: "warning", want: slog.LevelWarn},
		{in: "error", want: slog.LevelError},
		{in: "trace", wantErr: true},
		{in: "inf", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseLogLevel(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseLogLevel(%q) = %v, want error", tt.in, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("ParseLogLevel(%q) = %v, %v, want %v, nil", tt.in, got, err, tt.want)
		}
	}
}

func TestNewIngestLoggerHonorsLogLevel(t *testing.T) {
	t.Setenv(LogLevelEnv, "error")
	lg := newIngestLogger()
	if !lg.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("error should be enabled")
	}
	if lg.Enabled(context.Background(), slog.LevelWarn) {
		t.Fatal("warn should be disabled at error floor")
	}
}
