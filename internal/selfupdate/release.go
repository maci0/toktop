// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

// Package selfupdate replaces the running binary with a newer release.
//
// Fetch the release asset for this GOOS/GOARCH, verify its SHA-256 against the
// checksums the release ships, and rename it over the current executable.
// Nothing is executed before it is verified, and a failed verification leaves
// the running binary untouched.
//
// On Unix the dashboard notices the replacement on its own: internal/selfreload
// watches the executable and re-execs into whatever is there now, so an update
// applied from another terminal takes effect without anyone quitting. Windows
// cannot exec over a running image, so the dashboard exits and asks for a
// restart instead.
//
// One concern per file: release.go is the release API and everything off the
// network, checksum.go the verification of a fetched asset against the release
// checksums, install.go the filesystem half that puts a verified binary in
// place.
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/maci0/toktop/internal/core"
)

// DefaultRepo is the GitHub repository releases are fetched from.
const DefaultRepo = "maci0/toktop"

// TokenEnv is the optional environment variable authenticating the GitHub
// API calls `toktop update` makes, past the anonymous rate limit.
const TokenEnv = "GITHUB_TOKEN"

// githubToken reads that variable. A trailing newline, which
// `export GITHUB_TOKEN=$(cat token)` leaves behind, is stripped: the same
// rule sshPasswordEnv applies in internal/remote, and for the same reason,
// since a header value carrying one is refused by net/http with an error
// naming the transport rather than the variable. A line break anywhere else
// cannot be sent as a header at all, so the variable is named here.
func githubToken() (string, error) {
	tok := strings.TrimRight(os.Getenv(TokenEnv), "\r\n")
	if strings.ContainsAny(tok, "\r\n") {
		return "", fmt.Errorf("$%s contains a line break; the token file must hold the token alone", TokenEnv)
	}
	return tok, nil
}

// maxAssetBytes bounds a download. A release binary that large is a mistake or
// an attack, and either way should not fill the disk. Var so tests can shrink it.
var maxAssetBytes int64 = 256 << 20

// maxChecksumsArchive bounds the checksums tarball as it is fetched. A release
// ships one small text file inside it, so anything near this is a mistake or an
// attack.
const maxChecksumsArchive = 1 << 20

// Release is the subset of a GitHub release that matters here. The asset
// members are the only ones decoded: size and content_type are left out
// rather than carried unread, so a value spelled as a string or a float by
// some proxy cannot fail the whole decode and take the release down with it.
type Release struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// Version is the release version without a leading "v".
func (r *Release) Version() string { return strings.TrimPrefix(r.TagName, "v") }

// AssetName is the binary this platform needs from a release. It must match
// the names `make release` writes into dist/, or self-update finds nothing.
func AssetName(version string) string {
	name := fmt.Sprintf("toktop_%s_%s_%s", version, runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// checksumsName is the archive the release keeps its checksums.txt in.
func checksumsName(version string) string {
	return fmt.Sprintf("toktop_%s_checksums.tar.gz", version)
}

var client = &http.Client{
	Timeout:       5 * time.Minute,
	CheckRedirect: githubRedirect,
}

// trustedAssetURL reports whether a release asset URL is a GitHub download.
// Tests that serve fixtures from httptest swap this.
var trustedAssetURL = TrustedReleaseURL

// ValidateRepo reports whether repo is a GitHub owner/name, the only shape
// interpolated into the releases API path. Anything else is path traversal,
// a query string, or log injection into the error that names the URL.
func ValidateRepo(repo string) error {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("repo %q must be owner/name", repo)
	}
	if !githubOwner(owner) || !githubRepoName(name) {
		return fmt.Errorf("repo %q is not a GitHub owner/name", repo)
	}
	return nil
}

func githubOwner(s string) bool {
	if len(s) < 1 || len(s) > 39 || !alnum(s[0]) {
		return false
	}
	if len(s) == 1 {
		return true
	}
	for i := 1; i < len(s)-1; i++ {
		if !alnum(s[i]) && s[i] != '-' {
			return false
		}
	}
	return alnum(s[len(s)-1])
}

func githubRepoName(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 100 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if alnum(c) || c == '.' || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func alnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// githubDownloadHost is the allowlist every release download and redirect hop
// is checked against. Folded with core.FoldASCII, the fold this tree uses for
// host labels (see internal/remote/target.go and internal/bearer), rather than
// strings.ToLower: the needles are ASCII literals, and ToLower also folds
// runes whose lowercase is ASCII, so a U+212A (KELVIN SIGN) in a
// browser_download_url would satisfy a comparison the operator's GitHub never
// wrote. The composed spelling is folded too, so a host typed in the NFD form
// a macOS terminal supplies is the host already trusted.
func githubDownloadHost(host string) bool {
	h := core.FoldASCII(norm.NFC.String(host))
	if h == "github.com" || h == "api.github.com" {
		return true
	}
	return strings.HasSuffix(h, ".githubusercontent.com")
}

// httpsPort reports whether a URL's port is the one https is served on. A
// port is compared rather than ignored because Hostname() drops it: without
// this, "https://github.com:8443/..." names an allowed host and the check
// admits it, so the allowlist answers for a service GitHub does not serve on
// the name the operator trusts. An absent port is https's own.
func httpsPort(u *url.URL) bool {
	p := u.Port()
	return p == "" || p == "443"
}

// TrustedReleaseURL reports whether raw is a GitHub URL over https. Every
// release download and redirect hop is held to it, and so is the release page
// `toktop update --check` prints for a shell expansion: that page is never
// fetched, but it is release data landing in the caller's shell.
//
// A URL that survives that second job carries no byte a shell would read as
// anything but itself. url.Parse only refuses C0 and DEL, and the space it
// admits ends the word: a release page spelled
// "https://github.com/o/r/releases/tag/v1 x$(id)" parses, names a trusted
// host, and reaches the operator as two words with a command substitution in
// the second. Every character a release URL is built from is kept, and
// anything else is refused; none of the refused bytes appear in a real
// GitHub URL, so nothing legitimate stops working.
func TrustedReleaseURL(raw string) bool {
	if raw == "" || strings.ContainsFunc(raw, func(r rune) bool {
		return r > 0x7f || !isReleaseURLByte(byte(r))
	}) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return false
	}
	return httpsPort(u) && githubDownloadHost(u.Hostname())
}

// isReleaseURLByte reports whether a byte can appear in a release URL that is
// safe to paste into a shell unquoted: the unreserved set of RFC 3986 plus
// the path, query and fragment punctuation a GitHub URL uses, and the percent
// sign its escapes are built from. Every other byte is refused, which covers
// the space, both quote characters, the backtick, the dollar sign, the
// history-expansion bang and every shell metacharacter. A GitHub release URL
// is none of them, so nothing real is turned away.
func isReleaseURLByte(c byte) bool {
	if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
		return true
	}
	switch c {
	case '-', '.', '_', '~', ':', '/', '?', '#', '[', ']', '@', '%':
		return true
	}
	return false
}

// maxReleaseJSON bounds the release metadata body. It is a few hundred bytes
// of decoded struct; past this the body is a mistake or an attack, and either
// way should not be read into memory to be rejected.
const maxReleaseJSON = 4 << 20

// maxRedirects caps the hop count of a download, so a redirect loop between
// GitHub's own hosts still ends.
const maxRedirects = 10

// githubRedirect refuses hops off GitHub's download hosts, including
// http downgrades and SSRF via a hostile browser_download_url. Replaces
// the client's default policy, so it also caps the hop count. It also strips
// the Authorization header on hops off api.github.com so GITHUB_TOKEN never
// leaks to CDN or storage hosts.
func githubRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if req.URL.Scheme != "https" || req.URL.User != nil || !httpsPort(req.URL) || !githubDownloadHost(req.URL.Hostname()) {
		return fmt.Errorf("refusing redirect to %s", req.URL.Redacted())
	}
	if core.FoldASCII(norm.NFC.String(req.URL.Hostname())) != "api.github.com" && req.Header != nil {
		req.Header.Del("Authorization")
	}
	return nil
}

// Check queries the latest release. It is never called on the startup path:
// a version check must not stand between the user and the dashboard.
func Check(ctx context.Context, repo string) (*Release, error) {
	if repo == "" {
		repo = DefaultRepo
	}
	if err := ValidateRepo(repo); err != nil {
		return nil, err
	}
	owner, name, _ := strings.Cut(repo, "/")
	latest, err := url.JoinPath("https://api.github.com/repos", owner, name, "releases", "latest")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latest, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	tok, err := githubToken()
	if err != nil {
		return nil, err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := client.Do(req)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return nil, fmt.Errorf("%s: %w", latest, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 403 and 429 are how the API reports a spent anonymous quota. The
		// bare status leaves the reader with a GitHub URL and nothing to
		// change, and the fix is the one variable the update help screen
		// already documents for it.
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("github returned %s for %s (anonymous rate limit; set $GITHUB_TOKEN to authenticate)", core.HTTPStatus(resp.Status), latest)
		}
		return nil, fmt.Errorf("github returned %s for %s", core.HTTPStatus(resp.Status), latest)
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseJSON)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("cannot parse github release from %s: %w", latest, err)
	}
	if rel.TagName == "" {
		return nil, errors.New("release has no tag")
	}
	// The tag and the page URL reach the operator's terminal through the
	// status lines and the --check capture, folded here at the wire so every
	// consumer inherits it. GitHub's ref rules keep control characters out of
	// a tag in practice, but the fold is the same one every other string off
	// the network gets in this tree, and release data is the one that reaches
	// a terminal least guarded.
	rel.TagName = core.SingleLine(rel.TagName)
	rel.HTMLURL = core.SingleLine(rel.HTMLURL)
	return &rel, nil
}

// NewerThan reports whether the release is a different version from current.
// Comparison is deliberately exact rather than semver-aware: releases are the
// source of truth, and a "downgrade" published on purpose should be applied.
func (r *Release) NewerThan(current string) bool {
	return r.Version() != "" && r.Version() != strings.TrimPrefix(current, "v")
}

// releaseAssets picks this platform's binary and this version's checksums
// archive out of the decoded release. Both names come from rel.TagName, which
// GitHub chose, so a release tagged with a path or a glob names assets nothing
// will ever match and the install stops before fetching anything.
//
// The last asset of a given name wins, so a release carrying two entries
// named AssetName is decided by the order GitHub listed them in.
func releaseAssets(rel *Release) (assetURL, sumsURL string) {
	want := AssetName(rel.Version())
	sumsFile := checksumsName(rel.Version())
	for _, a := range rel.Assets {
		switch a.Name {
		case want:
			assetURL = a.URL
		case sumsFile:
			sumsURL = a.URL
		}
	}
	return assetURL, sumsURL
}

// fetch streams a release asset into w, refusing one past limit, and returns
// the hex SHA-256 of what it wrote. It asks for identity bytes: the asset is a
// tar.gz whose gzip framing belongs to the file, and a host that also labels
// the body Content-Encoding: gzip would otherwise have the transport
// decompress it, handing ChecksumListing a plain tar it cannot read and
// hashing bytes nobody else hashed.
func fetch(ctx context.Context, url string, w io.Writer, limit int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return "", fmt.Errorf("%s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s returned %s", url, resp.Status)
	}
	h := sha256.New()
	// Read one past the cap, so a truncated body cannot be written or hashed
	// as the whole asset: LimitReader alone would silently clip it.
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", fmt.Errorf("%s: %w", url, err)
	}
	if n > limit {
		return "", fmt.Errorf("%s exceeds %d bytes", url, limit)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
