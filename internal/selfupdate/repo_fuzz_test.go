// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package selfupdate

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"

	"github.com/maci0/toktop/internal/core"
)

// FuzzValidateRepo drives the repo string Check interpolates into the releases
// API path, and asserts across that boundary: a repo the validator admits must
// produce exactly the request target the owner/name names, on api.github.com,
// with no query, fragment or userinfo and no path segment the caller did not
// write. That is the property the validator exists for, and it holds only if
// the two halves of the check (githubOwner, githubRepoName) are exactly the
// predicate ValidateRepo applies, so the harness pins them together as well.
func FuzzValidateRepo(f *testing.F) {
	for _, repo := range []string{
		"",
		"maci0/toktop",
		"a/b",
		"toktop",
		"/toktop",
		"maci0/",
		"maci0//toktop",
		"maci0/toktop/extra",
		"../etc/passwd",
		"maci0/../../etc",
		"maci0/%2e%2e",
		"maci0/..",
		"maci0/.",
		"maci0/tok top",
		"maci0/toktop?x=1",
		"maci0/toktop#f",
		"maci0/toktop\r\nX-Injected: 1",
		"maci0/tok\ntop",
		"maci0\\toktop",
		"maci0/tok%2Ftop",
		"maci0/toktop/",
		"-maci0/toktop",
		"maci0-/toktop",
		"maci0/toktop.git",
		"maci0/tok_top",
		"m/t",
		"9/9",
		strings.Repeat("a", 39) + "/toktop",
		strings.Repeat("a", 40) + "/toktop",
		"maci0/" + strings.Repeat("b", 100),
		"maci0/" + strings.Repeat("b", 101),
		"måciø/toktop",
		"MA/CI0/Toktop",
		"\u212aevin/toktop",
		"maci0/toktop\u0000",
	} {
		f.Add(repo)
	}

	f.Fuzz(func(t *testing.T, repo string) {
		err := ValidateRepo(repo)
		owner, name, cut := strings.Cut(repo, "/")
		// The oracle the validator is defined by. Divergence between this and
		// the result means one of the two halves changed meaning, and a repo
		// that reaches the API path unchecked is a traversal out of the
		// operator's configured repository.
		want := cut && owner != "" && name != "" && !strings.Contains(name, "/") &&
			githubOwner(owner) && githubRepoName(name)
		if got := err == nil; got != want {
			t.Fatalf("ValidateRepo(%q) = %v, want admitted=%v", repo, err, want)
		}
		if err != nil {
			return
		}

		latest, joinErr := url.JoinPath("https://api.github.com/repos", owner, name, "releases", "latest")
		if joinErr != nil {
			t.Fatalf("admitted repo %q built an unparseable URL: %v", repo, joinErr)
		}
		u, parseErr := url.Parse(latest)
		if parseErr != nil {
			t.Fatalf("admitted repo %q built an unparseable URL %q: %v", repo, latest, parseErr)
		}
		if u.Scheme != "https" || u.Host != "api.github.com" || u.User != nil {
			t.Fatalf("admitted repo %q moved the request to %q", repo, u.Redacted())
		}
		if u.RawQuery != "" || u.Fragment != "" {
			t.Fatalf("admitted repo %q added a query or fragment: %q", repo, latest)
		}
		if want := "/repos/" + owner + "/" + name + "/releases/latest"; u.Path != want {
			t.Fatalf("admitted repo %q built path %q, want %q", repo, u.Path, want)
		}
		// The request target is fetched with the Authorization header set, so
		// it is held to the same host rule every download hop is.
		if !TrustedReleaseURL(latest) {
			t.Fatalf("admitted repo %q built a URL the download allowlist refuses: %q", repo, latest)
		}
		// No segment the operator did not write. The check is per segment,
		// not a substring: GitHub accepts dots inside a name, so "tok..top" is
		// a repository and only a segment that is "." or ".." re-resolves.
		for _, seg := range strings.Split(u.EscapedPath(), "/") {
			if unescaped, err := url.PathUnescape(seg); err != nil {
				t.Fatalf("admitted repo %q built an unescapable path %q", repo, u.EscapedPath())
			} else if unescaped == "." || unescaped == ".." {
				t.Fatalf("admitted repo %q left a traversal segment in %q", repo, u.EscapedPath())
			}
		}
		if gotOwner, gotName, ok := strings.Cut(strings.TrimPrefix(u.Path, "/repos/"), "/"); !ok ||
			gotOwner != owner || !strings.HasPrefix(gotName, name+"/") {
			t.Fatalf("admitted repo %q does not read back from %q", repo, u.Path)
		}
	})
}

// FuzzGithubRedirectAuth drives the redirect policy on a hop chosen by whoever
// answers the previous request. It is the only gate between a GITHUB_TOKEN
// bearing client and a host the operator never named, so the two things it
// decides are asserted rather than assumed: a hop it refuses is not one the
// allowlist would admit, and a hop it follows off api.github.com leaves
// without the Authorization header. The hop count is fuzzed with the target
// because the cap is the other half of the policy.
func FuzzGithubRedirectAuth(f *testing.F) {
	for _, hop := range []string{
		"",
		"https://github.com/a/b/releases/download/v1/toktop_1_linux_amd64",
		"https://api.github.com/repos/maci0/toktop/releases/latest",
		"https://API.GITHUB.COM/repos/maci0/toktop/releases/latest",
		"https://objects.githubusercontent.com/x",
		"https://evil.githubusercontent.com/x",
		"https://githubusercontent.com.evil.example/x",
		"https://githubusercontent.com/x",
		"https://user:pass@github.com/x",
		"https://user:pass@evil.example/x",
		"http://github.com/x",
		"http://api.github.com/x",
		"//github.com/x",
		"/relative/path",
		"https://[::1]/x",
		"https://127.0.0.1:8443/x",
		"https://169.254.169.254/latest/meta-data/",
		"https://\u212aevin.github.com/x",
		"https://localhost/github.com",
	} {
		for _, hops := range []int{0, 1, maxRedirects - 1, maxRedirects, maxRedirects + 1} {
			f.Add(hop, hops)
		}
	}

	f.Fuzz(func(t *testing.T, raw string, hops int) {
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer secret-token")

		// via is a slice net/http grows, so it is never negative and never
		// longer than the cap plus the one hop being checked. Fold the fuzzed
		// count into that range rather than dropping out of range inputs, so
		// the cap boundary stays under the fuzzer.
		if hops < 0 {
			hops = -hops
		}
		hops %= maxRedirects + 2

		via := make([]*http.Request, hops)
		err = githubRedirect(req, via)

		if hops >= maxRedirects {
			if err == nil {
				t.Fatalf("hop %d to %q followed past the redirect cap", hops, req.URL.Redacted())
			}
			return
		}
		admissible := req.URL.Scheme == "https" && req.URL.User == nil && githubDownloadHost(req.URL.Hostname())
		if err == nil {
			if !admissible {
				t.Fatalf("followed a hop to %q, which the allowlist refuses", req.URL.Redacted())
			}
			// The header is dropped on every host but the API's, so a hop to
			// a download host that kept it hands the token to a CDN.
			if core.FoldASCII(norm.NFC.String(req.URL.Hostname())) != "api.github.com" && req.Header.Get("Authorization") != "" {
				t.Fatalf("hop to %q kept the Authorization header", req.URL.Redacted())
			}
			return
		}
		if admissible {
			t.Fatalf("refused an admissible hop to %q: %v", req.URL.Redacted(), err)
		}
		// The refusal names the URL, and it must name the redacted one. The
		// check is that the redacted spelling is what the message carries, not
		// a search for the password: the sender chooses the rest of the URL
		// and can always spell the password again in the path or the
		// fragment, so only the redacted form being present says anything.
		if redacted := req.URL.Redacted(); !strings.Contains(err.Error(), redacted) {
			t.Fatalf("refusal for %q does not name the redacted URL %q: %v", raw, redacted, err)
		}
	})
}
