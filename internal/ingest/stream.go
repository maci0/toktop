package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"

	"github.com/maci0/toktop/internal/logcfg"
)

// streamResult is how a decoded body ended. status is zero for a clean end of
// stream, and carries the status to answer the sender with otherwise. keyed
// records whether the last id-less line's id came from the POST key and that
// line's position, which is what decides the sender's recovery hint.
type streamResult struct {
	decoded int
	stored  int
	keyed   bool
	status  int
	msg     string
	extra   []any
}

// decodeStream decodes the request body one JSON object at a time, recording
// each as it goes, and reports how the stream ended. Events are recorded
// before the next line is read, so a body that fails halfway leaves everything
// before the failing line in the feed; the returned counts say so.
//
// keyPrefix is derivedKeyPrefix(replayKey), computed once by the caller for the
// whole body and reused for every id-less line.
func (s *Server) decodeStream(dec *json.Decoder, progress *progressBody, replayKey, keyPrefix string, state *requestState) streamResult {
	var r streamResult
	// keyed tracks whether the last id-less line had an identity derived from
	// the POST key and its position in the body. Such a line can only be
	// recovered by replaying the whole request.
	var keyed bool
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if r.decoded > 0 && errors.Is(err, io.EOF) {
				return r // clean end of stream after at least one event
			}
			switch {
			case errors.Is(err, io.EOF):
				r.status, r.msg = http.StatusBadRequest, "empty body: expected one JSON object or an NDJSON stream"
			case errors.Is(err, os.ErrDeadlineExceeded):
				r.status, r.msg = http.StatusRequestTimeout, progress.stallReason()
			default:
				return r.decodeFailure(err, keyed)
			}
			return r
		}
		// Null unmarshals into a struct as zeros; the body must be an object.
		if kind := jsonRootKind(raw); kind != "object" {
			r.status = http.StatusBadRequest
			r.msg = "bad json: expected a JSON object or NDJSON stream, got " + kind
			return r
		}
		var wire agentEventWire
		if err := json.Unmarshal(raw, &wire); err != nil {
			return r.decodeFailure(err, keyed)
		}
		ev, err := eventFromWire(wire)
		if err != nil {
			r.status, r.msg = http.StatusBadRequest, err.Error()
			if errors.Is(err, errBadTS) {
				r.msg = "bad ts: " + r.msg
			}
			return r
		}
		now := s.instant()
		if ev.At.IsZero() {
			ev.At = now
		} else if ev.At.Sub(now) > maxEventSkew || now.Sub(ev.At) > maxEventSkew {
			ev.At = now
		}
		// Body id wins: that is the event's own identity. When the sender
		// omitted one, the POST-level key plus this line's index stands in,
		// so retrying the whole request does not double-count.
		if ev.ID == "" {
			keyed = replayKey != ""
			ev.ID = derivedEventID(keyPrefix, r.decoded+1)
		}
		if s.rec.RecordAgent(ev) {
			r.stored++
		}
		r.decoded++
		r.keyed = keyed
		if state != nil {
			state.accepted = r.decoded
			state.stored = r.stored
		}
	}
}

// decodeFailure classifies a failure that came out of the JSON decoder itself,
// which distinguishes a body too large, a broken read, and a malformed object.
func (r streamResult) decodeFailure(err error, keyed bool) streamResult {
	r.keyed = keyed
	if maxBytes, ok := errors.AsType[*http.MaxBytesError](err); ok {
		// A size failure is not a JSON failure; senders need the distinction to
		// know trimming (not re-encoding) is the fix.
		r.status = http.StatusRequestEntityTooLarge
		r.msg = fmt.Sprintf("event stream exceeds %d byte cap", maxBytes.Limit)
		return r
	}
	if _, ok := errors.AsType[*net.OpError](err); ok {
		r.status = http.StatusBadRequest
		r.msg = clientJSONError(err)
		r.extra = []any{"body_error", logcfg.RedactedField(err.Error(), 256)}
		return r
	}
	// Anything else that came out of the decoder is a read failure the
	// *net.OpError test does not name: a cancelled lifecycle, a connection
	// reset mid-body, a handler timeout. clientJSONError returns a string, so
	// without this the error value is gone by the time the audit line is
	// written, and the operator is left with "bad json: <text>" and no way to
	// tell a sender that vanished from a sender that sent the wrong bytes.
	//
	// A payload error the decoder classified itself is left alone: its
	// message already names the field or the offset a sender can act on, and
	// calling it a body read failure would point the operator at the network
	// when the sender's own object is what is wrong.
	if _, ok := errors.AsType[*json.SyntaxError](err); !ok {
		if _, ok := errors.AsType[*json.UnmarshalTypeError](err); !ok {
			r.status = http.StatusBadRequest
			r.msg = clientJSONError(err)
			r.extra = []any{"body_error", logcfg.RedactedField(err.Error(), 256)}
			return r
		}
	}
	r.status, r.msg = http.StatusBadRequest, clientJSONError(err)
	return r
}

// clientJSONError turns an encoding/json decode failure into a sender-facing
// reason: JSON field names, no Go type names.
func clientJSONError(err error) string {
	if _, ok := errors.AsType[*net.OpError](err); ok {
		return "request body read failed"
	}
	if ut, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
		if ut.Field != "" {
			return fmt.Sprintf("bad json: %s must be %s, not %s", ut.Field, wantJSONType(ut), ut.Value)
		}
		return fmt.Sprintf("bad json: expected a JSON object or NDJSON stream, got %s", ut.Value)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "bad json: truncated"
	}
	// A syntax failure in an NDJSON stream says nothing about where it
	// happened, so a sender with a 200-event body cannot find the line. The
	// decoder's offset is a body offset, which is what a sender can seek to.
	if se, ok := errors.AsType[*json.SyntaxError](err); ok {
		return fmt.Sprintf("bad json: %s at body offset %d", se.Error(), se.Offset)
	}
	return "bad json: " + err.Error()
}

func wantJSONType(ut *json.UnmarshalTypeError) string {
	if ut.Type != nil && ut.Type.String() == "string" {
		return "a string"
	}
	return "the documented type"
}
