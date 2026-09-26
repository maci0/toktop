// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package selfupdate

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
		if !githubAssetURL(u) {
			t.Errorf("githubAssetURL(%q) = false, want true", u)
		}
	}
	bad := []string{
		"http://github.com/maci0/toktop/releases/download/v1/toktop",
		"https://evil.example/malware",
		"https://github.com.evil.example/malware",
		"https://githubusercontent.com.evil.example/x",
		"https://169.254.169.254/latest",
		"https://user:pass@github.com/maci0/toktop/releases/download/v1/toktop",
		"file:///etc/passwd",
		"",
	}
	for _, u := range bad {
		if githubAssetURL(u) {
			t.Errorf("githubAssetURL(%q) = true, want false", u)
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
	_, err := Check(context.Background(), "maci0/toktop/../../../users/octocat")
	if err == nil {
		t.Fatal("Check accepted a path-injecting repo")
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
	if err := githubRedirect(req("https://user:pass@objects.githubusercontent.com/file"), via); err == nil {
		t.Fatal("userinfo redirect allowed")
	}
	if err := githubRedirect(req("https://evil.example/malware"), via); err == nil {
		t.Fatal("off-site hop allowed")
	}
	if err := githubRedirect(req("http://github.com/x"), via); err == nil {
		t.Fatal("http downgrade allowed")
	}
}

func TestApplyRejectsNonGitHubAssetURL(t *testing.T) {
	rel := &Release{TagName: "v9.9.9"}
	rel.Assets = append(rel.Assets,
		struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		}{Name: AssetName("9.9.9"), URL: "https://evil.example/asset"},
		struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
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
