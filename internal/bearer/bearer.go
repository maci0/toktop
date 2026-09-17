// Package bearer carries one process-wide optional Bearer token applied to
// engine HTTP requests (routers like OmniRoute require API keys even for
// model listing). The token is scoped: it rides requests only to
// destinations admitted via Allow, which callers reserve for endpoints the
// operator named explicitly (--add). Discovery probes dozens of localhost
// ports on spec, and whatever answers there is entitled to nothing, so a
// hostile listener on a scanned port cannot harvest a gateway API key that
// was never meant for it. Set once at startup, before discovery spawns
// goroutines.
package bearer

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

var (
	mu      sync.RWMutex
	tok     string
	allowed = map[string]bool{}
)

// Set stores the token applied to allowed engine requests.
func Set(token string) {
	mu.Lock()
	tok = token
	mu.Unlock()
}

// Allow admits one engine base URL as a token destination: every request
// bound for its origin may carry the Authorization header. Meant for
// endpoints the operator pointed at explicitly (--add); discovered or
// forwarded candidates never qualify.
func Allow(base string) {
	if o := origin(base); o != "" {
		mu.Lock()
		allowed[o] = true
		mu.Unlock()
	}
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

func CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if len(via) == 0 || originOf(req.URL) != originOf(via[0].URL) || !admits(req.URL) {
		req.Header.Del("Authorization")
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
// case, IPv6 brackets) collapse.
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
	return u.Scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}
