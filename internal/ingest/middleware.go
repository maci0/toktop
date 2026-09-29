package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/logcfg"
)

// routes builds the served handler: the endpoint table behind the security
// headers, the request id and the 404/405 guards. Kept apart from newServer
// so a test drives the same chain the listener runs, not a hand-built
// approximation of it.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	for _, e := range ingestEndpoints {
		mux.HandleFunc(e.primary()+" "+e.path, func(w http.ResponseWriter, r *http.Request) {
			e.handle(s, w, r)
		})
	}
	return s.wrap(mux)
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

// loopbackHostGuard returns the Host check for a listener bound to ln, or nil
// when the listener is not loopback-only.
//
// The Origin check in handlePost stops a page on another site from forging
// rows into the feed, and DNS rebinding defeats it: a page that resolves its
// own name to 127.0.0.1 for one fetch is same-origin with this endpoint, so
// the browser sends no Origin at all and the request sails past the guard. The
// Host header is what a rebound request still carries, and it carries the
// attacker's name, so this is the check that closes it.
//
// Only a loopback listener gets one. A --ingest bound to a routable address is
// reached under whatever name the operator's peers use for the box, and an
// allowlist there would refuse the very traffic the operator asked for; that
// bind is already the state main warns about, and the rebinding page reaches
// it only through the loopback interface anyway.
func loopbackHostGuard(addr net.Addr) func(string) bool {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || !tcp.IP.IsLoopback() {
		return nil
	}
	// A listener bound to the wildcard reports a nil IP, and IsLoopback is
	// false for it, so this covers only an explicit loopback bind.
	return func(host string) bool {
		name := host
		if h, _, err := net.SplitHostPort(host); err == nil {
			name = h
		}
		name = strings.TrimSuffix(strings.TrimSuffix(name, "]"), "[")
		if name == "" {
			return false
		}
		if ip := net.ParseIP(name); ip != nil {
			return ip.IsLoopback()
		}
		// The name form a browser or a curl on the same box uses for this
		// endpoint. Folded ASCII over NFC, the way every other host
		// comparison in the tree folds one: localhost is an ASCII literal and
		// strings.ToLower would also fold runes whose lowercase form is
		// ASCII, so a U+212A in the name would satisfy a compare the operator
		// never wrote.
		folded := core.FoldASCII(norm.NFC.String(name))
		return folded == "localhost" || strings.HasSuffix(folded, ".localhost")
	}
}

// wrap is the single ingest handler: security headers, request id, panic
// recover, and the 404/405 rejections with their audit lines. Both come
// from the endpoint table, so the 404 lists exactly what is served and a 405
// names the methods the path takes. A request that passes both guards is
// logged by its own handler, which knows the counts.
func (s *Server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w.Header())
		id := s.incomingRequestID(r)
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
			// The recovered value and the stack are the only two audit payloads
			// in this package that reach the line untrimmed. Every other sink
			// here folds a home into "~" and clips to a snippet, because the
			// audit stream outlives the run and gets pasted into issues. A
			// panic is the one place this process quotes text it did not
			// author: the handlers below decode event fields off the wire, so
			// a panic raised while holding one carries whatever the sender
			// wrote, and a stack frame names the file and line it unwound
			// through. Folding both is what keeps an account name out of a log
			// line that a bug report would carry with it.
			s.logRequest(r, state.id, http.StatusInternalServerError, state.accepted, state.stored, time.Since(start),
				core.RedactHome(core.Snippet([]byte(fmt.Sprintf("panic: %v", recov)))),
				"stack", logcfg.Field(core.RedactHome(string(debug.Stack())), 2048))
			http.Error(w, "internal error", http.StatusInternalServerError)
		}()

		// Ahead of the routing table, not after it: a rebound request must
		// learn nothing about what this endpoint serves, so the 404's
		// endpoint list and the 405's Allow header are both downstream of
		// this. It sits ahead of the method guard too, since only the POST
		// handler reads the Origin header and putting it here is what
		// covers /healthz as well.
		if s.hostGuard != nil && !s.hostGuard(r.Host) {
			http.Error(w, "host is not this machine; refusing a request addressed to another name", http.StatusForbidden)
			s.logRequest(r, id, http.StatusForbidden, 0, 0, time.Since(start), "host refused")
			return
		}
		e, known := lookupEndpoint(r.URL.Path)
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

// maxRequestID bounds the request id this endpoint echoes and logs. The
// sender's value is single-lined and capped here, so a long or control-laden
// header cannot ride into the audit line or the answer's headers. A sender
// that sends more than this gets the first maxRequestID characters back
// rather than its own value, so the cap is stated in the README.
const maxRequestID = 64

// incomingRequestID is the caller's own id when it sent one, and a counter
// derived name when it did not. The counter is per server and starts at one,
// so a request sequence replayed against a fresh server produces the same
// transcript twice: the ids land in the answer's headers and in the audit
// line, and a run that cannot be replayed cannot be diffed against a
// baseline. The id is a correlation handle, not a secret, and it is minted
// only when the sender supplied nothing to correlate against.
func (s *Server) incomingRequestID(r *http.Request) string {
	if v := logcfg.Field(r.Header.Get("X-Request-Id"), maxRequestID); v != "" {
		return v
	}
	return fmt.Sprintf("toktop-%012d", s.reqSeq.Add(1))
}

func (s *Server) requestID(r *http.Request) string {
	if state, ok := r.Context().Value(ctxRequest{}).(*requestState); ok && state.id != "" {
		return state.id
	}
	return s.incomingRequestID(r)
}

// logRequest writes the one audit line a finished request produces, success
// and rejection alike. accepted counts events decoded off the wire, stored how
// many the feed took: a replayed POST after a lost 202 differs from a first
// send only in stored.
//
// A Server built as a literal rather than through newServer carries no logger
// (newServer substitutes a discarding one for a nil argument), so the nil
// check is what keeps such a handler from panicking on its first request.
func (s *Server) logRequest(r *http.Request, reqID string, status, accepted, stored int, d time.Duration, errMsg string, extra ...any) {
	if s.log == nil {
		return
	}
	path := r.URL.Path
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
