// Package ingest runs a tiny localhost HTTP endpoint so agents, harnesses and
// scripts can push token-usage events that toktop renders live.
//
// One concern per file: server.go is the listener and the clock, endpoints.go
// the route table, middleware.go the chain every request passes, post.go the
// POST handler and the bounds on its body, stream.go the decode loop, and
// health.go the liveness probe.
package ingest

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/logcfg"
)

// Server accepts POST /v1/events (single object or newline-delimited stream)
// and GET or HEAD /healthz.
type Server struct {
	rec core.AgentRecorder
	// nowMu guards now, which every request goroutine reads through instant
	// and SetNow writes. Without it a SetNow after Serve started would race
	// the handler stamping the event in flight.
	nowMu sync.RWMutex
	now   func() time.Time // event stamps; defaults to time.Now. I/O deadlines stay wall-clock.
	srv   http.Server
	ln    net.Listener
	addr  string
	// hostGuard refuses a request whose Host names something other than the
	// loopback interface, and is nil when the listener was bound wider (an
	// explicit --ingest on a routable address), where the host is whatever
	// the operator's peers call it and no allowlist is right. Set in
	// newServer; nil on a Server built as a literal, which skips the check.
	hostGuard func(host string) bool
	log       *slog.Logger
	// Body bounds, fixed at construction. They were package vars so a test
	// could shrink them, which put mutable state on the request path: every
	// body read and every error string read them while a test wrote them, and
	// the OpenAPI spec test wrote one against a live server. Per-server fields
	// written before Serve leave the handler goroutines reading an immutable
	// value, and a test that wants different bounds builds a different server.
	// Absolute deadlines, so they stay on the wall clock and not on now.
	idleTimeout          time.Duration
	maxEventLifetime     time.Duration
	bodyIdleTimeout      time.Duration
	responseWriteTimeout time.Duration
}

// Body-bound defaults. Protocol limits rather than deployment tunables: a
// peer that stalls mid-body would otherwise pin an fd and a goroutine until
// the OS TCP timeout, and IdleTimeout does not apply mid-request. The absolute
// deadline caps total lifetime; each successful read extends the deadline up
// to that end, so slow-but-alive NDJSON streams keep working while silent ones
// are reaped. Tests shrink these per server, not per process.
const (
	defaultIdleTimeout   = 2 * time.Minute
	defaultMaxEventLife  = 10 * time.Minute
	defaultBodyIdle      = time.Minute
	defaultResponseWrite = 30 * time.Second
)

// idleTimeout reaps keep-alive connections that sit between requests. Without
// it a vanished peer holds an fd and a goroutine for the life of the dashboard.
// A peer that goes silent mid-body is a different bound: progressBody applies
// bodyIdleTimeout per read and maxEventLifetime to the stream as a whole. The
// endpoint is localhost-bound by default but can be exposed via --ingest.
const idleTimeout = 2 * time.Minute

// readHeaderTimeout bounds a request's header read. A peer that opens a
// connection and sends nothing would otherwise hold a goroutine until
// IdleTimeout, long after it stopped doing anything.
const readHeaderTimeout = 5 * time.Second

// maxHeaderBytes is the request-header budget, below net/http's 1 MiB
// default. An event POST carries a short header, so the extra room would
// only be there for a peer sending something this endpoint does not read.
const maxHeaderBytes = 16 << 10

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
	s.maxEventLifetime = defaultMaxEventLife
	s.bodyIdleTimeout = defaultBodyIdle
	s.responseWriteTimeout = defaultResponseWrite
	s.hostGuard = loopbackHostGuard(ln.Addr())
	s.srv = http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		// net/http interpolates conn.RemoteAddr into panic and handshake
		// lines. That is the same peer address logcfg.Remote redacts: personal
		// data when --ingest is bound off loopback.
		ErrorLog: slog.NewLogLogger(logcfg.RedactHandler{Handler: lg.Handler()}, slog.LevelError),
	}
	return s, nil
}

// SetNow overrides the clock used to stamp events that arrive without a
// timestamp and to clamp stamps that sit far from arrival. Request timeouts
// still use wall time. Safe to call while Serve is running: the write is taken
// under the same lock every handler reads it under. Demo mode passes the
// simulated clock so harness POSTs stay on the seeded timeline.
func (s *Server) SetNow(fn func() time.Time) {
	if fn == nil {
		fn = time.Now
	}
	s.nowMu.Lock()
	s.now = fn
	s.nowMu.Unlock()
}

func (s *Server) instant() time.Time {
	s.nowMu.RLock()
	fn := s.now
	s.nowMu.RUnlock()
	if fn != nil {
		return fn()
	}
	return time.Now()
}

// Addr returns the actual bound address (useful when starting on :0).
func (s *Server) Addr() string { return s.addr }

// Serve runs the ingest endpoint until Close. A Close from any goroutine is
// reported as a nil error, since that is how a run ends.
//
// Any other failure ends the endpoint for good, so it is audited here rather
// than left to the caller: an accept error that only reached an ad hoc
// stderr write lost the level floor and the addr redaction every other ingest
// line has, and under the alt screen a feed that stops accepting is a feed
// that silently stops.
func (s *Server) Serve() error {
	err := s.srv.Serve(s.ln)
	if err == http.ErrServerClosed {
		return nil
	}
	if err != nil && s.log != nil {
		s.log.Error("toktop: ingest stopped",
			"addr", s.addr,
			"error", logcfg.RedactedField(err.Error(), 256))
	}
	return err
}

// Close stops the endpoint and releases the listener. It is idempotent, and
// in-flight requests are dropped rather than drained: nothing posted to this
// endpoint outlives the process that receives it.
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
