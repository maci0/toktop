package bearer

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func resetAllowed() {
	mu.Lock()
	allowed = map[string]bool{}
	mu.Unlock()
}

func TestSetAndApply(t *testing.T) {
	t.Cleanup(func() { Set(""); resetAllowed() })
	Set("")

	req, _ := http.NewRequest("GET", "http://x/metrics", nil)
	Apply(req)
	if req.Header.Get("Authorization") != "" {
		t.Error("header must stay unset without a token")
	}

	Set("sk-test")
	// A token alone is not enough: undiscovered, un-admitted destinations
	// get nothing, so a hostile listener on a probed port cannot collect it.
	Apply(req)
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q for a destination never allowed", got)
	}

	Allow("http://x")
	Apply(req)
	if got := req.Header.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("Authorization = %q after Allow, want %q", got, "Bearer sk-test")
	}
}

func TestSetRejectsCRLF(t *testing.T) {
	t.Cleanup(func() { Set(""); resetAllowed() })
	err := Set("sk-test\r\nInjected-Header: value")
	// The refusal must be visible: dropped silently, the operator watches
	// every engine request come back 401 with no idea why.
	if err == nil {
		t.Error("Set with CRLF returned nil, want a refusal the caller can report")
	}
	if err := Allow("http://x"); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	req, _ := http.NewRequest("GET", "http://x/metrics", nil)
	Apply(req)
	if req.Header.Get("Authorization") != "" {
		t.Errorf("Authorization must be unset for token with CRLF, got %q", req.Header.Get("Authorization"))
	}
}

// A base that yields no http origin is an authorization the operator gave
// that never took effect. Reporting it distinguishes "the token was refused"
// from "the key is wrong".
func TestAllowRejectsBaseWithNoOrigin(t *testing.T) {
	t.Cleanup(func() { Set(""); resetAllowed() })
	Set("sk-test")
	for _, base := range []string{"localhost:8000", "//host:8000", "", "://"} {
		if err := Allow(base); err == nil {
			t.Errorf("Allow(%q) returned nil, want a refusal the caller can report", base)
		}
	}
	req, _ := http.NewRequest("GET", "http://x/metrics", nil)
	Apply(req)
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q after a refused Allow, want none", got)
	}
}

func TestAllowScopesByOrigin(t *testing.T) {
	t.Cleanup(func() { Set(""); resetAllowed() })
	Set("sk-test")

	cases := []struct {
		allowed string
		target  string
		want    bool
	}{
		{"http://10.0.0.5:8000/", "http://10.0.0.5:8000/v1/models", true},
		{"http://10.0.0.5:8000", "http://10.0.0.5:8001/v1/models", false}, // other port
		{"http://127.0.0.1:20128", "https://127.0.0.1:20128/v1/models", false},
		{"http://omni.lan", "http://omni.lan/v1/models", true}, // default port implied
		{"http://OMNI.lan", "http://omni.lan/v1/models", true}, // host case folds
		{"http://[::1]:8420", "http://[::1]:8420/api/version", true},
		{"http://[::1]:8420", "http://127.0.0.1:8420/api/version", false},
		{"http://caf\u00e9.lan:8000", "http://cafe\u0301.lan:8000/v1/models", true},
		{"http://cafe\u0301.lan:8000", "http://caf\u00e9.lan:8000/v1/models", true},
	}
	for _, tc := range cases {
		resetAllowed()
		Allow(tc.allowed)
		req, err := http.NewRequest("GET", tc.target, nil)
		if err != nil {
			t.Fatalf("bad target %q: %v", tc.target, err)
		}
		Apply(req)
		got := req.Header.Get("Authorization") == "Bearer sk-test"
		if got != tc.want {
			t.Errorf("Allow(%q) then request %q: attached = %v, want %v",
				tc.allowed, tc.target, got, tc.want)
		}
	}

	// Malformed bases and non-HTTP(S) schemes are refused rather than admitted.
	for _, bad := range []string{"", "not a url", "ftp://x", "http://"} {
		resetAllowed()
		Allow(bad)
		if len(allowed) != 0 {
			t.Errorf("Allow(%q) admitted something", bad)
		}
	}
}

func TestCheckRedirectScopesAuthorization(t *testing.T) {
	t.Cleanup(resetAllowed)
	for _, tc := range []struct {
		name    string
		target  string
		allow   bool
		wantErr bool
	}{
		{name: "same origin", target: "https://engine.local/next", allow: true},
		{name: "default port", target: "https://ENGINE.local:443/next", allow: true},
		{name: "other port", target: "https://engine.local:8443/next", allow: true, wantErr: true},
		{name: "downgrade", target: "http://engine.local/next", allow: true, wantErr: true},
		{name: "subdomain", target: "https://sub.engine.local/next", allow: true, wantErr: true},
		{name: "not admitted", target: "https://engine.local/next", wantErr: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetAllowed()
			if tc.allow {
				Allow(tc.target)
			}
			first, err := http.NewRequest(http.MethodGet, "https://engine.local/", nil)
			if err != nil {
				t.Fatal(err)
			}
			next, err := http.NewRequest(http.MethodGet, tc.target, nil)
			if err != nil {
				t.Fatal(err)
			}
			next.Header.Set("Authorization", "Bearer sk-test")
			err = CheckRedirect(next, []*http.Request{first})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("hop to %s was allowed, want refusal", tc.target)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := next.Header.Get("Authorization"); got != "Bearer sk-test" {
				t.Errorf("Authorization = %q, want it kept on a same-origin hop", got)
			}
			if err := CheckRedirect(next, make([]*http.Request, 10)); err == nil {
				t.Fatal("redirect limit not enforced")
			}
		})
	}
}

// A hop that leaves the origin is refused, so the redirect target is never
// contacted at all: the engine a scanned port or an ssh-forwarded port answers
// for cannot turn a poll into a request to any URL the operator's host reaches.
func TestCheckRedirectRefusesCrossOriginHop(t *testing.T) {
	t.Cleanup(func() { Set(""); resetAllowed() })
	Set("sk-test")

	var reached atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
	}))
	defer other.Close()

	chain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("first hop Authorization = %q, want the token", got)
		}
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer chain.Close()

	Allow(chain.URL)
	req, _ := http.NewRequest("GET", chain.URL, nil)
	Apply(req)
	if got := req.Header.Get("Authorization"); got != "Bearer sk-test" {
		t.Fatalf("first hop lost the token: %q", got)
	}
	c := &http.Client{CheckRedirect: CheckRedirect}
	resp, err := c.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("cross-origin redirect was followed")
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("redirect target was contacted %d time(s), want 0", n)
	}
}

// A same-origin redirect still works, and the token rides the hop: an engine
// that answers a poll with a redirect to another path on itself is normal.
func TestCheckRedirectFollowsSameOriginChain(t *testing.T) {
	t.Cleanup(func() { Set(""); resetAllowed() })
	Set("sk-test")

	var sawAuth atomic.Value
	sawAuth.Store("")
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth.Store(r.Header.Get("Authorization"))
		fmt.Fprint(w, `{}`)
	}))
	defer final.Close()

	chain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			sawAuth.Store(r.Header.Get("Authorization"))
			fmt.Fprint(w, `{}`)
			return
		}
		http.Redirect(w, r, "/final", http.StatusFound)
	}))
	defer chain.Close()

	Allow(chain.URL)
	req, _ := http.NewRequest("GET", chain.URL, nil)
	Apply(req)
	resp, err := (&http.Client{CheckRedirect: CheckRedirect}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if a := sawAuth.Load().(string); a != "Bearer sk-test" {
		t.Errorf("Authorization = %q on the same-origin hop, want the token", a)
	}
}
