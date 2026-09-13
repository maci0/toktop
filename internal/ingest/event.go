package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// eventFromWire sanitizes one decoded event. Timestamp clamping against
// arrival time stays in handlePost: that bound is a property of the request,
// not of the wire object.
func eventFromWire(wire agentEventWire) (core.AgentEvent, error) {
	at, err := parseEventTime(wire.At)
	if err != nil {
		return core.AgentEvent{}, err
	}
	prompt, output, thinking, err := parseTokenFields(wire)
	if err != nil {
		return core.AgentEvent{}, err
	}
	ev := core.AgentEvent{
		At:             at,
		ID:             wire.ID,
		Agent:          wire.Agent,
		Model:          wire.Model,
		Kind:           wire.Kind,
		PromptTokens:   prompt,
		OutputTokens:   output,
		ThinkingTokens: thinking,
		ViaEngine:      wire.ViaEngine,
		Note:           wire.Note,
	}
	// Event fields are attacker-shaped text (any local process or peer
	// able to reach this endpoint): strip terminal escape sequences and
	// control characters before the values are stored and later rendered.
	// Defaults come after sanitization: a value the sanitizer empties
	// (pure escape sequences) must not slip past the fallback.
	ev.ID = core.ClampField(core.SanitizeText(ev.ID), 128)
	ev.Agent = core.ClampField(core.SanitizeText(ev.Agent), 64)
	// Mixed Latin+Cyrillic/Greek names spoof a real agent in the feed
	// ("сlaude" vs "claude"). Collapse them to the anonymous default.
	if core.MixedScriptIdentity(ev.Agent) {
		ev.Agent = ""
	}
	if ev.Agent == "" {
		ev.Agent = "anonymous"
	}
	ev.Model = core.ClampField(core.SanitizeText(ev.Model), 128)
	ev.ViaEngine = core.ClampField(core.SanitizeText(ev.ViaEngine), 128)
	ev.Note = core.ClampField(core.SanitizeText(ev.Note), 512) // free-form fields are capped so one giant event cannot dominate the retained feed
	// Token counts are unsigned quantities; negative or absurd values
	// are junk from a misbehaving sender and must not enter the
	// retained feed (summing MaxInt64 across events wraps the totals).
	ev.PromptTokens = clampTokens(ev.PromptTokens)
	ev.OutputTokens = clampTokens(ev.OutputTokens)
	ev.ThinkingTokens = clampTokens(ev.ThinkingTokens)
	switch ev.Kind {
	case core.AgentKindTurn, core.AgentKindTool, core.AgentKindError, core.AgentKindNote:
	default:
		ev.Kind = core.ClampField(core.SanitizeText(strings.ToLower(ev.Kind)), 24)
	}
	if ev.Kind == "" {
		ev.Kind = core.AgentKindTurn
	}
	return ev, nil
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
		if f > math.MaxInt64 || f < math.MinInt64 {
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
func jsonRootKind(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "empty"
	}
	switch s[0] {
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

// maxEventTokens bounds a token count on one event. Real usage never
// approaches it; a sender claiming more is lying or broken, and summing
// MaxInt64 values across the retained feed would wrap the agent totals.
const maxEventTokens = 1 << 40

func clampTokens(n int64) int64 {
	if n < 0 || n > maxEventTokens {
		return 0
	}
	return n
}
