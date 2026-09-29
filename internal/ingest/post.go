package ingest

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/maci0/toktop/internal/logcfg"
)

// maxEventBody caps one POST (single object or NDJSON stream). Legit events
// are tiny; without a cap a client can stream unbounded bytes into the
// decode loop for as long as the connection stays up.
const maxEventBody = 1 << 20

// maxInFlightEvents bounds the POST bodies decoded at any one instant.
//
// net/http runs one goroutine per accepted connection and nothing caps how
// many it will accept, and the per-read deadline below is a whole minute, so
// a peer that opens connections and then stops sending holds a descriptor
// and a goroutine each for that minute. Past the cap a POST is refused
// instead of read: the handler answers and returns at once, so the pile-up
// costs the process nothing it cannot hand back, and a sender that retries
// (503, Retry-After) gets in as soon as a slot frees.
const maxInFlightEvents = 64

// eventSlots counts the POST bodies being decoded. It is process-wide: the
// endpoint is one listener bound to a fixed address, so the descriptors a
// pile-up would cost are the process's either way, and a cap shared by every
// server in it is the same bound. The channel is the var tests shrink.
var eventSlots = make(chan struct{}, maxInFlightEvents)

// maxEventSkew bounds how far a claimed event timestamp may sit from arrival
// before it is clamped to the arrival instant. The stamp is a sender's word:
// a wrong clock (or a forged event, since this endpoint authenticates
// nothing) can claim an instant hours away from this one. Ahead, that pins
// the agent view's "● live" marker (a negative idle duration reads as fresh)
// and renders a future wall-clock time in the feed until real time caught up.
// Behind, it is worse than a wrong clock on screen: the event sorts to the
// front of the retained feed, so once the feed is full every later event of
// that sender is refused as outside the window, the 202 reports stored below
// accepted, and the sender is told to resend a stream that will be refused
// again on every attempt. A sender whose clock lags by hours (a host that was
// never on the network since boot, a restored VM snapshot) contributes
// nothing at all that way. Modest skew between machines stays honored.
const maxEventSkew = 2 * time.Minute

// retryAfterSeconds is the delay every 503 from this endpoint advertises, in
// whole seconds (RFC 9110 Retry-After). One constant because the refused POST
// and the health probe must name the same wait: a client that waits what one
// says and ignores the other retries faster than the slots free.
const retryAfterSeconds = 1

// progressBody arms the read deadline before every read: no progress within
// idle, or past the absolute end, surfaces as an i/o timeout from Decode.
// The bounds travel with the body, read from the server that is handling the
// request rather than from package state every request shares, so a 408 names
// the ones that applied here. Deadline setting is best effort; on
// ResponseWriters without support the body degrades to volume-only capping.
type progressBody struct {
	io.ReadCloser
	rc    *http.ResponseController
	idle  time.Duration // no progress for this long reaps the body
	life  time.Duration // the whole POST's lifetime, named by the 408
	until time.Time     // the absolute end, until the lifetime
	last  time.Time     // the deadline the read that failed was armed with
	// armed records whether a deadline of ours was ever accepted. It is false
	// on a ResponseWriter that does not support them, and then the i/o
	// timeout that reaches the decoder is the server's own, not one of the
	// two below, so naming either of them would be a guess.
	armed bool
}

// arm sets the read deadline, reporting whether the writer took it. Callers
// that must not name a deadline they never got use the result.
func (b *progressBody) arm(deadline time.Time) error {
	err := b.rc.SetReadDeadline(deadline)
	if err == nil {
		b.armed = true
	}
	return err
}

func (b *progressBody) Read(p []byte) (int, error) {
	next := time.Now().Add(b.idle)
	if next.After(b.until) {
		next = b.until
	}
	b.last = next
	_ = b.arm(next)
	return b.ReadCloser.Read(p)
}

// stallReason names the bound a 408 broke on. The idle window and the
// absolute lifetime are separate conditions with opposite fixes (send
// sooner, send less), and they both surface as the same i/o timeout, so a
// bare "request stalled" leaves the sender to guess which one it hit. A
// decode error always follows a read, and every read arms a deadline, so
// one of the two always fired.
func (b *progressBody) stallReason() string {
	if !b.armed {
		return "request stalled: the server's read timeout fired, not this endpoint's body limits"
	}
	if b.last.Before(b.until) {
		return fmt.Sprintf("request stalled: no body bytes for %s", b.idle)
	}
	return fmt.Sprintf("request stalled: stream exceeded the %s lifetime", b.life)
}

// clientEventKey is the caller-supplied idempotency token for this POST, if
// any. Only Idempotency-Key counts: X-Request-Id is a correlation id, often
// reused across distinct sends or minted per attempt, so treating it as an
// event id would collapse unrelated turns or fail to collapse retries.
func clientEventKey(r *http.Request) string {
	return r.Header.Get("Idempotency-Key")
}

// derivedKeyPrefix normalizes a replay key and hashes it into the 16 hex
// chars every derived id starts with. The key is NFC-normalized and hashed
// rather than truncated, so a collision needs only a match on the 8-byte
// sha256 prefix however long the keys are.
//
// The prefix is a property of the POST, not of the line, so a caller
// decoding a whole body computes it once and hands it to derivedEventID per
// line rather than re-normalizing and re-hashing the same key per line.
func derivedKeyPrefix(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(norm.NFC.String(key)))
	return hex.EncodeToString(sum[:8])
}

// derivedEventID maps one line of a POST onto a stable event id so a replay
// of the same stream (lost 202, retry after a mid-stream 400) lands on the
// same keys the collector already ignores. prefix comes from derivedKeyPrefix
// for this POST's replay key; seq is 1-based within the POST. A derived id is
// 16 hex chars plus the ":seq" suffix, well inside the core.AgentIDMax cap
// wireEventID applies to a caller-supplied id, so it never needs clamping of
// its own and the collector stores it verbatim.
func derivedEventID(prefix string, seq int) string {
	if prefix == "" || seq < 1 {
		return ""
	}
	return prefix + ":" + strconv.Itoa(seq)
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	// Bound once. The acquire below and the deferred release must name the same
	// channel: re-reading the var at release time would hand the permit back to
	// whatever the var holds by then, and a receive on a channel this request
	// never sent to blocks forever, so a handler would never return and a slot
	// would never come back. Same reason handleHealth binds it, to read len and
	// cap of one channel.
	slots := eventSlots
	reqID := s.requestID(r)
	state, _ := r.Context().Value(ctxRequest{}).(*requestState)
	// keyAttrs is the request's replay key on the audit line, hashed into the
	// same prefix the derived event ids carry (derivedKeyPrefix). It is filled
	// in once the header has been read, so the rejections answered before that
	// carry nothing: they never decoded a line, so they named no event ids.
	// With it, a POST that stored fewer events than it sent can be tied to the
	// ids it minted, and a sender comparing its own retries with the feed
	// matches the two records on one field.
	var keyAttrs []any
	done := func(status, accepted, stored int, errMsg string, extra ...any) {
		s.logRequest(r, reqID, status, accepted, stored, time.Since(start), errMsg, append(extra, keyAttrs...)...)
	}
	// n counts events decoded off the wire, stored how many the feed took.
	// A replay of an already-retained id decodes fine and stores nothing.
	n, stored := 0, 0
	reject := func(status int, msg string, extra ...any) {
		sw := &statusWriter{ResponseWriter: w}
		http.Error(sw, msg, status)
		if sw.err != nil {
			extra = append(extra, "response_error", logcfg.RedactedField(sw.err.Error(), logcfg.FieldCap))
		}
		done(status, n, stored, msg, extra...)
	}

	// A POST carrying an Origin header is browser-driven: every browser
	// attaches Origin to a cross-site write, while curl, scripts and agent
	// harnesses never send one. Without this check any web page the operator
	// visits could fire a no-preflight POST here (the endpoint does not read
	// Content-Type, so text/plain sails past CORS preflight) and forge rows
	// into the live feed.
	rc := http.NewResponseController(w)
	// A writer that refuses the deadline leaves every response below with no
	// bound at all: a peer that stops reading pins this goroutine to the OS
	// TCP timeout, and the audit line would still read a clean 202. The
	// refusal rides the request's own audit line rather than being dropped,
	// the way progressBody.armed records whether a read deadline was accepted.
	life, idle, write := s.bodyBounds()
	var writeArm []any
	armWrite := func() {
		if err := rc.SetWriteDeadline(time.Now().Add(write)); err != nil {
			writeArm = []any{"write_deadline_unarmed", logcfg.RedactedField(err.Error(), logcfg.FieldCap)}
		} else {
			writeArm = nil
		}
	}
	if r.Header.Get("Origin") != "" {
		msg := "browser-originated requests are not accepted; post from a script or agent without an Origin header"
		armWrite()
		reject(http.StatusForbidden, msg, writeArm...)
		return
	}
	// The slot covers the decode, the one part of the request that can hold a
	// goroutine and a descriptor for a body nobody finishes sending. Acquired
	// after the Origin guard, which answers without reading anything.
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
		armWrite()
		// in_flight beside the cap, so a run of these says how close the
		// endpoint is to refusing everything rather than only that it did:
		// the same number /healthz reports, on the same refusals.
		reject(http.StatusServiceUnavailable,
			fmt.Sprintf("at most %d event streams are decoded at once; retry", cap(slots)),
			append([]any{"in_flight", len(slots), "slot_cap", cap(slots)}, writeArm...)...)
		return
	}
	until := time.Now().Add(life)
	progress := &progressBody{ReadCloser: r.Body, rc: rc, idle: idle, life: life, until: until}
	_ = progress.arm(until) // covers reads before the first progress extension
	r.Body = http.MaxBytesReader(w, progress, maxEventBody)
	br := bufio.NewReader(r.Body)
	// A Peek error is not acted on here: a body that cannot be read fails the
	// first Decode with the same cause, and the decode path already reports
	// it. Logging it twice would bury the line that names the request.
	if lead, _ := br.Peek(3); len(lead) == 3 && lead[0] == 0xef && lead[1] == 0xbb && lead[2] == 0xbf {
		_, _ = br.Discard(3)
	}
	dec := json.NewDecoder(br)
	defer r.Body.Close()
	replayKey := clientEventKey(r)
	// The prefix is a property of the POST, not of a line, so it is hashed
	// once here and handed to decodeStream per line rather than re-normalizing
	// and re-hashing the same key for every event.
	keyPrefix := derivedKeyPrefix(replayKey)
	if keyPrefix != "" {
		keyAttrs = []any{"event_key", keyPrefix}
	}
	// fail reports a stream-level error. Events decode-and-record one by one,
	// so everything before the failing line is already in the feed; saying so
	// lets a sender recover without duplicating what was kept.
	fail := func(res streamResult) {
		msg := res.msg
		if res.decoded > 0 {
			// accepted counts what the wire carried, stored what the feed took.
			// A replayed stream decodes every line and records none, so naming
			// the decoded count "recorded" would send a sender looking for
			// events the feed never had.
			// One event reads in the singular, and the two branches differ only
			// in what the feed took: a replayed id decodes but is not recorded,
			// so that count is reported as received, not recorded.
			noun, verb := "events", "were"
			if res.decoded == 1 {
				noun, verb = "event", "was"
			}
			if res.stored == res.decoded {
				msg += fmt.Sprintf("; %d earlier %s in this stream %s recorded", res.decoded, noun, verb)
			} else {
				msg += fmt.Sprintf("; %d earlier %s in this stream %s received, %d recorded",
					res.decoded, noun, verb, res.stored)
			}
			// Resuming with the remaining lines is right only when the kept
			// events carry no derived id. A derived id is the POST key plus
			// the line's 1-based index, so a resumed POST numbers its first
			// line 1 again: its events land on ids the feed already holds and
			// are dropped as duplicates, losing the rest of the stream
			// silently. The key makes a full replay safe instead.
			if res.keyed {
				msg += "; resend the whole request with the same Idempotency-Key"
			} else {
				msg += "; resend the remaining events to continue"
			}
		}
		armWrite()
		reject(res.status, msg, append(append([]any{}, res.extra...), writeArm...)...)
	}
	res := s.decodeStream(dec, progress, replayKey, keyPrefix, state)
	n, stored = res.decoded, res.stored
	if res.status != 0 {
		fail(res)
		return
	}
	armWrite()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	// stored is what the feed took, accepted what the wire carried: a sender
	// retrying after a lost 202 reads the gap here and on its own audit line.
	if _, err := fmt.Fprintf(w, `{"accepted":%d,"stored":%d}`+"\n", n, stored); err != nil {
		// The events are already recorded, so the status stands. The reason
		// still belongs in the audit line: without it a vanished sender and a
		// timeout mid-body are indistinguishable from success.
		done(http.StatusAccepted, n, stored, "response write failed: "+logcfg.RedactAddrs(err.Error()), writeArm...)
		return
	}
	_ = rc.SetWriteDeadline(time.Time{}) // keep-alive must not inherit the write cap
	done(http.StatusAccepted, n, stored, "", writeArm...)
}
