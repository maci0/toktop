package ingest

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
)

const (
	eventsPath = "/v1/events"
	healthPath = "/healthz"
	// healthOK is the whole healthy answer, newline included: http.Error
	// appends one, the 202 ack writes one, and the site's own /health answers
	// the same. A probe that reads a whole line rather than trimming should
	// not have to special-case this one.
	healthOK = "ok\n"
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
// mistake a sender can act on rather than a dead end. Each is named with the
// methods it actually takes, the list the 405 on a known path builds its Allow
// header and its own body from: a sender correcting one typo from the methods
// the primary alone would not tell it not to repeat.
//
// Each entry is one path with its methods in parentheses, and the entries are
// joined by a semicolon. Reading the methods off the front of the entry (the
// "POST /v1/events" order) runs them into the next path across a comma, so
// "POST /v1/events, GET, HEAD /healthz" can be split two ways and names a
// third endpoint, "/events", that is not served. The parenthesised form is
// unambiguous: split on "; ", take the methods between "(" and ")", and the
// list a sender parses is the one the table holds.
func notFoundMessage() string {
	advertised := make([]string, 0, len(ingestEndpoints))
	for _, e := range ingestEndpoints {
		advertised = append(advertised, e.path+" ("+e.allow()+")")
	}
	return "not found; endpoints: " + strings.Join(advertised, "; ")
}

func methodNotAllowedMessage(e endpoint, method string) string {
	return fmt.Sprintf("method not allowed; %s accepts %s, not %s", e.path, e.allow(), method)
}
