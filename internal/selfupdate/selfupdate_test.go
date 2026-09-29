// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package selfupdate

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// NewerThan decides whether --update replaces the running binary. The
// comparison is deliberately exact rather than semver-aware: releases are
// the source of truth, so any different tag applies, a published downgrade
// included, and only an identical or tagless release must be skipped.
func TestNewerThanIsExactNotSemver(t *testing.T) {
	cases := []struct {
		tag, current string
		want         bool
	}{
		{"v1.2.3", "1.2.3", false},  // same release, both spellings of v
		{"1.2.3", "v1.2.3", false},  //
		{"v1.3.0", "v1.3.0", false}, // dev build reporting its own release
		{"v1.3.0", "1.2.3", true},   // ordinary upgrade
		{"v1.2.2", "1.2.3", true},   // a published downgrade applies on purpose
		{"v2.0.0-pre.1", "2.0.0-pre.0", true},
		{"", "1.2.3", false}, // tagless release must not loop updates forever
		{"", "", false},      //
	}
	for _, c := range cases {
		rel := &Release{TagName: c.tag}
		if got := rel.NewerThan(c.current); got != c.want {
			t.Errorf("(&Release{TagName:%q}).NewerThan(%q) = %v, want %v",
				c.tag, c.current, got, c.want)
		}
	}
}

func TestValidateRepoAcceptsGitHubOwnerName(t *testing.T) {
	for _, repo := range []string{DefaultRepo, "a/b", "Org-Name/my.repo_1", "nccgroup/exploit"} {
		if err := ValidateRepo(repo); err != nil {
			t.Errorf("ValidateRepo(%q) = %v, want nil", repo, err)
		}
	}
}

func TestValidateRepoRejectsPathInjection(t *testing.T) {
	for _, repo := range []string{
		"",
		"toktop",
		"maci0/toktop/extra",
		"maci0/toktop/../../../users/octocat",
		"maci0/toktop?foo=1",
		"maci0/toktop#frag",
		"maci0/toktop\nAuthorization: Bearer x",
		"maci0/toktop ",
		"/maci0/toktop",
		"../evil/toktop",
		"maci0/..",
		"maci0/.",
		"-user/toktop",
		"user-/toktop",
		"maci0/toktop/releases/latest",
	} {
		if err := ValidateRepo(repo); err == nil {
			t.Errorf("ValidateRepo(%q) = nil, want error", repo)
		}
	}
}

func TestGitHubAssetURL(t *testing.T) {
	ok := []string{
		"https://github.com/maci0/toktop/releases/download/v1/toktop",
		"https://objects.githubusercontent.com/github-production-release-asset/1",
		"https://release-assets.githubusercontent.com/github-production-release-asset/1",
		"https://api.github.com/repos/maci0/toktop/releases/assets/1",
	}
	for _, u := range ok {
		if !TrustedReleaseURL(u) {
			t.Errorf("TrustedReleaseURL(%q) = false, want true", u)
		}
	}
	bad := []string{
		"http://github.com/maci0/toktop/releases/download/v1/toktop",
		"https://evil.example/malware",
		"https://github.com.evil.example/malware",
		"https://githubusercontent.com.evil.example/x",
		"https://169.254.169.254/latest",
		"https://user:pass@github.com/maci0/toktop/releases/download/v1/toktop",
		"https://github.com:8443/maci0/toktop/releases/download/v1/toktop",
		"https://objects.githubusercontent.com:8443/github-production-release-asset/1",
		"file:///etc/passwd",
		"",
	}
	for _, u := range bad {
		if TrustedReleaseURL(u) {
			t.Errorf("TrustedReleaseURL(%q) = true, want false", u)
		}
	}
}

// A hop the redirect policy follows carries the same authority as an asset
// URL, so the port is held to the same bound there: an allowlist that matched
// the host alone would admit a service GitHub does not serve on the name the
// operator trusts.
func TestGitHubRedirectRefusesNonHTTPSPort(t *testing.T) {
	for _, raw := range []string{
		"https://github.com:8443/maci0/toktop/releases/download/v1/toktop",
		"https://release-assets.githubusercontent.com:8443/asset",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("url.Parse(%q) = %v", raw, err)
		}
		req := &http.Request{URL: u, Header: http.Header{}}
		if err := githubRedirect(req, []*http.Request{{URL: &url.URL{Scheme: "https", Host: "api.github.com"}}}); err == nil {
			t.Errorf("githubRedirect(%q) = nil, want refusal", raw)
		}
	}
}

func TestChecksumForRejectsNonHex(t *testing.T) {
	name := "toktop_1.2.3_linux_amd64"
	okHex := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if sum, ok := ChecksumFor(okHex+"  "+name+"\n", name); !ok || sum != okHex {
		t.Fatalf("valid hex rejected: %q %v", sum, ok)
	}
	junk := "e3b0c44298fc1c149afbf4c8996fb9\x7f\x00\x00\x00a\xee41e4649b934ca495991b7852b855"
	if sum, ok := ChecksumFor(junk+" *"+name+"\n", name); ok {
		t.Fatalf("non-hex 64-byte field accepted: %q", sum)
	}
}

func TestCheckRejectsBadRepoWithoutNetwork(t *testing.T) {
	// The contract is ValidateRepo refusing the string, not Check returning
	// some error. Dropping the check lets the traversal reach the network and
	// come back as a transport or 4xx failure, which err == nil alone would
	// still accept, so the assertion is on which refusal it is.
	const bad = "maci0/toktop/../../../users/octocat"
	want := ValidateRepo(bad)
	if want == nil {
		t.Fatal("ValidateRepo accepted a path-injecting repo")
	}
	_, err := Check(context.Background(), bad)
	if err == nil || err.Error() != want.Error() {
		t.Fatalf("Check(%q) error = %v, want the ValidateRepo refusal %v", bad, err, want)
	}
}

func TestGitHubRedirectStaysOnGitHub(t *testing.T) {
	req := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Request{URL: u}
	}
	via := []*http.Request{req("https://api.github.com/repos/maci0/toktop/releases/latest")}
	cdnReq := req("https://objects.githubusercontent.com/file")
	cdnReq.Header = http.Header{"Authorization": []string{"Bearer secret"}}
	if err := githubRedirect(cdnReq, via); err != nil {
		t.Fatalf("cdn hop refused: %v", err)
	}
	if cdnReq.Header.Get("Authorization") != "" {
		t.Fatal("Authorization header leaked to CDN host on redirect")
	}
	// The strip is host-conditional, and only the strip direction was
	// asserted: a rule that deleted the header on every hop would pass every
	// refusal case below while breaking the authenticated api.github.com hop.
	apiReq := req("https://api.github.com/repos/maci0/toktop/releases/12345")
	apiReq.Header = http.Header{"Authorization": []string{"Bearer secret"}}
	if err := githubRedirect(apiReq, via); err != nil {
		t.Fatalf("api.github.com hop refused: %v", err)
	}
	if apiReq.Header.Get("Authorization") != "Bearer secret" {
		t.Fatal("Authorization stripped from an api.github.com hop")
	}
	if err := githubRedirect(req("https://user:pass@objects.githubusercontent.com/file"), via); err == nil {
		t.Fatal("userinfo redirect allowed")
	}
	if err := githubRedirect(req("https://evil.example/malware"), via); err == nil {
		t.Fatal("off-site hop allowed")
	}
	if err := githubRedirect(req("http://github.com/x"), via); err == nil {
		t.Fatal("http downgrade allowed")
	}
	// Replacing the client's default policy also caps the chain, so a
	// server that keeps redirecting to itself is stopped rather than followed.
	if err := githubRedirect(req("https://github.com/x"), make([]*http.Request, 10)); err == nil {
		t.Fatal("redirect chain past the hop cap allowed")
	}
}

func TestApplyRejectsNonGitHubAssetURL(t *testing.T) {
	rel := &Release{TagName: "v9.9.9"}
	rel.Assets = append(rel.Assets,
		struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}{Name: AssetName("9.9.9"), URL: "https://evil.example/asset"},
		struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}{Name: checksumsName("9.9.9"), URL: "https://evil.example/checksums"},
	)
	if _, err := applyTo(t.Context(), rel, t.TempDir()+"/toktop"); err == nil ||
		!strings.Contains(err.Error(), "GitHub download") {
		t.Fatalf("non-GitHub asset URL must be refused, got %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCheck(t *testing.T) {
	orig := client.Transport
	defer func() { client.Transport = orig }()

	t.Run("success", func(t *testing.T) {
		client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if !strings.HasSuffix(req.URL.Path, "/releases/latest") {
				return nil, fmt.Errorf("unexpected path: %s", req.URL.Path)
			}
			body := `{"tag_name":"v1.2.3","assets":[{"name":"toktop_1.2.3_linux_amd64","browser_download_url":"https://github.com/foo/bar"}]}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		})
		rel, err := Check(context.Background(), "maci0/toktop")
		if err != nil {
			t.Fatalf("Check() err = %v", err)
		}
		if rel.TagName != "v1.2.3" {
			t.Fatalf("TagName = %q, want v1.2.3", rel.TagName)
		}
	})

	for _, tc := range []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"http error", http.StatusNotFound, `{"message":"Not Found"}`, "404"},
		{"missing tag", http.StatusOK, `{"tag_name":""}`, "no tag"},
		{"invalid json", http.StatusOK, `{invalid json`, "cannot parse"},
		{"rate limited", http.StatusForbidden, `{"message":"API rate limit exceeded"}`, "$GITHUB_TOKEN"},
		{"throttled", http.StatusTooManyRequests, `{"message":"slow down"}`, "$GITHUB_TOKEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: tc.status,
					Status:     fmt.Sprintf("%d %s", tc.status, http.StatusText(tc.status)),
					Body:       io.NopCloser(strings.NewReader(tc.body)),
					Header:     make(http.Header),
				}, nil
			})
			_, err := Check(context.Background(), "maci0/toktop")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Check() err = %v, want %s error", err, tc.wantErr)
			}
		})
	}
}

// The token read from the environment must be what a header can carry: a
// trailing newline from `$(cat token)` is stripped, and a line break
// anywhere else is named instead of reaching net/http as an invalid header.
func TestCheckSendsUsableTokenOnly(t *testing.T) {
	orig := client.Transport
	defer func() { client.Transport = orig }()

	for _, tc := range []struct {
		name     string
		token    string
		wantAuth string
		wantErr  bool
	}{
		{name: "unset sends no token"},
		{name: "trailing newline is stripped", token: "ghp_x\n", wantAuth: "Bearer ghp_x"},
		{name: "plain token is sent", token: "ghp_x", wantAuth: "Bearer ghp_x"},
		{name: "interior newline is refused", token: "ghp_x\nghp_y", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(TokenEnv, tc.token)
			var got string
			client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				got = req.Header.Get("Authorization")
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Body:       io.NopCloser(strings.NewReader(`{"tag_name":"v1.2.3"}`)),
					Header:     make(http.Header),
				}, nil
			})
			_, err := Check(context.Background(), "maci0/toktop")
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), TokenEnv) {
					t.Fatalf("Check() err = %v, want an error naming $%s", err, TokenEnv)
				}
				return
			}
			if err != nil {
				t.Fatalf("Check() err = %v", err)
			}
			if got != tc.wantAuth {
				t.Fatalf("Authorization = %q, want %q", got, tc.wantAuth)
			}
		})
	}
}

func TestFileChecksumRejectsOversized(t *testing.T) {
	orig := maxAssetBytes
	maxAssetBytes = 10
	t.Cleanup(func() { maxAssetBytes = orig })

	dir := t.TempDir()
	path := filepath.Join(dir, "oversized")
	if err := os.WriteFile(path, []byte("this is more than ten bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := fileChecksum(path)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("fileChecksum err = %v, want exceeds error", err)
	}
}

// `toktop update --check` prints the release page for shell capture, so the
// page URL is held to the GitHub-host rule the downloads already pass.
func TestTrustedReleaseURL(t *testing.T) {
	for _, ok := range []string{
		"https://github.com/maci0/toktop/releases/tag/v0.15.0",
		"https://api.github.com/repos/maci0/toktop/releases/1",
	} {
		if !TrustedReleaseURL(ok) {
			t.Errorf("TrustedReleaseURL(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{
		"",
		"http://github.com/maci0/toktop/releases/tag/v1",
		"https://evil.example/releases/tag/v1",
		"https://github.com.evil.example/x",
		"javascript:alert(1)",
		"https://user:pass@github.com/x",
		// The release page lands in the operator's shell through
		// url=$(toktop update --check), so a byte the shell reads as
		// anything but part of the word is refused even on a trusted host.
		"https://github.com/o/r/releases/tag/v1 x$(id)",
		"https://github.com/o/r/releases/tag/v1;id",
		"https://github.com/o/r/releases/tag/v1`id`",
		"https://github.com/o/r/releases/tag/v1'x'",
		"https://github.com/o/r/releases/tag/v1\"x\"",
		"https://github.com/o/r/releases/tag/v1\\x",
		"https://github.com/o/r/releases/tag/v1é",
	} {
		if TrustedReleaseURL(bad) {
			t.Errorf("TrustedReleaseURL(%q) = true, want false", bad)
		}
	}
}

// An update replaces the bytes, not the permission bits. A binary installed
// deliberately narrow (0700, a private build on a shared host) came back
// world-executable under a hardcoded 0755, and nothing in the output said so.
func TestInstallModeCarriesTheInstalledModeForward(t *testing.T) {
	for _, perm := range []fs.FileMode{0o700, 0o750, 0o755} {
		dir := t.TempDir()
		self := filepath.Join(dir, "toktop")
		if err := os.WriteFile(self, []byte("old"), perm); err != nil {
			t.Fatalf("seed %s: %v", perm, err)
		}
		if err := os.Chmod(self, perm); err != nil {
			t.Fatalf("chmod seed %s: %v", perm, err)
		}
		if got := installMode(self); got != perm {
			t.Errorf("installMode(%s) = %o, want %o", perm, got, perm)
		}
	}
}

// A binary that cannot be stat'd (first install, or a path that vanished)
// still has to end up executable.
func TestInstallModeFallsBackWhenTheTargetIsGone(t *testing.T) {
	got := installMode(filepath.Join(t.TempDir(), "absent"))
	if got != defaultInstallMode {
		t.Errorf("installMode(absent) = %o, want %o", got, defaultInstallMode)
	}
}

// Recovery runs before the download and before the checksum, so it is the one
// place content reaches the executable unverified. It must promote a leftover
// of an update and refuse anything else.
func TestRestoreDisplacedRefusesWhatAnUpdateDidNotLeave(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "toktop")
	displaced := self + displacedSuffix

	t.Run("regular file is restored", func(t *testing.T) {
		if err := os.WriteFile(displaced, []byte("binary"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := restoreDisplaced(self, displaced); err != nil {
			t.Fatalf("restore: %v", err)
		}
		if _, err := os.Stat(self); err != nil {
			t.Fatalf("not restored: %v", err)
		}
	})

	t.Run("symlink is refused", func(t *testing.T) {
		os.Remove(self)
		target := filepath.Join(dir, "elsewhere")
		if err := os.WriteFile(target, []byte("attacker"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, displaced); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := restoreDisplaced(self, displaced); err == nil {
			t.Fatal("restored a symlink onto the install path")
		}
		if _, err := os.Lstat(self); !os.IsNotExist(err) {
			t.Error("install path exists after refusing the symlink")
		}
	})

	t.Run("empty file is not promoted", func(t *testing.T) {
		os.Remove(self)
		os.Remove(displaced) // the symlink the previous case left behind
		if err := os.WriteFile(displaced, nil, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := restoreDisplaced(self, displaced); err != nil {
			t.Fatalf("an empty leftover must be skipped, not fail the update: %v", err)
		}
		if _, err := os.Stat(self); !os.IsNotExist(err) {
			t.Error("an empty file was promoted onto the install path, leaving a binary no platform can run")
		}
	})

	t.Run("directory is refused", func(t *testing.T) {
		os.Remove(displaced)
		if err := os.Mkdir(displaced, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := restoreDisplaced(self, displaced); err == nil {
			t.Fatal("restored a directory onto the install path")
		}
		if _, err := os.Lstat(self); !os.IsNotExist(err) {
			t.Error("install path exists after refusing the directory")
		}
	})
}
