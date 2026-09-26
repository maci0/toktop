package bearer

import (
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
	Set("sk-test\r\nInjected-Header: value")
	Allow("http://x")
	req, _ := http.NewRequest("GET", "http://x/metrics", nil)
	Apply(req)
	if req.Header.Get("Authorization") != "" {
		t.Errorf("Authorization must be unset for token with CRLF, got %q", req.Header.Get("Authorization"))
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
		name   string
		target string
		allow  bool
		want   string
	}{
		{"same origin", "https://engine.local/next", true, "Bearer sk-test"},
		{"default port", "https://ENGINE.local:443/next", true, "Bearer sk-test"},
		{"other port", "https://engine.local:8443/next", true, ""},
		{"downgrade", "http://engine.local/next", true, ""},
		{"subdomain", "https://sub.engine.local/next", true, ""},
		{"not admitted", "https://engine.local/next", false, ""},
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
			if err := CheckRedirect(next, []*http.Request{first}); err != nil {
				t.Fatal(err)
			}
			if got := next.Header.Get("Authorization"); got != tc.want {
				t.Errorf("Authorization = %q, want %q", got, tc.want)
			}
			if err := CheckRedirect(next, make([]*http.Request, 10)); err == nil {
				t.Fatal("redirect limit not enforced")
			}
		})
	}
}

func TestCheckRedirectStripsHeaderOnCrossOriginChain(t *testing.T) {
	t.Cleanup(func() { Set(""); resetAllowed() })
	Set("sk-test")

	var sawAuth atomic.Value
	sawAuth.Store("")
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth.Store(r.Header.Get("Authorization"))
	}))
	defer other.Close()

	chain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if a := sawAuth.Load().(string); a != "" {
		t.Errorf("Authorization = %q reached the redirect target, want unset", a)
	}
}
