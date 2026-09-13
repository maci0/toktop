package ingest

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rivo/uniseg"

	"github.com/maci0/toktop/internal/core"
)

// FuzzHandlePost drives the full POST /v1/events pipeline with arbitrary
// bytes: JSON/NDJSON decode, timestamp parsing, sanitization, clamping and
// recording, including every error path (bad JSON, bad ts, size cap). The
// event feed is retained for the process lifetime and rendered to the
// terminal, so anything recorded must obey the boundary guarantees: fields
// are escape-free and character-capped, token counts non-negative, defaults
// applied, and only documented status codes leave the handler.
func FuzzHandlePost(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{"agent":"coder","kind":"tool","prompt_tokens":4200,"output_tokens":310,"note":"shell(git status)"}`),
		[]byte(`{"agent":"coder","via_engine":"127.0.0.1:11434","output_tokens":50}`),
		[]byte(`{"via_engine":"127.0.0.1:11434\u001b]0;x\u0007"}`),
		[]byte(`{"id":"turn-1","agent":"coder","output_tokens":50}`),
		[]byte(`{"id":"` + strings.Repeat("a", 300) + `","agent":"x"}`),
		[]byte("{\"agent\":\"a\",\"ts\":\"2026-01-02T03:04:05Z\"}\n{\"agent\":\"b\",\"ts\":\"2026-01-02T05:04:05+02:00\"}"),
		[]byte(`{"agent":"py","ts":"2026-01-02T03:04:05.123456Z"}`),
		[]byte(`{"agent":"x","ts":"yesterday"}`),
		[]byte(`{"agent":"x","ts":""}`),
		[]byte(`{"agent":"x","ts":null}`),
		[]byte(`{"agent":"x","ts":123}`),
		[]byte(`{not json`),
		nil,
		[]byte("\n\n"),
		[]byte(`{"kind":"\u001b]0;pwned\u0007weird"}`),
		[]byte(`{"agent":"esc\u001b[2Jclear","model":"\u009bhidden"}`),
		[]byte(`{"agent":"clau\u200bde\u202eedualc"}`),
		[]byte(`{"id":"cafe\u0301","agent":"cafe\u0301"}`),
		[]byte(`{"agent":"clau\ufe00de"}`),
		[]byte(`{"agent":"foo\u2028bar"}`),
		[]byte(`{"agent":"` + strings.Repeat("a", 300) + `"}`),
		[]byte(`{"agent":"` + strings.Repeat("é", 300) + `"}`),
		[]byte(`{"agent":"` + strings.Repeat("\U0001F1E9\U0001F1EA", 80) + `"}`),
		[]byte("{\"agent\":\"clau\U000E0064e\"}"),
		[]byte(`{"prompt_tokens":-500,"output_tokens":-99999999999}`),
		[]byte(`{"agent":["array"],"note":{"obj":true}}`),
		[]byte(`{"agent":"x"} trailing garbage`),
		[]byte(`[[[[[[[[["deep"]]]]]]]]]`),
		[]byte("{\"agent\":\"\xff\xfe\"}"),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		rec := &memRecorder{}
		s := &Server{rec: rec}

		r := httptest.NewRequest(http.MethodPost, "/v1/events", bytes.NewReader(body))
		w := httptest.NewRecorder()
		s.handlePost(w, r)

		respBody := w.Body.String()
		for _, leaked := range []string{"agentEventWire", "Go struct field", "Go value of type", "2006-01-02T15:04:05"} {
			if strings.Contains(respBody, leaked) {
				t.Fatalf("response leaked internals %q: %q", leaked, respBody)
			}
		}
		switch w.Code {
		case http.StatusAccepted:
			if len(rec.evs) == 0 {
				t.Fatal("202 accepted but no event recorded")
			}
			// Events written to the wire must match those actually recorded.
			var ack int
			if n, _ := fmt.Sscanf(respBody, `{"accepted":%d}`, &ack); n != 1 || ack != len(rec.evs) {
				t.Fatalf("ack %q vs %d recorded events", respBody, len(rec.evs))
			}
		case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
			// A mid-stream failure keeps the events decoded before it; each
			// one still has to satisfy the boundary checks below.
		default:
			t.Fatalf("unexpected status %d for body %q", w.Code, body)
		}

		for i, ev := range rec.evs {
			if ev.Agent == "" {
				t.Errorf("event %d: empty agent must default", i)
			}
			if ev.Kind == "" {
				t.Errorf("event %d: empty kind must default", i)
			}
			if ev.At.IsZero() {
				t.Errorf("event %d: zero timestamp must be stamped", i)
			}
			if ev.PromptTokens < 0 || ev.OutputTokens < 0 || ev.ThinkingTokens < 0 {
				t.Errorf("event %d: negative token counts retained: %+v", i, ev)
			}
			if n := uniseg.GraphemeClusterCount(ev.Agent); n > 64 {
				t.Errorf("event %d: agent = %d characters, cap 64", i, n)
			}
			if n := uniseg.GraphemeClusterCount(ev.Model); n > 128 {
				t.Errorf("event %d: model = %d characters, cap 128", i, n)
			}
			if n := uniseg.GraphemeClusterCount(ev.Note); n > 512 {
				t.Errorf("event %d: note = %d characters, cap 512", i, n)
			}
			if n := uniseg.GraphemeClusterCount(ev.Kind); n > 24 {
				t.Errorf("event %d: kind = %d characters, cap 24", i, n)
			}
			if n := uniseg.GraphemeClusterCount(ev.ID); n > 128 {
				t.Errorf("event %d: id = %d characters, cap 128", i, n)
			}
			if n := uniseg.GraphemeClusterCount(ev.ViaEngine); n > 128 {
				t.Errorf("event %d: via_engine = %d characters, cap 128", i, n)
			}
			assertRenderSafe(t, i, "agent", ev.Agent)
			assertRenderSafe(t, i, "model", ev.Model)
			assertRenderSafe(t, i, "note", ev.Note)
			assertRenderSafe(t, i, "kind", ev.Kind)
			assertRenderSafe(t, i, "id", ev.ID)
			assertRenderSafe(t, i, "via_engine", ev.ViaEngine)
		}
	})
}

// assertRenderSafe fails if s still holds anything SanitizeText is supposed
// to strip: ESC bytes, C0 controls other than the preserved newline/tab,
// DEL, C1 controls, bidi marks, tag characters, or other format runes.
func assertRenderSafe(t *testing.T, i int, field, s string) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Fatalf("event %d: %s is not valid UTF-8: %q", i, field, s)
	}
	if got := core.SanitizeText(s); got != s {
		t.Fatalf("event %d: %s retains unsanitized text: %q -> %q", i, field, s, got)
	}
}
