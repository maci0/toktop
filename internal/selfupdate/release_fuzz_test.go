// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package selfupdate

import (
	"encoding/json"
	"io"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// releaseBodyLimit mirrors the cap Check puts on the GitHub response before
// decoding it, so the harness parses exactly what production parses.
const releaseBodyLimit = 4 << 20

// releaseAssetsJSON builds a release "assets" array from name/URL pairs. The
// size and content_type members are spelled the way a proxy rewrites them, so
// the decode is exercised against a body that is not the one GitHub sends.
func releaseAssetsJSON(pairs ...string) string {
	var b strings.Builder
	b.WriteString(`[`)
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			b.WriteString(",")
		}
		name, _ := json.Marshal(pairs[i])
		link, _ := json.Marshal(pairs[i+1])
		b.WriteString(`{"name":`)
		b.Write(name)
		b.WriteString(`,"browser_download_url":`)
		b.Write(link)
		b.WriteString(`,"size":"big","content_type":null}`)
	}
	b.WriteString(`]`)
	return b.String()
}

// FuzzReleaseJSON drives the GitHub release document: Check decodes a body off
// the network into Release, and applyTo then picks the two assets to fetch out
// of it. A hostile or proxy-mangled body must not panic, and the two URLs the
// picker returns are the only strings that reach the network, so each one that
// survives the trusted-asset check must parse as exactly what TrustedReleaseURL
// admitted: https, no userinfo, a GitHub download host. Without that a
// release naming browser_download_url "https://user:pass@evil.example/..."
// would move the fetch, and the GITHUB_TOKEN header with it, off GitHub.
func FuzzReleaseJSON(f *testing.F) {
	asset := "https://github.com/maci0/toktop/releases/download/v1.2.3/" + AssetName("1.2.3")
	sums := "https://github.com/maci0/toktop/releases/download/v1.2.3/" + checksumsName("1.2.3")

	for _, seed := range []string{
		`{"tag_name":"v1.2.3","assets":[` + releaseAssetsJSON(AssetName("1.2.3"), asset, checksumsName("1.2.3"), sums) + `]}`,
		`{"tag_name":"v1.2.3","assets":[]}`,
		`{"tag_name":"1.2.3","assets":[` + releaseAssetsJSON(AssetName("1.2.3"), asset, checksumsName("1.2.3"), sums) + `]}`,
		`{"tag_name":"","assets":[` + releaseAssetsJSON(AssetName(""), asset) + `]}`,
		`{"tag_name":"v1.2.3","assets":[` + releaseAssetsJSON(AssetName("1.2.3"), "https://user:pass@github.example/x", checksumsName("1.2.3"), "http://github.com/y") + `]}`,
		`{"tag_name":"v1.2.3","assets":[` + releaseAssetsJSON(AssetName("1.2.3"), "", checksumsName("1.2.3"), sums) + `]}`,
		`{"tag_name":"v1.2.3","assets":[` + releaseAssetsJSON(AssetName("1.2.3"), asset, AssetName("1.2.3"), sums, checksumsName("1.2.3"), "https://last.example/z") + `]}`,
		`{"tag_name":"v1.2.3","assets":[` + releaseAssetsJSON(AssetName("1.2.3"), "https://github.com/a/../../../b", checksumsName("1.2.3"), "//github.com/c") + `]}`,
		`{"tag_name":"v1.2.3","assets":null}`,
		`{"tag_name":"v1.2.3","html_url":"https://github.com/x","draft":true,"prerelease":false}`,
		`{"tag_name":null}`,
		`{"tag_name":123}`,
		`{"assets":"many"}`,
		`{}`, `[]`, `null`, `""`, "{", "",
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		rel, ok := decodeRelease(body)
		if !ok {
			return
		}
		again, ok := decodeRelease(body)
		if !ok {
			t.Fatalf("release decode is not deterministic for %q", body)
		}
		if !reflect.DeepEqual(rel, again) {
			t.Fatalf("release decode is not deterministic for %q:\n%+v\n%+v", body, rel, again)
		}
		if rel.TagName == "" {
			// Check refuses a release with no tag, before any asset is named.
			return
		}
		if v := rel.Version(); strings.TrimPrefix(rel.TagName, "v") != v {
			t.Fatalf("Version() = %q for TagName %q", v, rel.TagName)
		}

		assetURL, sumsURL := releaseAssets(rel)
		for _, raw := range []string{assetURL, sumsURL} {
			if raw == "" {
				continue
			}
			if !TrustedReleaseURL(raw) {
				continue // applyTo refuses this before any request is made
			}
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("TrustedReleaseURL admitted %q, which url.Parse rejects: %v", raw, err)
			}
			if u.Scheme != "https" || u.User != nil || u.Host == "" {
				t.Fatalf("TrustedReleaseURL admitted %q: scheme=%q user=%v host=%q", raw, u.Scheme, u.User, u.Host)
			}
			host := strings.ToLower(u.Hostname())
			if host != "github.com" && host != "api.github.com" && !strings.HasSuffix(host, ".githubusercontent.com") {
				t.Fatalf("TrustedReleaseURL admitted off-GitHub host %q in %q", host, raw)
			}
		}
	})
}

// decodeRelease is Check's decode step alone, over the body cap Check applies.
func decodeRelease(body []byte) (*Release, bool) {
	var rel Release
	if err := json.NewDecoder(io.LimitReader(strings.NewReader(string(body)), releaseBodyLimit)).Decode(&rel); err != nil {
		return nil, false
	}
	return &rel, true
}
