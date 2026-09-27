// Package ingest runs a tiny localhost HTTP endpoint so agents, harnesses and
// scripts can push token-usage events that toktop renders live.
package ingest

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/logcfg"
)

// Server accepts POST /v1/events (single object or newline-delimited stream)
// and GET /healthz.
type Server struct {
	rec  core.AgentRecorder
	now  func() time.Time // event stamps; defaults to time.Now. I/O deadlines stay wall-clock.
	srv  http.Server
	ln   net.Listener
	addr string
	log  *slog.Logger
}

// idleTimeout reaps keep-alive connections that sit between requests. Both
// timeouts zero would let vanished peers hold an fd and a goroutine apiece
// for the life of the dashboard; the endpoint is localhost-bound by default
// but can be exposed via --ingest.
var idleTimeout = 2 * time.Minute

// New binds addr and returns a server that Serve will accept on. The listen
// happens here so Addr reports the actual bound port (including :0) before
// Serve runs.
func New(addr string, rec core.AgentRecorder) (*Server, error) {
	return newServer(addr, rec, logcfg.Logger())
}

func newServer(addr string, rec core.AgentRecorder, lg *slog.Logger) (*Server, error) {
	// A server with no recorder answers 202 for every event and shows none of
	// them, and /healthz still says ok. Refuse the construction instead, so
	// the caller reports it at startup rather than the endpoint looking fine
	// while every POST is discarded.
	if rec == nil {
		return nil, errors.New("ingest needs a recorder")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if lg == nil {
		lg = slog.New(slog.DiscardHandler)
	}
	s := &Server{rec: rec, now: time.Now, ln: ln, addr: ln.Addr().String(), log: lg}
	mux := http.NewServeMux()
	for _, e := range ingestEndpoints {
		mux.HandleFunc(e.primary()+" "+e.path, func(w http.ResponseWriter, r *http.Request) {
			e.handle(s, w, r)
		})
	}
	s.srv = http.Server{
		Handler:           s.wrap(mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    16 << 10, // default 1 MiB; this endpoint has no large headers
		// net/http interpolates conn.RemoteAddr into panic and handshake
		// lines. That is the same peer address logcfg.Remote redacts: personal
		// data when --ingest is bound off loopback.
		ErrorLog: slog.NewLogLogger(logcfg.RedactHandler{Handler: lg.Handler()}, slog.LevelError),
	}
	return s, nil
}

type ctxRequest struct{}

// requestState carries the counts a request accumulated, so a panic after
// some events were recorded still reports how many the feed actually took.
type requestState struct {
	id       string
	accepted int
	stored   int
}

func setSecurityHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
}

// wrap is the single ingest handler: security headers, request id, panic
// recover, and the 404/405 rejections with their audit lines. Both come
// from the endpoint table, so the 404 lists exactly what is served and a 405
// names the methods the path takes. A request that passes both guards is
// logged by its own handler, which knows the counts.
func (s *Server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w.Header())
		id := incomingRequestID(r)
		w.Header().Set("X-Request-Id", id)
		state := &requestState{id: id}
		r = r.WithContext(context.WithValue(r.Context(), ctxRequest{}, state))

		start := time.Now()
		defer func() {
			recov := recover()
			if recov == nil {
				return
			}
			if recov == http.ErrAbortHandler {
				panic(recov)
			}
			s.logRequest(r, state.id, http.StatusInternalServerError, state.accepted, state.stored, time.Since(start),
				fmt.Sprintf("panic: %v", recov),
				"stack", logcfg.Field(string(debug.Stack()), 2048))
			http.Error(w, "internal error", http.StatusInternalServerError)
		}()

		e, known := endpoint{}, false
		if r.URL != nil {
			e, known = lookupEndpoint(r.URL.Path)
		}
		if !known {
			http.Error(w, notFoundMessage(), http.StatusNotFound)
			s.logRequest(r, id, http.StatusNotFound, 0, 0, time.Since(start), "not found")
			return
		}
		// The mux would answer a wrong method with the same status and Allow
		// header, but a body that names no method. Answering here keeps both
		// rejections in one error envelope: a plain-text line the caller can
		// act on, the way the 404 names the endpoints.
		if !e.serves(r.Method) {
			w.Header().Set("Allow", e.allow())
			msg := methodNotAllowedMessage(e, r.Method)
			http.Error(w, msg, http.StatusMethodNotAllowed)
			s.logRequest(r, id, http.StatusMethodNotAllowed, 0, 0, time.Since(start), "method not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func incomingRequestID(r *http.Request) string {
	if v := logcfg.Field(r.Header.Get("X-Request-Id"), 64); v != "" {
		return v
	}
	return rand.Text()
}

// clientEventKey is the caller-supplied idempotency token for this POST, if
// any. Only Idempotency-Key counts: X-Request-Id is a correlation id, often
// reused across distinct sends or minted per attempt, so treating it as an
// event id would collapse unrelated turns or fail to collapse retries.
func clientEventKey(r *http.Request) string {
	return r.Header.Get("Idempotency-Key")
}

// derivedEventID maps one line of a POST onto a stable event id so a replay
// of the same stream (lost 202, retry after a mid-stream 400) lands on the
// same keys the collector already ignores. seq is 1-based within the POST.
// The key is NFC-normalized and hashed rather than truncated, so a collision
// needs only a match on the 8-byte sha256 prefix however long the keys are.
// A derived id is 16 hex chars plus the ":seq" suffix, well inside the
// 128-character id cap applied when the event is stored.
func derivedEventID(key string, seq int) string {
	if key == "" || seq < 1 {
		return ""
	}
	suffix := ":" + strconv.Itoa(seq)
	sum := sha256.Sum256([]byte(norm.NFC.String(key)))
	return hex.EncodeToString(sum[:8]) + suffix
}

func requestID(r *http.Request) string {
	if state, ok := r.Context().Value(ctxRequest{}).(*requestState); ok && state.id != "" {
		return state.id
	}
	return incomingRequestID(r)
}

// logRequest writes the one audit line a finished request produces, success
// and rejection alike. accepted counts events decoded off the wire, stored how
// many the feed took: a replayed POST after a lost 202 differs from a first
// send only in stored.
func (s *Server) logRequest(r *http.Request, reqID string, status, accepted, stored int, d time.Duration, errMsg string, extra ...any) {
	if s.log == nil {
		return
	}
	path := ""
	if r.URL != nil {
		path = r.URL.Path
	}
	attrs := []any{
		"req", reqID,
		"method", logcfg.Field(r.Method, 16),
		"path", logcfg.Field(path, 64),
		"remote", logcfg.Remote(r.RemoteAddr),
		"status", status,
		"accepted", accepted,
		"stored", stored,
		"duration", d.Round(time.Microsecond),
	}
	if errMsg != "" {
		attrs = append(attrs, "error", logcfg.Field(errMsg, 256))
	}
	attrs = append(attrs, extra...)
	level := slog.LevelInfo
	switch {
	case status >= 500:
		level = slog.LevelError
	case status >= 400 || errMsg != "":
		level = slog.LevelWarn
	}
	s.log.Log(r.Context(), level, "toktop: ingest", attrs...)
}

const (
	eventsPath = "/v1/events"
	healthPath = "/healthz"
)

// endpoint is one served path, the methods it answers, and the handler that
// serves them. This table is the single source for routing, the 404 endpoint
// list and the 405 Allow header, so an endpoint registered in one place
// cannot be missing from the other two.
type endpoint struct {
	path    string
	methods []string // methods answered exactly; list HEAD explicitly to take it
	handle  func(*Server, http.ResponseWriter, *http.Request)
}

// primary is the method the path is registered and advertised under.
func (e endpoint) primary() string { return e.methods[0] }

func (e endpoint) allow() string { return strings.Join(e.methods, ", ") }

func (e endpoint) serves(method string) bool { return slices.Contains(e.methods, method) }

var ingestEndpoints = []endpoint{
	{path: eventsPath, methods: []string{http.MethodPost}, handle: (*Server).handlePost},
	{path: healthPath, methods: []string{http.MethodGet, http.MethodHead}, handle: (*Server).handleHealth},
}

func lookupEndpoint(path string) (endpoint, bool) {
	for _, e := range ingestEndpoints {
		if e.path == path {
			return e, true
		}
	}
	return endpoint{}, false
}

// notFoundMessage names every served endpoint, so an unknown path is a route
// mistake a sender can act on rather than a dead end.
func notFoundMessage() string {
	advertised := make([]string, 0, len(ingestEndpoints))
	for _, e := range ingestEndpoints {
		advertised = append(advertised, e.primary()+" "+e.path)
	}
	return "not found; endpoints: " + strings.Join(advertised, ", ")
}

func methodNotAllowedMessage(e endpoint, method string) string {
	return fmt.Sprintf("method not allowed; %s accepts %s, not %s", e.path, e.allow(), method)
}

type statusWriter struct {
	http.ResponseWriter
	status int
	err    error
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	if err != nil {
		w.err = err
	}
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// SetNow overrides the clock used to stamp events that arrive without a
// timestamp and to clamp far-future stamps. Request timeouts still use
// wall time. Call before Serve. Demo mode passes the simulated clock so
// harness POSTs stay on the seeded timeline.
func (s *Server) SetNow(fn func() time.Time) {
	if fn == nil {
		fn = time.Now
	}
	s.now = fn
}

func (s *Server) instant() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Addr returns the actual bound address (useful when starting on :0).
func (s *Server) Addr() string { return s.addr }

func (s *Server) Serve() error {
	err := s.srv.Serve(s.ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) Close() error {
	err := s.srv.Close()
	// Serve is the only path that tracks ln on the http.Server. Close
	// before Serve (or racing it) would otherwise leave the listen fd
	// bound until process exit.
	if s.ln != nil {
		if cerr := s.ln.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) && err == nil {
			err = cerr
		}
	}
	return err
}

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
// server in it is the same bound. A var so tests can shrink it.
var eventSlots = make(chan struct{}, maxInFlightEvents)

// maxEventSkew bounds how far ahead of arrival a claimed event timestamp may
// sit before it is clamped to the arrival instant. The stamp is a sender's
// word: a wrong clock (or a forged event, since this endpoint authenticates
// nothing) can claim an instant hours ahead, which would pin the agent view's
// "● live" marker (a negative idle duration reads as fresh) and render a
// future wall-clock time in the feed until real time caught up. Modest skew
// between machines stays honored.
const maxEventSkew = 2 * time.Minute

// maxEventLifetime and bodyIdleTimeout bound how long one POST may hold the
// connection. The byte cap above limits volume, not time: a peer that sends
// headers and then drips bytes (or goes silent mid-body) would otherwise pin
// an fd and a goroutine apiece until it finishes, and IdleTimeout does not
// apply mid-request. The absolute deadline caps total lifetime; each
// successful read extends the deadline up to that end, so slow-but-alive
// NDJSON streams keep working while silent ones are reaped. Both are vars so
// tests can shrink them.
var (
	maxEventLifetime = 10 * time.Minute
	bodyIdleTimeout  = time.Minute
	// responseWriteTimeout bounds writing the status line and tiny JSON
	// body after the request has been read. Without it a peer that stops
	// reading pins the handler goroutine until the OS TCP timeout.
	responseWriteTimeout = 30 * time.Second
)

// progressBody arms the read deadline before every read: no progress within
// bodyIdleTimeout, or past the absolute end, surfaces as an i/o timeout from
// Decode. Deadline setting is best effort; on ResponseWriters without
// support the body degrades to volume-only capping.
type progressBody struct {
	io.ReadCloser
	rc    *http.ResponseController
	until time.Time
}

func (b *progressBody) Read(p []byte) (int, error) {
	next := time.Now().Add(bodyIdleTimeout)
	if next.After(b.until) {
		next = b.until
	}
	_ = b.rc.SetReadDeadline(next)
	return b.ReadCloser.Read(p)
}

// handleHealth answers the liveness probe. Plain text like every other
// non-event answer here, and never audited: a probe runs continuously and
// would drown the event log.
//
// It reports degraded rather than ok while every decode slot is held. At that
// point the endpoint answers 503 to every POST, so a probe that keeps saying
// ok describes a service that accepts nothing.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if in := len(eventSlots); in >= cap(eventSlots) {
		// The same Retry-After the refused POSTs carry: a 503 that names no
		// delay leaves a client to invent one, and the slot frees as soon as a
		// stalled body gives up.
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "degraded: %d/%d event streams in flight; events are being refused\n", in, cap(eventSlots))
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := requestID(r)
	state, _ := r.Context().Value(ctxRequest{}).(*requestState)
	done := func(status, accepted, stored int, errMsg string, extra ...any) {
		s.logRequest(r, reqID, status, accepted, stored, time.Since(start), errMsg, extra...)
	}
	// n counts events decoded off the wire, stored how many the feed took.
	// A replay of an already-retained id decodes fine and stores nothing.
	n, stored := 0, 0
	reject := func(status int, msg string, extra ...any) {
		sw := &statusWriter{ResponseWriter: w}
		http.Error(sw, msg, status)
		if sw.err != nil {
			extra = append(extra, "response_error", logcfg.Field(logcfg.RedactAddrs(sw.err.Error()), 256))
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
	armWrite := func() {
		_ = rc.SetWriteDeadline(time.Now().Add(responseWriteTimeout))
	}
	if r.Header.Get("Origin") != "" {
		msg := "browser-originated requests are not accepted; post from a script or agent without an Origin header"
		armWrite()
		reject(http.StatusForbidden, msg)
		return
	}
	// The slot covers the decode, the one part of the request that can hold a
	// goroutine and a descriptor for a body nobody finishes sending. Acquired
	// after the Origin guard, which answers without reading anything.
	select {
	case eventSlots <- struct{}{}:
		defer func() { <-eventSlots }()
	default:
		w.Header().Set("Retry-After", "1")
		armWrite()
		reject(http.StatusServiceUnavailable,
			fmt.Sprintf("at most %d event streams are decoded at once; retry", cap(eventSlots)))
		return
	}
	until := time.Now().Add(maxEventLifetime)
	_ = rc.SetReadDeadline(until) // covers reads before the first progress extension
	r.Body = http.MaxBytesReader(w, &progressBody{ReadCloser: r.Body, rc: rc, until: until}, maxEventBody)
	br := bufio.NewReader(r.Body)
	if lead, _ := br.Peek(3); len(lead) == 3 && lead[0] == 0xef && lead[1] == 0xbb && lead[2] == 0xbf {
		_, _ = br.Discard(3)
	}
	dec := json.NewDecoder(br)
	defer r.Body.Close()
	replayKey := clientEventKey(r)
	// keyedStream is set once a line arrives without its own id, so its
	// identity was derived from the POST key and its position in the body.
	// Such a line can only be recovered by replaying the whole request.
	keyedStream := false
	// fail reports a stream-level error. Events decode-and-record one by one,
	// so everything before the failing line is already in the feed; saying so
	// lets a sender recover without duplicating what was kept.
	fail := func(status int, msg string, extra ...any) {
		if n > 0 {
			// accepted counts what the wire carried, stored what the feed took.
			// A replayed stream decodes every line and records none, so naming
			// the decoded count "recorded" would send a sender looking for
			// events the feed never had.
			if stored == n {
				if n == 1 {
					msg += "; 1 earlier event in this stream was recorded"
				} else {
					msg += fmt.Sprintf("; %d earlier events in this stream were recorded", n)
				}
			} else {
				msg += fmt.Sprintf("; %d earlier events in this stream were received, %d recorded",
					n, stored)
			}
			// Resuming with the remaining lines is right only when the kept
			// events carry no derived id. A derived id is the POST key plus
			// the line's 1-based index, so a resumed POST numbers its first
			// line 1 again: its events land on ids the feed already holds and
			// are dropped as duplicates, losing the rest of the stream
			// silently. The key makes a full replay safe instead.
			if keyedStream {
				msg += "; resend the whole request with the same Idempotency-Key"
			} else {
				msg += "; resend the remaining events to continue"
			}
		}
		armWrite()
		reject(status, msg, extra...)
	}
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if n > 0 && errors.Is(err, io.EOF) {
				break // clean end of stream after at least one event
			}
			if errors.Is(err, io.EOF) {
				fail(http.StatusBadRequest, "empty body: expected one JSON object or an NDJSON stream")
				return
			}
			if errors.Is(err, os.ErrDeadlineExceeded) {
				fail(http.StatusRequestTimeout, "request stalled")
				return
			}
			if maxBytes, ok := errors.AsType[*http.MaxBytesError](err); ok {
				// A size failure is not a JSON failure; senders need the
				// distinction to know trimming (not re-encoding) is the fix.
				fail(http.StatusRequestEntityTooLarge,
					fmt.Sprintf("event stream exceeds %d byte cap", maxBytes.Limit))
				return
			}
			if _, ok := errors.AsType[*net.OpError](err); ok {
				fail(http.StatusBadRequest, clientJSONError(err),
					"body_error", logcfg.Field(logcfg.RedactAddrs(err.Error()), 256))
				return
			}
			fail(http.StatusBadRequest, clientJSONError(err))
			return
		}
		// Null unmarshals into a struct as zeros; the body must be an object.
		if kind := jsonRootKind(raw); kind != "object" {
			fail(http.StatusBadRequest, "bad json: expected a JSON object or NDJSON stream, got "+kind)
			return
		}
		var wire agentEventWire
		if err := json.Unmarshal(raw, &wire); err != nil {
			fail(http.StatusBadRequest, clientJSONError(err))
			return
		}
		ev, err := eventFromWire(wire)
		if err != nil {
			msg := err.Error()
			if errors.Is(err, errBadTS) {
				msg = "bad ts: " + msg
			}
			fail(http.StatusBadRequest, msg)
			return
		}
		now := s.instant()
		if ev.At.IsZero() {
			ev.At = now
		} else if ev.At.Sub(now) > maxEventSkew {
			ev.At = now
		}
		// Body id wins: that is the event's own identity. When the sender
		// omitted one, the POST-level key plus this line's index stands in,
		// so retrying the whole request does not double-count.
		if ev.ID == "" {
			keyedStream = replayKey != ""
			ev.ID = derivedEventID(replayKey, n+1)
		}
		if s.rec.RecordAgent(ev) {
			stored++
		}
		n++
		if state != nil {
			state.accepted = n
			state.stored = stored
		}
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
		done(http.StatusAccepted, n, stored, "response write failed: "+logcfg.RedactAddrs(err.Error()))
		return
	}
	_ = rc.SetWriteDeadline(time.Time{}) // keep-alive must not inherit the write cap
	done(http.StatusAccepted, n, stored, "")
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
