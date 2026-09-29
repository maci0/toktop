package ingest

import (
	"fmt"
	"net/http"
	"strconv"
)

// handleHealth answers the liveness probe. Plain text like every other
// non-event answer here, and never audited: a probe runs continuously and
// would drown the event log.
//
// It reports degraded rather than ok while every decode slot is held. At that
// point the endpoint answers 503 to every POST, so a probe that keeps saying
// ok describes a service that accepts nothing.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
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
