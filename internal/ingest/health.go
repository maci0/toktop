package ingest

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/maci0/toktop/internal/logcfg"
)

// handleHealth answers the liveness probe. Plain text like every other
// non-event answer here, and a healthy probe is never audited: a probe runs
// continuously and would drown the event log.
//
// A degraded answer is logged, once per episode, by auditHealth. A run with no
// slots held and a run with every one held produce exactly one line each: the
// 503 is the only thing that says the endpoint is refusing every event, and on
// a box whose senders have all stopped posting, the refused POSTs that would
// otherwise carry that news never arrive.
//
// It reports degraded rather than ok while every decode slot is held. At that
// point the endpoint answers 503 to every POST, so a probe that keeps saying
// ok describes a service that accepts nothing.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	// The body and the status are settled before a byte is written, so the
	// length below is the length of the answer actually sent.
	body, status := healthOK, http.StatusOK
	slots := eventSlots
	if in := len(slots); in >= cap(slots) {
		// The same Retry-After the refused POSTs carry: a 503 that names no
		// delay leaves a client to invent one, and the slot frees as soon as a
		// stalled body gives up.
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
		status = http.StatusServiceUnavailable
		body = fmt.Sprintf("degraded: %d/%d event streams in flight; events are being refused\n", in, cap(slots))
	}
	s.auditHealth(r, status, len(slots), cap(slots), time.Since(start))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// Stated here rather than left to the runtime: a HEAD carries the GET's
	// headers and no body (RFC 9110), and net/http takes the length from the
	// body it withheld, so a HEAD answered without this states no length at
	// all where its GET states the size of the answer. The endpoint table
	// serves HEAD alongside GET, so a probe that asks one has to read the
	// same answer either way. The error bodies need no such line: http.Error
	// writes the body for a HEAD too and net/http counts those bytes, so
	// those answers state their length either way.
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	fmt.Fprint(w, body)
}

// auditHealth writes one line when the probe crosses into degraded and one
// when it crosses back, and nothing in between. The latch is what makes the
// line affordable: an uptime probe asks every few seconds, so logging the
// state rather than the crossing would bury the event log a line per check.
//
// The fields are the ones every other ingest line carries, so one filter on
// method, path or duration covers a probe with the rest, plus the two numbers
// the refused POSTs audit beside their reason: the endpoint reports 503 here
// because the decode slots are the cap, so a line that named the 503 without
// them would say the endpoint is refusing events and not how close it is.
func (s *Server) auditHealth(r *http.Request, status, inFlight, slots int, d time.Duration) {
	if s.log == nil {
		return
	}
	degraded := status == http.StatusServiceUnavailable
	s.healthMu.Lock()
	was := s.healthDegraded
	s.healthDegraded = degraded
	s.healthMu.Unlock()
	if degraded == was {
		return
	}
	level, msg := slog.LevelInfo, "toktop: ingest accepting events again"
	if degraded {
		level, msg = slog.LevelWarn, "toktop: ingest refusing events"
	}
	s.log.Log(r.Context(), level, msg,
		"req", s.requestID(r),
		"method", logcfg.Field(r.Method, 16),
		"path", logcfg.Field(r.URL.Path, 64),
		"remote", logcfg.Remote(r.RemoteAddr),
		"status", status,
		"in_flight", inFlight,
		"slot_cap", slots,
		"duration", d.Round(time.Microsecond),
	)
}
