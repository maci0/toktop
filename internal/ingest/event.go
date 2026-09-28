package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/rivo/uniseg"

	"github.com/maci0/toktop/internal/core"
)

// eventFromWire sanitizes one decoded event. Timestamp clamping against
// arrival time stays in decodeStream: that bound is a property of the request,
// not of the wire object.
func eventFromWire(wire agentEventWire) (core.AgentEvent, error) {
	at, err := parseEventTime(wire.At)
	if err != nil {
		return core.AgentEvent{}, err
	}
	id, err := wireEventID(wire.ID)
	if err != nil {
		return core.AgentEvent{}, err
	}
	prompt, output, thinking, err := parseTokenFields(wire)
	if err != nil {
		return core.AgentEvent{}, err
	}
	span, err := parseSpanFields(wire)
	if err != nil {
		return core.AgentEvent{}, err
	}
	ev := core.AgentEvent{
		At:             at,
		ID:             id,
		Agent:          wire.Agent,
		Model:          wire.Model,
		Kind:           wire.Kind,
		PromptTokens:   prompt,
		OutputTokens:   output,
		ThinkingTokens: thinking,
		ViaEngine:      wire.ViaEngine,
		Note:           wire.Note,
		Span:           span,
	}
	// Event fields are attacker-shaped text (any local process or peer
	// able to reach this endpoint): strip terminal escape sequences and
	// control characters, and fold to one line, before the values are
	// stored and later rendered into a cell of a row.
	// Defaults come after sanitization: a value the sanitizer empties
	// (pure escape sequences) must not slip past the fallback.
	ev.Agent = core.AgentNameField(ev.Agent)
	ev.Model = core.ClampField(core.SingleLine(ev.Model), core.AgentModelMax)
	ev.ViaEngine = core.ClampField(core.SingleLine(ev.ViaEngine), core.AgentViaMax)
	// Free-form fields are capped so one giant event cannot dominate the
	// retained feed, and the note gets the same treatment a locally watched
	// working directory gets (core.ShortDir): a note naming a working
	// directory is the one event field that routinely carries a path, and a
	// path under $HOME names the account. It reaches the feed, the live
	// dashboard and the --once --plain report, which is often redirected
	// into a file or a journal.
	ev.Note = core.ClampField(core.RedactHome(shortNote(core.SingleLine(ev.Note))), core.AgentNoteMax)
	// Token counts are unsigned quantities; negative or absurd values
	// are junk from a misbehaving sender and must not enter the
	// retained feed (summing MaxInt64 across events wraps the totals).
	ev.PromptTokens = core.ClampEventTokens(ev.PromptTokens)
	ev.OutputTokens = core.ClampEventTokens(ev.OutputTokens)
	ev.ThinkingTokens = core.ClampEventTokens(ev.ThinkingTokens)
	switch ev.Kind {
	case core.AgentKindTurn, core.AgentKindTool, core.AgentKindError, core.AgentKindNote:
	default:
		// core.FoldASCII, not strings.ToLower: the four kinds are ASCII
		// literals, and a sender is free to spell one with U+0130 or U+212A.
		// ToLower would fold those into the ASCII letter and store the event
		// under a kind the sender never wrote, so the feed's kind column
		// (core.AgentKindError drives a red row) reads as something the
		// wire did not say.
		ev.Kind = core.ClampField(core.SingleLine(core.FoldASCII(ev.Kind)), core.AgentKindMax)
	}
	if ev.Kind == "" {
		ev.Kind = core.AgentKindTurn
	}
	return ev, nil
}

// shortNote reduces a note that is nothing but a working directory to the
// last two components, the way a locally watched agent's directory is. A
// pushed path reaches the feed whole otherwise, and everything above the
// checkout is where a client's name and a project index sit: names of
// people and organizations the operator never meant to publish into a
// dashboard they redirect into a file.
//
// A note carrying anything else is free text and is left as the sender wrote
// it, past the home fold the caller applies. A path with a space in it is
// free text as much as a path, and the two spellings cannot be told apart
// here; that one keeps the home fold alone.
func shortNote(note string) string {
	if !pathNote(note) {
		return note
	}
	return core.ShortDir(note)
}

// pathNote reports whether a note is a bare path: one token, carrying a
// separator or spelled from home, and free of the "·" the feed separates its
// own parts with. Prose has spaces in it; a path that has a space in it is
// indistinguishable from prose, and shortening that would cut a sentence in
// half.
func pathNote(note string) bool {
	if note == "" || strings.ContainsAny(note, " \t·") {
		return false
	}
	return strings.ContainsAny(note, `/\`) || strings.HasPrefix(note, "~")
}

// wireEventID is the stored form of a caller-supplied event id. An absent id
// is not an error: the endpoint derives one from the POST's Idempotency-Key,
// and the handler only reaches here for an id the sender did write.
//
// A supplied id that cannot be stored whole is refused rather than clamped.
// The id is the dedup key, so clamping one past the cap folds every key with
// that prefix onto a single stored id, and the second event is dropped as a
// duplicate of the first: the sender loses a turn and the answer's
// accepted/stored pair reads as a replay. The same is true of an id that
// sanitizes to nothing, which leaves the event with no key at all and is
// counted again on every replay. Both are answered the way an out-of-range
// token count is: a 400 naming the field.
func wireEventID(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	line := core.SingleLine(raw)
	// Clusters, not bytes or runes: the cap is a display cap the feed applies
	// in clusters, so an id of 129 flags is over it and an id of 129 bytes of
	// a two-byte rune is not.
	if uniseg.GraphemeClusterCount(line) > core.AgentIDMax {
		return "", fmt.Errorf("bad id: must be at most %d characters", core.AgentIDMax)
	}
	id := core.ClampField(line, core.AgentIDMax)
	if id == "" {
		return "", errors.New("bad id: must be at least one printable character")
	}
	return id, nil
}

// agentEventWire mirrors core.AgentEvent for decoding, with ts and token
// counts carried raw: encoding/json's time parser demands an explicit offset,
// and int64 rejects whole JSON numbers such as 100.0, so a well-formed but
// offset-less stamp or a Python-dumped float would abort the whole stream
// with 400 before parseEventTime / parseTokenJSON could apply the documented
// forms.
type agentEventWire struct {
	At             json.RawMessage `json:"ts"`
	ID             string          `json:"id"`
	Agent          string          `json:"agent"`
	Model          string          `json:"model"`
	Kind           string          `json:"kind"`
	PromptTokens   json.RawMessage `json:"prompt_tokens"`
	OutputTokens   json.RawMessage `json:"output_tokens"`
	ThinkingTokens json.RawMessage `json:"thinking_tokens"`
	ViaEngine      string          `json:"via_engine"`
	Note           string          `json:"note"`
	// SpanMs is how long the model spent on this event's tokens, in
	// milliseconds, carried raw for the same reason as the token counts: a
	// sender that dumps a float should not abort the stream. core.AgentEvent
	// has held Span since the local watcher began setting it, and a sender
	// arriving over HTTP could not report one, so a pushed agent's rate was
	// always the gap between events while a locally watched one was the
	// model's own time. Zero, or absent, keeps the documented meaning: the
	// gap between events.
	SpanMs json.RawMessage `json:"span_ms"`
}

// parseEventTime decodes an event's ts field as RFC 3339 (offset required).
// Absent, null, or whitespace-only yields the zero Time; the caller stamps
// arrival. Anything else is errBadTS.
func parseEventTime(raw json.RawMessage) (time.Time, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return time.Time{}, nil
	}
	v, err := strconv.Unquote(s)
	if err != nil {
		return time.Time{}, errBadTS
	}
	if strings.TrimSpace(v) == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, errBadTS
	}
	return t, nil
}

var errBadTS = errors.New("must be an RFC 3339 string")

// parseTokenFields reads the three token counts. Whole JSON numbers (100.0,
// 1e2) count as integers; a fractional remainder or a non-number is 400.
func parseTokenFields(w agentEventWire) (prompt, output, thinking int64, err error) {
	if prompt, err = parseTokenJSON(w.PromptTokens, "prompt_tokens"); err != nil {
		return
	}
	if output, err = parseTokenJSON(w.OutputTokens, "output_tokens"); err != nil {
		return
	}
	thinking, err = parseTokenJSON(w.ThinkingTokens, "thinking_tokens")
	return
}

// parseSpanFields reads the model-reported duration in milliseconds and
// returns it clamped to core.MaxEventSpan. Absent, null or whitespace-only is
// zero, which keeps the documented meaning: the rate is the gap between
// events. A non-number is the same 400 the token counts use, but a number
// outside the bound is dropped to zero rather than refused, because a span
// only ever scales a rate down and the three token fields set the precedent
// of clamping rather than rejecting.
func parseSpanFields(w agentEventWire) (time.Duration, error) {
	ms, err := parseTokenJSON(w.SpanMs, "span_ms")
	if err != nil {
		return 0, err
	}
	return core.ClampEventSpan(time.Duration(ms) * time.Millisecond), nil
}

func parseTokenJSON(raw json.RawMessage, field string) (int64, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, nil
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) && math.Trunc(f) == f {
		if f >= math.MaxInt64 || f < math.MinInt64 {
			return 0, fmt.Errorf("bad json: %s is out of range", field)
		}
		return int64(f), nil
	}
	return 0, fmt.Errorf("bad json: %s must be an integer", field)
}

// jsonRootKind names the JSON value kind of a decoded raw message so a
// non-object root (null, array, string, number) can be refused with the
// same "expected a JSON object" wording Decode-into-struct already used
// for arrays. encoding/json treats null as a zero struct, which is why
// this check sits in front of Unmarshal.
//
// The leading byte answers it, so the raw slice is scanned in place: a
// stream of thousands of events would otherwise copy and trim every line a
// second time before the parse that actually reads it.
func jsonRootKind(raw []byte) string {
	i := 0
	for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\n' || raw[i] == '\r') {
		i++
	}
	if i == len(raw) {
		return "empty"
	}
	switch raw[i] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "bool"
	case 'n':
		return "null"
	default:
		return "number"
	}
}
