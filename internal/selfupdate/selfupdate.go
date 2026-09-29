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
package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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

// maxChecksumsDecoded bounds decompressed checksums-archive bytes. The
// compressed fetch is already maxChecksumsArchive; without a decoded cap a gzip
// bomb inside that envelope would expand while the tar walker skipped non-
// matching members.
const maxChecksumsDecoded = 2 << 20

// maxChecksumsListing bounds the checksums.txt member itself, which every
// other member of the archive is skipped past.
const maxChecksumsListing = 1 << 20

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
	return githubDownloadHost(u.Hostname())
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
	if req.URL.Scheme != "https" || req.URL.User != nil || !githubDownloadHost(req.URL.Hostname()) {
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

// Apply downloads, verifies, and installs the release over the running
// executable. It returns the path that was replaced.
//
// The new binary is written next to the current one (same filesystem, so the
// rename is atomic) and only renamed after its checksum matches. A failed
// verification leaves the running binary untouched. If the destination
// already matches the release checksum, the asset is not fetched or replaced.
func Apply(ctx context.Context, rel *Release) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return "", err
	}
	return applyTo(ctx, rel, self)
}

// applyTo is Apply with an explicit target, so the install path can be tested
// without replacing the test binary. The results are named so the staging
// file's cleanup can fold its own failure into the error being returned.
func applyTo(ctx context.Context, rel *Release, self string) (installed string, err error) {
	// Recovery before the network. A killed update can leave the binary under
	// the displaced name with nothing at the installed path, and a download
	// that then fails (offline, rate-limited, checksum mismatch) would leave
	// it that way. Restoring first is a no-op unless the installed path is
	// missing.
	if err := restoreDisplaced(self, self+displacedSuffix); err != nil {
		return "", err
	}
	want := AssetName(rel.Version())
	sumsFile := checksumsName(rel.Version())
	assetURL, sumsURL := releaseAssets(rel)
	if assetURL == "" {
		return "", fmt.Errorf("release %s has no asset %s", rel.TagName, want)
	}
	if sumsURL == "" {
		return "", fmt.Errorf("release %s has no %s; refusing to install unverified binary", rel.TagName, sumsFile)
	}
	if !trustedAssetURL(assetURL) || !trustedAssetURL(sumsURL) {
		return "", fmt.Errorf("release %s asset URL is not a GitHub download", rel.TagName)
	}

	var archive bytes.Buffer
	if _, err := fetch(ctx, sumsURL, &archive, maxChecksumsArchive); err != nil {
		return "", fmt.Errorf("cannot fetch checksums: %w", err)
	}
	sums, err := ChecksumListing(archive.Bytes())
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", sumsFile, err)
	}
	expect, ok := ChecksumFor(sums, want)
	if !ok {
		return "", fmt.Errorf("checksums.txt has no entry for %s", want)
	}
	// A binary toktop cannot read is not an up-to-date binary: reporting the
	// read failure names the permission or path problem instead of spending a
	// download on an install that cannot replace the same file anyway.
	have, cerr := fileChecksum(self)
	if cerr != nil {
		if !errors.Is(cerr, fs.ErrNotExist) {
			return "", fmt.Errorf("cannot checksum %s: %w", self, cerr)
		}
	} else if have == expect {
		// Already this release, so nothing is downloaded or installed. The
		// leftover .old a killed or locked install leaves is still cleared
		// here, because install is the only other place that removes it and
		// this path never reaches install. Only on the platform whose install
		// displaces: the file is written by installDisplacing, which only
		// Windows reaches, so clearing it everywhere else removes a path
		// beside the binary that toktop never created and never owned. Best
		// effort: the .old holds a running image there, so a refusal to
		// delete it is a condition the next install retries, not a failed
		// update.
		if runtime.GOOS == "windows" {
			_ = os.Remove(self + displacedSuffix)
		}
		return self, nil
	}

	dir := filepath.Dir(self)
	core.SweepStaleTemps(dir, updateTempPrefix)
	tmp, err := os.CreateTemp(dir, updateTempPrefix+"*")
	if err != nil {
		return "", fmt.Errorf("cannot write next to %s: %w", self, err)
	}
	tmpName := tmp.Name()
	// A failed update that leaves its partial download behind is reported
	// with the failure, not swallowed: the operator otherwise sees the
	// original error with no sign the install directory now holds an
	// unverified file.
	defer func() {
		tmp.Close()
		err = core.DiscardStaged(tmpName, err)
	}()

	sum, err := fetch(ctx, assetURL, tmp, maxAssetBytes)
	if err != nil {
		return "", err
	}
	if sum != expect {
		return "", fmt.Errorf("checksum mismatch for %s: got %s, want %s", want, sum, expect)
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("cannot flush %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("cannot close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return "", fmt.Errorf("cannot make %s executable: %w", tmpName, err)
	}
	// Two `toktop update` runs are not hypothetical: an operator whose first
	// run reported a network failure re-runs it in a second terminal, and a
	// dashboard's own watch makes the replacement visible before the first
	// run has printed anything. Both reach this point with a verified
	// download of the same release, and the check above was taken before
	// either of them got here, so without the install lock the last rename
	// decides which one wins and an update that ran after an older one can
	// leave the older binary installed. Where the install displaces, the
	// two-rename sequence is worse than a lost race: each run renames the
	// installed binary aside and removes the other's, so a run that dies
	// between its two renames leaves the only copy under the displaced name.
	if err := lockInstall(self, func() error { return installIfChanged(tmpName, self, expect) }); err != nil {
		return "", err
	}
	return self, nil
}

// The install lock: one install of one binary at a time, across processes.
//
// It is a file created exclusively beside the binary and removed on release,
// the same construction and for the same reason as the host-key store's in
// internal/remote (lockStore): exclusive create is atomic on every filesystem
// toktop runs on, which a flock over the binary itself is not on Windows, and
// the binary's directory is guaranteed to exist because the binary is in it.
const (
	// installLockSuffix names the lock file beside the installed binary.
	installLockSuffix = ".lock"
	// installLockPoll is how often a waiting install looks for the lock again.
	installLockPoll = 20 * time.Millisecond
	// installLockStale is how old a lock file has to be before it is assumed
	// to belong to a process that died mid-replace and is broken. The
	// download is outside the lock precisely so that this can stay far above
	// the critical section: a stale threshold near the section's own length
	// would break a live lock on a loaded host. Without the break, one kill
	// would wedge every later update.
	installLockStale = time.Minute
)

// installLockWait bounds how long an install waits for a peer to finish its
// replace. The critical section is a checksum of one file and a rename, so this
// is generous; exceeding it means a peer died holding the lock, which the stale
// check then breaks. Var so tests can shrink it.
var installLockWait = 5 * time.Second

// lockInstall runs fn with an exclusive lock beside the installed binary. A
// lock that cannot be released is reported rather than dropped: the next
// update spends installLockWait on it before breaking it as stale, and an
// operator who never learns why has no way to act. The result is named so the
// deferred release can fold its own failure into whatever fn returned.
func lockInstall(self string, fn func() error) (err error) {
	lock := self + installLockSuffix
	deadline := time.Now().Add(installLockWait)
	for {
		f, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if cerr := f.Close(); cerr != nil {
				lerr := fmt.Errorf("cannot write %s: %w", core.RedactHome(lock), cerr)
				if rerr := os.Remove(lock); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
					lerr = errors.Join(lerr, fmt.Errorf("left a lock file at %s that must be deleted: %w",
						core.RedactHome(lock), rerr))
				}
				return lerr
			}
			defer func() {
				if rerr := os.Remove(lock); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
					err = errors.Join(err, fmt.Errorf("cannot release the lock at %s: %w",
						core.RedactHome(lock), rerr))
				}
			}()
			return fn()
		}
		if !os.IsExist(err) {
			// The directory is unwritable, or the filesystem has no exclusive
			// create. The install itself fails on its own with a clearer
			// error, so run it rather than reporting a lock error the operator
			// cannot act on. This is the same rule lockStore follows.
			return fn()
		}
		// A stale lock is only retried once the break actually took. A lock
		// that cannot be unlinked would otherwise keep the stale arm true and
		// spin here with no deadline check. The reason the break failed rides
		// the give-up message: without it an unremovable lock is reported as
		// one another toktop holds, which is not true and leaves the operator
		// with nothing to act on.
		var breakErr error
		// core.Age, not time.Since: the mtime carries no monotonic reading,
		// so this ages a wall clock, and a backward step (an NTP correction, a
		// resumed laptop) makes the age negative. A negative age is not
		// "older than installLockStale", so the lock from a killed update is
		// never broken and the give-up below names a peer that is not running.
		if info, serr := os.Stat(lock); serr == nil && core.Age(time.Now(), info.ModTime()) > installLockStale {
			if breakErr = os.Remove(lock); breakErr == nil {
				continue
			}
		}
		if time.Now().After(deadline) {
			if breakErr != nil {
				return fmt.Errorf("%s is locked by another toktop; the stale lock at %s could not be removed: %w",
					core.RedactHome(self), core.RedactHome(lock), breakErr)
			}
			return fmt.Errorf("%s is locked by another toktop; giving up after %s",
				core.RedactHome(self), installLockWait)
		}
		time.Sleep(installLockPoll)
	}
}

// installIfChanged puts the staged binary in place unless the installed one
// already carries expect.
//
// The checksum is re-read here, not only before the download, because that is
// what makes a duplicate run a no-op instead of a second replace. Two runs of
// the same release both pass the pre-download check (neither has installed
// anything yet), both download, and both arrive here; whichever runs second
// finds the release already installed and leaves the binary alone, so the two
// runs leave the same state as one. On Windows the second run is what would
// otherwise rename the installed binary aside and delete the first run's,
// leaving the only copy of the binary under the displaced name.
//
// A binary that cannot be read is installed over, not refused: the read
// failure is the condition this path exists to recover from, and install
// already refuses anything that does not match the release checksums.
func installIfChanged(tmpName, self, expect string) error {
	if have, cerr := fileChecksum(self); cerr == nil && have == expect {
		return nil
	}
	if err := install(tmpName, self); err != nil {
		return fmt.Errorf("cannot replace %s: %w", self, err)
	}
	return nil
}

// updateTempPrefix names the staging file a download is written to before it
// is renamed over the running binary.
const updateTempPrefix = ".toktop-update-"

// install puts the verified binary in place.
//
// A rename is atomic, and on Unix it works even while the old binary is
// running. Windows refuses to replace a running image but does allow renaming
// it out of the way first, so that is what happens there; the displaced file
// is removed on the next update, since it is still locked during this one.
// A kill between those two renames leaves the binary displaced, and the next
// run puts it back before installing over it (restoreDisplaced).
//
// The caller's fsync covered the download, not the rename: flushing the
// directory is what makes the new binary survive a crash, and without it an
// update that reported success can be gone on the next boot, leaving the
// previous version and a message saying otherwise.
//
// The caller has already checked tmpName against the release checksums, so
// install trusts its input. It is not transactional: a kill between the two
// Windows renames leaves the old binary displaced until the next run, and
// nothing here reverts a rename that already succeeded.
func install(tmpName, self string) error {
	if runtime.GOOS != "windows" {
		if err := os.Rename(tmpName, self); err != nil {
			return err
		}
		core.SyncDir(filepath.Dir(self))
		return nil
	}
	return installDisplacing(tmpName, self)
}

// displacedSuffix names the copy installDisplacing moves the installed binary
// to before renaming the new one in.
const displacedSuffix = ".old"

// installDisplacing is the two-rename install used where rename cannot replace
// a running image. It is separate from install so the sequence can be tested
// on every platform rather than only on the one that needs it.
//
// A kill between the two renames leaves the binary under the displaced name
// and nothing at the original path, which is worse here than for the host-key
// pin store: a store can be rebuilt by hand, but a host with no binary cannot
// run `toktop update` to replace one, so the next run restores the displaced
// file before it does anything else.
func installDisplacing(tmpName, self string) error {
	displaced := self + displacedSuffix
	// Recovery comes first, and its ordering is the whole point: when a
	// killed update left the binary displaced, that file is the only copy of
	// the installed binary, so removing a leftover first would delete the
	// install this run exists to recover. A leftover from a *completed*
	// update is stale, but then the binary is at the installed path and
	// restoreDisplaced does nothing, leaving the removal below to clear it.
	if err := restoreDisplaced(self, displaced); err != nil {
		return err
	}
	// The previous update's .old is still locked during this one, so a failed
	// removal is expected to be transient. Silently proceeding turns it into
	// a rename error naming the running binary, not the undeletable .old.
	if derr := os.Remove(displaced); derr != nil && !os.IsNotExist(derr) {
		return fmt.Errorf("cannot remove previous update %s: %w", displaced, derr)
	}
	if err := os.Rename(self, displaced); err != nil {
		return fmt.Errorf("cannot displace %s: %w", self, err)
	}
	if err := os.Rename(tmpName, self); err != nil {
		// Put the running binary back rather than leaving nothing installed.
		if rerr := os.Rename(displaced, self); rerr != nil {
			return fmt.Errorf("%w (could not restore original: %w)", err, rerr)
		}
		return err
	}
	core.SyncDir(filepath.Dir(self))
	return nil
}

// restoreDisplaced renames a binary left at the displaced path by a killed
// update back to where it belongs. It is a no-op unless that path is missing
// and the displaced one is not: an update that never started, or one that
// completed, leaves nothing to recover and must not have a stale file put
// under a path that is already correct.
func restoreDisplaced(self, displaced string) error {
	if _, err := os.Stat(self); !os.IsNotExist(err) {
		return nil
	}
	if err := os.Rename(displaced, self); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cannot restore %s from %s: %w", core.RedactHome(self), core.RedactHome(displaced), err)
	}
	core.SyncDir(filepath.Dir(self))
	return nil
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

// fileChecksum is the hex SHA-256 of path, capped the same way a download is
// so a huge existing file cannot fill memory on the "already current" check.
func fileChecksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxAssetBytes+1))
	if err != nil {
		return "", err
	}
	if n > maxAssetBytes {
		return "", fmt.Errorf("file %s exceeds %d bytes", path, maxAssetBytes)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ChecksumListing pulls checksums.txt out of the tar.gz the release ships it
// in. Entries are matched by base name, so a wrapper directory around the
// file does not matter; everything else in the archive is skipped.
func ChecksumListing(archive []byte) (string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(io.LimitReader(gz, maxChecksumsDecoded))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", errors.New("no checksums.txt in the archive")
		}
		if err != nil {
			return "", err
		}
		if filepath.Base(h.Name) != "checksums.txt" {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, maxChecksumsListing))
		if err != nil {
			return "", err
		}
		return string(body), nil
	}
}

// ChecksumFor finds one file's expected hash in a `sha256sum` style listing
// ("<hex>  <name>", with an optional binary-mode asterisk).
func ChecksumFor(listing, name string) (string, bool) {
	for line := range strings.SplitSeq(listing, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		sum, file := fields[0], strings.TrimPrefix(fields[1], "*")
		if filepath.Base(file) != name || len(sum) != 64 {
			continue
		}
		if _, err := hex.DecodeString(sum); err != nil {
			continue
		}
		return strings.ToLower(sum), true
	}
	return "", false
}
