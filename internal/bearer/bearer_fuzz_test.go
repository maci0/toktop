package bearer

import (
	"net/http"
	"net/url"
	"testing"
)

// FuzzCheckRedirect throws two operator-uncontrolled URLs at the redirect
// policy: `from` is the origin an engine request started on, `to` is whatever
// a listening endpoint chose to put in its Location header. A hostile or
// confused listener must never be able to walk the chain off that origin, so
// a nil error from CheckRedirect has to mean the two origins are identical.
func FuzzCheckRedirect(f *testing.F) {
	for _, seed := range [][2]string{
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080/metrics"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:9090/"},
		{"http://127.0.0.1:8080", "https://127.0.0.1:8080/"},
		{"https://x:443", "https://x/"},
		{"http://x", "http://169.254.169.254/latest/meta-data/"},
		{"http://[::1]:8080", "http://[0:0:0:0:0:0:0:1]:8080/"},
		{"http://x", "http://x.:8080/"},
		{"http://x", "http://x@evil.example/"},
		{"http://x", "/relative"},
		{"http://x", "file:///etc/passwd"},
		{"http://x", "gopher://x/"},
		{"http://x", ""},
		{"", "http://x/"},
		{"http://x", "http://x:00080/"},
		{"http://x", "http://X:8080/"},
		{"http://x", "http://x/\r\nX-Injected: 1"},
		{"", ""},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, fromRaw, toRaw string) {
		from, errFrom := url.Parse(fromRaw)
		to, errTo := url.Parse(toRaw)
		if errFrom != nil || errTo != nil {
			return // an unparseable URL never becomes a request
		}
		req := &http.Request{URL: to, Method: http.MethodGet}
		via := []*http.Request{{URL: from, Method: http.MethodGet}}

		err := CheckRedirect(req, via)
		if err == nil {
			if o, h := originOf(from), originOf(to); o != h {
				t.Fatalf("hop %q -> %q allowed: origins %q and %q differ", fromRaw, toRaw, o, h)
			}
		}
		if err := CheckRedirect(req, nil); err != nil {
			t.Fatalf("first hop refused (%v), want nil: the chain's own origin is always admissible", err)
		}
		// Ten hops is the cap, so a chain that long must stop whatever the
		// origins say.
		if err := CheckRedirect(req, make([]*http.Request, 10)); err == nil {
			t.Fatalf("hop %q -> %q allowed past the 10-hop cap", fromRaw, toRaw)
		}
	})
}

// FuzzAllowAdmitsOnlyItsOwnOrigin throws an admitted base URL and a request
// URL at the allowlist. The token rides the request only when the request's
// origin is the one the operator named, so every admitted destination and
// every non-admitted one must be classified by origin identity alone.
func FuzzAllowAdmitsOnlyItsOwnOrigin(f *testing.F) {
	for _, seed := range [][2]string{
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080/v1/models"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8081/v1/models"},
		{"https://x", "https://x/v1/models"},
		{"https://x", "https://X:443/v1/models"},
		{"https://x", "http://x/v1/models"},
		{"https://x", "https://x:8443/"},
		{"http://x", "http://x@evil.example/"},
		{"http://x", "http://x/../y"},
		{"http://x", "//evil.example/"},
		{"http://x", "http://x/?a=1"},
		{"", "http://x/"},
		{"http://x", ""},
		{"not a url at all", "http://x/"},
		{"ftp://x", "ftp://x/"},
		{"http://x", "http://x:99999/"},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, baseRaw, reqRaw string) {
		t.Cleanup(func() { Set(""); resetAllowed() })
		Set("")
		if err := Allow(baseRaw); err != nil {
			if origin(baseRaw) != "" {
				t.Fatalf("Allow(%q) refused an origin it can derive (%q): %v", baseRaw, origin(baseRaw), err)
			}
			return
		}
		u, err := url.Parse(reqRaw)
		if err != nil {
			return
		}
		req := &http.Request{URL: u, Method: http.MethodGet}
		Apply(req)

		got := req.Header.Get("Authorization")
		switch {
		case originOf(u) == origin(baseRaw):
			// Still unset: no token is configured, and a destination must
			// never receive a header it was not granted.
			if got != "" {
				t.Fatalf("Apply set %q with no token configured", got)
			}
		case got != "":
			t.Fatalf("Apply sent %q to %q, whose origin %q is not the admitted %q",
				got, reqRaw, originOf(u), origin(baseRaw))
		}
	})
}

// FuzzOriginOfIsStable pins the identity used on both sides of every origin
// comparison: the same URL must render the same origin every time, and a URL
// with no http origin must render the empty string so nothing is admitted by
// accident.
func FuzzOriginOfIsStable(f *testing.F) {
	for _, seed := range []string{
		"http://x", "https://x:443/", "http://x:80/", "http://[::1]:8080",
		"http://x#frag", "http://user:pw@x:8080/p?q=1", "HTTP://X/", "http://xn--kva.example/",
		"file:///tmp/x", "//x", "http://", "http:///path", "http://x:0/",
		"http://x:65536/", "http://\u212Ax.example/", "http://x:8080\t/",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		u, err := url.Parse(raw)
		if err != nil {
			return
		}
		got := originOf(u)
		if got != originOf(u) {
			t.Fatalf("originOf(%q) is not stable: %q then %q", raw, got, originOf(u))
		}
		if got == "" {
			return
		}
		if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			t.Fatalf("originOf(%q) = %q for a URL with no http origin", raw, got)
		}
		// The rendered origin must parse back to the same scheme, host and
		// port, or a later comparison against it compares nothing.
		back, err := url.Parse(got)
		if err != nil {
			t.Fatalf("originOf(%q) = %q does not parse: %v", raw, got, err)
		}
		if back.Scheme != u.Scheme || back.Port() != effectivePort(u) {
			t.Fatalf("originOf(%q) = %q, which does not round trip to %s://%s:%s",
				raw, got, u.Scheme, u.Host, effectivePort(u))
		}
	})
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}
