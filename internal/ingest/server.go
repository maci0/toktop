// Package ingest runs a tiny localhost HTTP endpoint so agents, harnesses and
// scripts can push token-usage events that toktop renders live.
package ingest

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/maci0/toktop/internal/core"
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
	return newServer(addr, rec, newIngestLogger())
}

func newServer(addr string, rec core.AgentRecorder, lg *slog.Logger) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if lg == nil {
		lg = slog.New(slog.DiscardHandler)
	}
	s := &Server{rec: rec, now: time.Now, ln: ln, addr: ln.Addr().String(), log: lg}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/events", s.handlePost)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})
	s.srv = http.Server{
		Handler:           s.wrap(mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    16 << 10, // default 1 MiB; this endpoint has no large headers
		// net/http interpolates conn.RemoteAddr into panic and handshake
		// lines. That is the same peer address logPost redacts: personal
		// data when --ingest is bound off loopback.
		ErrorLog: slog.NewLogLogger(addrRedactHandler{lg.Handler()}, slog.LevelError),
	}
	return s, nil
}

type ctxReqID struct{}

func setSecurityHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
}

// wrap is the single ingest handler: security headers, request id, panic
// recover, 404 naming the two endpoints, and 404/405 audit lines. POST
// /v1/events keeps a bare ResponseWriter so handlePost can SetReadDeadline.
func (s *Server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w.Header())
		id := incomingRequestID(r)
		w.Header().Set("X-Request-Id", id)
		r = r.WithContext(context.WithValue(r.Context(), ctxReqID{}, id))

		start := time.Now()
		defer func() {
			recov := recover()
			if recov == nil {
				return
			}
			if recov == http.ErrAbortHandler {
				panic(recov)
			}
			s.logRequest(r, requestID(r), http.StatusInternalServerError, 0, time.Since(start),
				fmt.Sprintf("panic: %v", recov),
				"stack", logField(string(debug.Stack()), 2048))
			http.Error(w, "internal error", http.StatusInternalServerError)
		}()

		if r.URL == nil || !knownIngestPath(r.URL.Path) {
			http.Error(w, "not found; endpoints: POST /v1/events, GET /healthz", http.StatusNotFound)
			s.logRequest(r, id, http.StatusNotFound, 0, time.Since(start), "not found")
			return
		}
		if skipUnhandledLog(r) {
			next.ServeHTTP(w, r)
			return
		}
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		if status < 400 {
			return
		}
		msg := "method not allowed"
		if status != http.StatusMethodNotAllowed {
			msg = "not found"
			if status != http.StatusNotFound {
				msg = strings.ToLower(http.StatusText(status))
			}
		}
		s.logRequest(r, id, status, 0, time.Since(start), msg)
	})
}

func incomingRequestID(r *http.Request) string {
	if v := logField(r.Header.Get("X-Request-Id"), 64); v != "" {
		return v
	}
	return rand.Text()
}

// clientEventKey is the caller-supplied idempotency token for this POST, if
// any. Only Idempotency-Key counts: X-Request-Id is a correlation id, often
// reused across distinct sends or minted per attempt, so treating it as an
// event id would collapse unrelated turns or fail to collapse retries.
func clientEventKey(r *http.Request) string {
	return logField(r.Header.Get("Idempotency-Key"), 128)
}

// derivedEventID maps one line of a POST onto a stable event id so a replay
// of the same stream (lost 202, retry after a mid-stream 400) lands on the
// same keys the collector already ignores. seq is 1-based within the POST.
func derivedEventID(key string, seq int) string {
	if key == "" || seq < 1 {
		return ""
	}
	suffix := ":" + strconv.Itoa(seq)
	head := core.ClampField(key, 128-utf8.RuneCountInString(suffix))
	if head == "" {
		return ""
	}
	return head + suffix
}

func requestID(r *http.Request) string {
	if v, ok := r.Context().Value(ctxReqID{}).(string); ok && v != "" {
		return v
	}
	return incomingRequestID(r)
}

// LogLevelEnv is the process environment variable that sets the ingest
// audit-log floor. Empty means info. Main validates the value at startup.
const LogLevelEnv = "TOKTOP_LOG_LEVEL"

// ParseLogLevel maps a TOKTOP_LOG_LEVEL value onto a slog floor.
// Empty is info. Accepted names are debug, info, warn (or warning), and error.
func ParseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("$%s must be debug, info, warn, or error, got %q", LogLevelEnv, s)
	}
}

func newIngestLogger() *slog.Logger {
	lvl, err := ParseLogLevel(os.Getenv(LogLevelEnv))
	if err != nil {
		lvl = slog.LevelInfo // main already rejected this; stay quiet if constructed in tests
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:       lvl,
		ReplaceAttr: utcLogTime,
	}))
}

func utcLogTime(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
		return slog.String(slog.TimeKey, a.Value.Time().UTC().Format(time.RFC3339Nano))
	}
	return a
}

// logField prepares attacker-shaped text for a single-line log attribute:
// terminal escapes stripped, whitespace collapsed so a payload cannot split
// the line, then capped.
func logField(s string, n int) string {
	return core.ClampField(strings.Join(strings.Fields(core.SanitizeText(s)), " "), n)
}

// logRemote prepares a peer address for the ingest audit line. Loopback
// keeps the port so a local sender can be told apart; any other IP is
// dropped. The address is personal data when --ingest is bound off loopback.
func logRemote(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "unknown"
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return net.JoinHostPort("loopback", port)
	}
	return "remote"
}

// remoteAddrPat matches the host:port form net.Addr.String uses for TCP:
// dotted IPv4, or bracketed IPv6 (including zone ids).
var remoteAddrPat = regexp.MustCompile(`(?:\d{1,3}(?:\.\d{1,3}){3}|\[[0-9A-Fa-f:.%]+\]):\d{1,5}`)

// redactLogAddrs rewrites host:port appearances with logRemote so a line
// from net/http's ErrorLog cannot carry a peer IP.
func redactLogAddrs(s string) string {
	if !strings.ContainsAny(s, ".[") {
		return s
	}
	return remoteAddrPat.ReplaceAllStringFunc(s, logRemote)
}

// addrRedactHandler rewrites slog messages the way logRemote rewrites the
// audit line's remote attribute. http.Server.ErrorLog is a *log.Logger, so
// the peer address arrives as text in the message, not as a structured attr.
type addrRedactHandler struct {
	slog.Handler
}

func (h addrRedactHandler) Handle(ctx context.Context, r slog.Record) error {
	r.Message = redactLogAddrs(r.Message)
	return h.Handler.Handle(ctx, r)
}

func (h addrRedactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return addrRedactHandler{h.Handler.WithAttrs(attrs)}
}

func (h addrRedactHandler) WithGroup(name string) slog.Handler {
	return addrRedactHandler{h.Handler.WithGroup(name)}
}

func (s *Server) logRequest(r *http.Request, reqID string, status, accepted int, d time.Duration, errMsg string, extra ...any) {
	if s.log == nil {
		return
	}
	path := ""
	if r.URL != nil {
		path = r.URL.Path
	}
	attrs := []any{
		"req", reqID,
		"method", logField(r.Method, 16),
		"path", logField(path, 64),
		"remote", logRemote(r.RemoteAddr),
		"status", status,
		"accepted", accepted,
		"duration", d.Round(time.Microsecond),
	}
	if errMsg != "" {
		attrs = append(attrs, "error", logField(errMsg, 256))
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

func knownIngestPath(path string) bool {
	switch path {
	case "/v1/events", "/healthz":
		return true
	}
	return false
}

func skipUnhandledLog(r *http.Request) bool {
	switch r.URL.Path {
	case "/healthz":
		return r.Method == http.MethodGet || r.Method == http.MethodHead
	case "/v1/events":
		return r.Method == http.MethodPost
	}
	return false
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
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

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := requestID(r)
	if w.Header().Get("X-Request-Id") == "" {
		w.Header().Set("X-Request-Id", reqID)
	}
	done := func(status, accepted int, errMsg string) {
		s.logRequest(r, reqID, status, accepted, time.Since(start), errMsg)
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
		http.Error(w, msg, http.StatusForbidden)
		done(http.StatusForbidden, 0, msg)
		return
	}
	until := time.Now().Add(maxEventLifetime)
	_ = rc.SetReadDeadline(until) // covers reads before the first progress extension
	r.Body = http.MaxBytesReader(w, &progressBody{ReadCloser: r.Body, rc: rc, until: until}, maxEventBody)
	dec := json.NewDecoder(r.Body)
	defer r.Body.Close()
	n := 0
	replayKey := clientEventKey(r)
	// fail reports a stream-level error. Events decode-and-record one by one,
	// so everything before the failing line is already in the feed; saying so
	// lets senders resume after the failure instead of replaying the whole
	// stream and duplicating what was kept.
	fail := func(status int, msg string) {
		if n > 0 {
			if n == 1 {
				msg += "; 1 earlier event in this stream was recorded"
			} else {
				msg += fmt.Sprintf("; %d earlier events in this stream were recorded", n)
			}
		}
		armWrite()
		http.Error(w, msg, status)
		done(status, n, msg)
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
			ev.ID = derivedEventID(replayKey, n+1)
		}
		s.rec.RecordAgent(ev)
		n++
	}
	armWrite()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if _, err := fmt.Fprintf(w, `{"accepted":%d}`+"\n", n); err != nil {
		done(http.StatusAccepted, n, "response write failed")
		return
	}
	_ = rc.SetWriteDeadline(time.Time{}) // keep-alive must not inherit the write cap
	done(http.StatusAccepted, n, "")
}

// clientJSONError turns an encoding/json decode failure into a sender-facing
// reason: JSON field names, no Go type names.
func clientJSONError(err error) string {
	if ut, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
		if ut.Field != "" {
			return fmt.Sprintf("bad json: %s must be %s, not %s", ut.Field, wantJSONType(ut), ut.Value)
		}
		return fmt.Sprintf("bad json: expected a JSON object or NDJSON stream, got %s", ut.Value)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "bad json: truncated"
	}
	return "bad json: " + err.Error()
}

func wantJSONType(ut *json.UnmarshalTypeError) string {
	if ut.Type != nil && ut.Type.String() == "string" {
		return "a string"
	}
	return "the documented type"
}
