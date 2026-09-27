// Package bearer carries one process-wide optional Bearer token applied to
// engine HTTP requests (routers like OmniRoute require API keys even for
// model listing). The token is scoped: it rides requests only to
// destinations admitted via Allow, which callers reserve for endpoints the
// operator named explicitly (--add). Discovery probes dozens of localhost
// ports on spec, and whatever answers there is entitled to nothing, so a
// hostile listener on a scanned port cannot harvest a gateway API key that
// was never meant for it. Set once at startup, before discovery spawns
// goroutines.
//
// The package also owns the redirect policy every engine request runs under
// (CheckRedirect), which confines a chain to the origin it started on for the
// same reason.
package bearer

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/maci0/toktop/internal/core"
	"golang.org/x/text/unicode/norm"
)

var (
	mu      sync.RWMutex
	tok     string
	allowed = map[string]bool{}
)

// Set stores the token applied to allowed engine requests. Tokens containing
// CRLF or newline characters are refused to prevent HTTP header injection.
// A refusal is returned, not silently downgraded to "no token": an operator
// who supplied a credential must not watch every request come back 401
// without being told the credential was refused.
func Set(token string) error {
	token = strings.TrimSpace(token)
	if strings.ContainsAny(token, "\r\n") {
		return errors.New("bearer token contains CR or LF, refusing to send it as a header")
	}
	mu.Lock()
	tok = token
	mu.Unlock()
	return nil
}

// Allow admits one engine base URL as a token destination: every request
// bound for its origin may carry the Authorization header. Meant for
// endpoints the operator pointed at explicitly (--add); discovered or
// forwarded candidates never qualify. A base that yields no http/https
// origin is reported rather than skipped, since the operator authorized that
// destination and would otherwise get an unauthenticated 401.
func Allow(base string) error {
	o := origin(base)
	if o == "" {
		return fmt.Errorf("cannot derive an http origin from %q for bearer token use", base)
	}
	mu.Lock()
	allowed[o] = true
	mu.Unlock()
	return nil
}

// Apply sets the Authorization header when a token is configured and the
// request is bound for an allowed destination.
func Apply(req *http.Request) {
	mu.RLock()
	t := tok
	mu.RUnlock()
	if t == "" || !admits(req.URL) {
		return
	}
	req.Header.Set("Authorization", "Bearer "+t)
}

// CheckRedirect is the redirect policy for every engine request, so it carries
// both the credential rule and the destination rule.
//
// A hop off the original origin is refused rather than followed. Every engine
// request starts from an address nobody authenticated as a toktop peer: a
// scanned loopback port, or a port forwarded over ssh from a remote host the
// operator merely wants to watch. Answering a poll with a redirect would make
// that listener a confused deputy, turning a read of 127.0.0.1:8080 into a
// request to any URL the operator's host can reach, including cloud instance
// metadata and services on the operator's LAN, and surfacing the answer in the
// dashboard. Confining the chain to one origin also covers the https to http
// downgrade the origin comparison treats as a different origin, so no engine
// token is ever sent in cleartext to a hop that chose the scheme.
//
// The Authorization header needs no stripping for the same reason: a hop that
// leaves the origin is never made, and a hop within it lands on the origin
// Apply already admitted or already refused. The hop count stays capped.
func CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if len(via) == 0 {
		return nil
	}
	from, to := originOf(via[0].URL), originOf(req.URL)
	if to == "" {
		return fmt.Errorf("refusing redirect from %s to a non-http destination", from)
	}
	if from != to {
		return fmt.Errorf("refusing redirect from %s to %s", from, to)
	}
	return nil
}

func admits(u *url.URL) bool {
	o := originOf(u)
	if o == "" {
		return false
	}
	mu.RLock()
	defer mu.RUnlock()
	return allowed[o]
}

func origin(raw string) string {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil {
		return ""
	}
	return originOf(u)
}

// originOf renders a URL's scheme://host:port identity. Both sides of the
// comparison go through it, so spelling differences (default port, host
// case, IPv6 brackets) collapse. Case folds ASCII only: a host label that
// carries U+212A would fold to "k" under strings.ToLower and reach an
// allowlist entry its producer never wrote, which is the one direction a
// wrong fold must not err in.
func originOf(u *url.URL) string {
	if u == nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		default:
			port = "80"
		}
	}
	return u.Scheme + "://" + net.JoinHostPort(core.FoldASCII(norm.NFC.String(u.Hostname())), port)
}
