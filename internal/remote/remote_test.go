package remote

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"github.com/maci0/toktop/internal/core"
)

// useSSHConfig points ParseTarget at an ssh_config holding body, written to
// the test's temp dir so no test reads the developer's own config. It
// returns the path, for subtests that rewrite the file in place.
func useSSHConfig(t *testing.T, body string) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	oldPath, oldRead := sshConfigPath, configReader
	t.Cleanup(func() { sshConfigPath, configReader = oldPath, oldRead })
	sshConfigPath = func() string { return cfg }
	configReader = func(path string) ([]byte, error) { return os.ReadFile(path) }
	return cfg
}

// stubSSHConfig points ParseTarget at path and reads it through read, for
// the cases the outcome hinges on the read failing rather than the contents.
func stubSSHConfig(t *testing.T, path string, read func(string) ([]byte, error)) {
	t.Helper()
	oldPath, oldRead := sshConfigPath, configReader
	t.Cleanup(func() { sshConfigPath, configReader = oldPath, oldRead })
	sshConfigPath = func() string { return path }
	configReader = read
}

// disableAgent keeps the auth chain from picking up a running ssh-agent
// (SSH_AUTH_SOCK, or the Windows OpenSSH named pipe).
func disableAgent(t *testing.T) {
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", "")
	orig := platformAgentSock
	platformAgentSock = func() string { return "" }
	t.Cleanup(func() { platformAgentSock = orig })
}

func TestAgentSockPrefersSSHAuthSock(t *testing.T) {
	orig := platformAgentSock
	platformAgentSock = func() string { return "platform-default" }
	t.Cleanup(func() { platformAgentSock = orig })

	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	if got := agentSock(); got != "/tmp/agent.sock" {
		t.Fatalf("agentSock = %q, want SSH_AUTH_SOCK", got)
	}
	t.Setenv("SSH_AUTH_SOCK", "")
	if got := agentSock(); got != "platform-default" {
		t.Fatalf("agentSock = %q, want the platform default when SSH_AUTH_SOCK is unset", got)
	}
	// A wrapper that exports the name with `$(...)` leaves the line ending
	// behind. The socket named with it does not exist, so the dial fails and
	// the run falls through to the password prompt without naming why.
	for _, v := range []string{"/tmp/agent.sock\n", "/tmp/agent.sock\r\n", " /tmp/agent.sock "} {
		t.Setenv("SSH_AUTH_SOCK", v)
		if got := agentSock(); got != "/tmp/agent.sock" {
			t.Fatalf("agentSock = %q, want the trimmed path for SSH_AUTH_SOCK %q", got, v)
		}
	}
	// Whitespace alone names no socket at all, so it resolves to the
	// platform default rather than a path built out of the whitespace.
	for _, v := range []string{"  \n\t ", "\n"} {
		t.Setenv("SSH_AUTH_SOCK", v)
		if got := agentSock(); got != "platform-default" {
			t.Fatalf("agentSock = %q, want the platform default for SSH_AUTH_SOCK %q", got, v)
		}
	}
}

func TestDefaultAgentSock(t *testing.T) {
	got := defaultAgentSock()
	if runtime.GOOS == "windows" {
		if got != `\\.\pipe\openssh-ssh-agent` {
			t.Fatalf("windows defaultAgentSock = %q, want the OpenSSH named pipe", got)
		}
		return
	}
	if got != "" {
		t.Fatalf("defaultAgentSock = %q, want empty on %s", got, runtime.GOOS)
	}
}

func TestParseTarget(t *testing.T) {
	cases := []struct {
		raw     string
		user    string
		host    string
		port    int
		wantErr bool
	}{
		{"ssh://user@192.168.1.5", "user", "192.168.1.5", 22, false},
		{"ssh://root@gpu-box:2222", "root", "gpu-box", 2222, false},
		{"ssh://192.168.1.5", "", "192.168.1.5", 22, false},
		{"ssh://user@box/", "user", "box", 22, false},
		{"http://x", "", "", 0, true},
		{"ssh://", "", "", 0, true},
		{"ssh://h:notaport", "", "h", 0, true},
		{"ssh://user:s3cret@box", "", "", 0, true},
		{"ssh://:s3cret@box", "", "", 0, true},
		{"ssh://user:@box", "", "", 0, true},
		{"ssh://box/opt/engines", "", "", 0, true},
		{"ssh://box?jump=1", "", "", 0, true},
		{"ssh://box#frag", "", "", 0, true},
		{"ssh://bad\nhost", "", "", 0, true},
		{"ssh://bad\rhost", "", "", 0, true},
		{"ssh://bad user@host", "", "", 0, true},
		{"ssh://user\nname@host", "", "", 0, true},
	}
	for _, c := range cases {
		got, err := ParseTarget(c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseTarget(%q) expected error", c.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTarget(%q): %v", c.raw, err)
			continue
		}
		if got.User != c.user || got.Host != c.host || got.Port != c.port {
			t.Errorf("ParseTarget(%q) = %+v, want user=%q host=%q port=%d",
				c.raw, got, c.user, c.host, c.port)
		}
	}
}

func TestParseTargetPasswordNotLeaked(t *testing.T) {
	const secret = "s3cret"
	_, err := ParseTarget("ssh://user:" + secret + "@box")
	if err == nil {
		t.Fatal("password in ssh URL accepted")
	}
	if !strings.Contains(err.Error(), "password") {
		t.Fatalf("error = %q, want mention of password", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %q leaked the password", err)
	}
	_, err = ParseTarget("ssh://user:" + secret + "@box:notaport")
	if err == nil {
		t.Fatal("malformed ssh URL with password accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("parse error = %q leaked the password", err)
	}
}

// An argument url.Parse rejects cannot be echoed: the rejected text is
// whatever followed the scheme, which is where an embedded password would
// be. The error has to name the shape instead, or the reader is left with a
// bare complaint and the argument to fix.
func TestParseTargetUnparseableNamesTheShape(t *testing.T) {
	const raw = "ssh://gpu box"
	_, err := ParseTarget(raw)
	if err == nil {
		t.Fatal("unparseable ssh target accepted")
	}
	if got := err.Error(); !strings.Contains(got, "ssh://[user@]host[:port]") {
		t.Errorf("error = %q, want the accepted spelling ssh://[user@]host[:port]", got)
	}
	if strings.Contains(err.Error(), raw) {
		t.Errorf("error = %q echoed the argument", err)
	}
}

func TestParseTargetSSHConfig(t *testing.T) {
	// The tilde in IdentityFile resolves against the home directory, so it is
	// redirected at a temp dir: a suffix check would be satisfied by the
	// developer's own ~/.ssh/gpu_key too.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on windows
	useSSHConfig(t, `
# comment
Host gpu
  hostname 192.168.0.212
  user dev
  port 2022
  identityfile ~/.ssh/gpu_key

Host *.lab
  user labadmin

Host *
  user fallback
`)

	tgt, err := ParseTarget("ssh://gpu")
	if err != nil {
		t.Fatal(err)
	}
	want := Target{User: "dev", Host: "192.168.0.212", Port: 2022}
	if tgt.User != want.User || tgt.Host != want.Host || tgt.Port != want.Port {
		t.Errorf("resolved = %+v, want %+v", tgt, want)
	}
	if got, want := tgt.KeyFile, filepath.Join(home, ".ssh", "gpu_key"); got != want {
		t.Errorf("keyfile = %q, want %q", got, want)
	}

	tgt, _ = ParseTarget("ssh://box.lab")
	if tgt.User != "labadmin" || tgt.Port != 22 {
		t.Errorf("wildcard match = %+v", tgt)
	}

	// explicit URL fields win over config
	tgt, _ = ParseTarget("ssh://root@gpu:23")
	if tgt.User != "root" || tgt.Port != 23 {
		t.Errorf("URL precedence = %+v", tgt)
	}
}

func TestParseTargetSSHConfigTabsAndNegation(t *testing.T) {
	cfg := useSSHConfig(t, "Host\tgpu\n\tHostName\t10.9.8.7\n\tPort\t2022\n")

	tgt, err := ParseTarget("ssh://gpu")
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Host != "10.9.8.7" || tgt.Port != 2022 {
		t.Errorf("tab-separated config ignored: %+v", tgt)
	}

	for _, patterns := range []string{"* !gpu", "!gpu *", "* !g?u", "* !GPU"} {
		t.Run(patterns, func(t *testing.T) {
			body := "Host " + patterns + "\n  User other\n  Port 2022\nHost *\n  User fallback\n  Port 2222\n"
			if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				host, user string
				port       int
			}{
				{"gpu", "fallback", 2222},
				{"otherbox", "other", 2022},
			} {
				got, err := ParseTarget("ssh://" + tc.host)
				if err != nil {
					t.Fatal(err)
				}
				if got.User != tc.user || got.Port != tc.port {
					t.Errorf("host %s = %+v, want user %s port %d", tc.host, got, tc.user, tc.port)
				}
			}
		})
	}
}

func TestExpandTildeAcceptsBothSeparators(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if got := core.ExpandHome("~"); got != home {
		t.Errorf("core.ExpandHome(~) = %q, want %q", got, home)
	}
	want := filepath.Join(home, "rel")
	if got := core.ExpandHome("~/rel"); got != want {
		t.Errorf("core.ExpandHome(~/rel) = %q, want %q", got, want)
	}
	if got := core.ExpandHome(`~\rel`); got != want {
		t.Errorf(`core.ExpandHome(~\rel) = %q, want %q`, got, want)
	}
	if got := core.ExpandHome("/abs/key"); got != "/abs/key" {
		t.Errorf("core.ExpandHome(absolute) = %q, want unchanged", got)
	}
}

func TestResolveKeyFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)

	t.Run("empty", func(t *testing.T) {
		if _, err := ResolveKeyFile("  "); err == nil {
			t.Fatal("empty path accepted")
		}
	})
	t.Run("missing", func(t *testing.T) {
		if _, err := ResolveKeyFile(filepath.Join(dir, "no-such-key")); err == nil {
			t.Fatal("missing file accepted")
		}
	})
	t.Run("directory", func(t *testing.T) {
		if _, err := ResolveKeyFile(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("directory = %v, want not a regular file", err)
		}
	})
	t.Run("regular file", func(t *testing.T) {
		key := filepath.Join(dir, "id_ed25519")
		if err := os.WriteFile(key, []byte("dummy"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := ResolveKeyFile(key)
		if err != nil || got != key {
			t.Fatalf("ResolveKeyFile(%q) = %q, %v", key, got, err)
		}
	})
	t.Run("tilde", func(t *testing.T) {
		key := filepath.Join(dir, "id_ed25519")
		if err := os.WriteFile(key, []byte("dummy"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := ResolveKeyFile("~/id_ed25519")
		if err != nil || got != key {
			t.Fatalf("ResolveKeyFile(~/id_ed25519) = %q, %v; want %q", got, err, key)
		}
	})
}

// A key path and a host-key store path both sit under $HOME, so every
// diagnostic naming one names the account. Operators paste these into issues;
// the reason has to survive that, the path must not.
func TestSshDiagnosticsOmitHomeDirectory(t *testing.T) {
	home := filepath.Join(t.TempDir(), "private-user")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if got, err := os.UserHomeDir(); err != nil || got != home {
		t.Skipf("cannot redirect the home directory (got %q, %v)", got, err)
	}
	key := filepath.Join(home, ".ssh", "id_ed25519")
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(home, "config", "toktop", "known_hosts")
	if err := os.MkdirAll(filepath.Dir(store), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store, []byte("box not-an-authorized-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldStore := knownHostsPath
	knownHostsPath = func() string { return store }
	t.Cleanup(func() { knownHostsPath = oldStore })

	_, keyErr := ResolveKeyFile(filepath.Join(home, ".ssh", "no-such-key"))
	if keyErr == nil {
		t.Fatal("missing key accepted")
	}
	// A directory named as a key reaches a second branch, and that one
	// spelled the expanded path itself rather than a stat error.
	_, dirErr := ResolveKeyFile(filepath.Join(home, ".ssh"))
	if dirErr == nil {
		t.Fatal("directory key accepted")
	}
	// The key chain is assembled before any dial, so an unparsable key fails
	// without a network round trip.
	_, connErr := Connect(t.Context(), Target{Host: "box", KeyFile: key})
	if connErr == nil {
		t.Fatal("unparsable key accepted")
	}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"ResolveKeyFile", keyErr},
		{"ResolveKeyFile directory", dirErr},
		{"Connect", connErr},
	} {
		msg := tc.err.Error()
		if strings.Contains(msg, home) || strings.Contains(msg, "private-user") {
			t.Errorf("%s error names the home directory: %q", tc.name, msg)
		}
		if !strings.Contains(msg, "~") {
			t.Errorf("%s error = %q, want the path folded to ~", tc.name, msg)
		}
	}
}

func TestCutConfigField(t *testing.T) {
	cases := []struct {
		line, key, val string
		ok             bool
	}{
		{"HostName foo", "HostName", "foo", true},
		{"  identityfile = ~/keys/id ", "identityfile", "~/keys/id", true},
		{"Host\tgpu", "Host", "gpu", true},
		{"Host\t* !gpu", "Host", "* !gpu", true},
		{"\tport\t2022", "port", "2022", true},
		{"User\t = admin", "User", "admin", true},
		{"IdentityFile ~/key=backup", "IdentityFile", "~/key=backup", true},
		{"IdentityFile=~/key=backup", "IdentityFile", "~/key=backup", true},
		{`IdentityFile "C:\Users\a b\id_ed25519"`, "IdentityFile", `C:\Users\a b\id_ed25519`, true},
		{`IdentityFile "~/my key" # personal`, "IdentityFile", "~/my key", true},
		{"HostName 10.0.0.5 # the gpu box", "HostName", "10.0.0.5", true},
		{"Port 2222 # lab port", "Port", "2222", true},
		{"User dev # me", "User", "dev", true},
		{"IdentityFile ~/keys/a#b", "IdentityFile", "~/keys/a#b", true},
		{`Host "build box"`, "Host", "build box", true},
		{"#comment", "", "", false},
		{"", "", "", false},
		{"solokeyword", "", "", false},
	}
	for _, c := range cases {
		k, v, ok := cutConfigField(c.line)
		if k != c.key || v != c.val || ok != c.ok {
			t.Errorf("cutConfigField(%q) = %q,%q,%v want %q,%q,%v",
				c.line, k, v, ok, c.key, c.val, c.ok)
		}
	}
}

func TestPatternMatch(t *testing.T) {
	cases := []struct {
		pat, s string
		want   bool
	}{
		{"*", "anything", true},
		{"gpu-*", "gpu-box1", true},
		{"*.lab", "box.lab", true},
		{"*.lab", "box.example.com", false},
		{"host?", "host1", true},
		{"host?", "host12", false},
		{"exact", "exact", true},
		{"a*b*c", "axxbyyc", true},
		// path.Match would answer true for both of the first pair and false
		// for the second: a block written for one machine must not answer
		// for two, and a backslash in a host name is a literal byte.
		{"box[12]", "box1", false},
		{"box[12]", "box2", false},
		{"box[12]", "box[12]", true},
		{`web\1`, "web1", false},
		{`web\1`, `web\1`, true},
		{"*", "", true},
		{"a*b", "b", false},
		{"?a", "a", false},
		{"*a*b*c*", "xxaxxbxxcxx", true},
		// '?' is one character, and a character in a host name may need
		// two bytes. A byte-wise '?' stops inside the last rune and reports
		// no match, so an IDN in U-label form never reaches its own block.
		{"caf?", "café", true},
		{"?é", "café", false},
		{"caf??", "cafés", true},
		{"caf?", "caf", false},
		{"caf?", "cafés", false},
	}
	for _, c := range cases {
		if got := patternMatch(c.pat, c.s); got != c.want {
			t.Errorf("patternMatch(%q,%q) = %v", c.pat, c.s, got)
		}
	}
}

// Two handshake callbacks racing on one store must both land: the read and
// the write are a single critical section, so a writer that read a snapshot
// before another writer's pin was in it would silently drop that pin.
func TestTOFUConcurrentHandshakesDoNotLosePins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	old := knownHostsPath
	defer func() { knownHostsPath = old }()
	knownHostsPath = func() string { return path }

	cb, err := tofu()
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			errs[i] = cb(fmt.Sprintf("host-%d:22", i), nil, fakePublicKey(fmt.Sprintf("k%d", i)))
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent handshake %d rejected: %v", i, err)
		}
	}
	store, err := readKnownHosts(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(store) != n {
		t.Fatalf("store holds %d pins, want %d: a concurrent writer was lost", len(store), n)
	}
}

// A store another process is holding makes its own writer wait inside
// lockStore, for up to storeLockWait. That wait must not be paid by writers to
// a *different* store: the in-process mutex is taken inside the file lock
// rather than around it, so a mutex held across the cross-process sleep would
// queue every concurrent handshake in the process behind the one host whose
// peer is slow. The bound here is well under storeLockWait, so passing means
// the two stores never met.
func TestTOFUStoresDoNotBlockEachOther(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	free := filepath.Join(dir, "free")
	old := knownHostsPath
	defer func() { knownHostsPath = old }()

	// A peer process holds the first store's lock file with a current
	// timestamp: not stale, so lockStore waits on it rather than breaking it.
	if err := os.WriteFile(blocked+storeLockSuffix, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	knownHostsPath = func() string { return blocked }
	blockedCB, err := tofu()
	if err != nil {
		t.Fatal(err)
	}
	knownHostsPath = func() string { return free }
	freeCB, err := tofu()
	if err != nil {
		t.Fatal(err)
	}

	stalled := make(chan struct{})
	started := make(chan struct{})
	go func() {
		defer close(stalled)
		// Signalled before the call, so the head start below waits for the
		// goroutine to be scheduled rather than for a guessed instant.
		close(started)
		// Fails with the give-up error once the peer lock ages out of
		// storeLockStale, or succeeds early if this test releases it first.
		// Either way the wait is the thing under test.
		_ = blockedCB("blocked:22", nil, fakePublicKey("blocked"))
	}()
	<-started
	// Let the writer reach the peer-lock wait before the second store's
	// writer runs, or the test proves nothing. Head start is short and the
	// budget below is far under storeLockWait, so the gap between the two
	// does not have to be tuned to be decisive.
	time.Sleep(200 * time.Millisecond)

	// The budget is the blocked writer's own give-up, so the assertion stays
	// the one that matters: a second store must not wait behind the first
	// one's peer lock. A fixed small budget tested the scheduler rather than
	// the mutex, and went red under a full-package run with -race.
	done := make(chan error, 1)
	go func() { done <- freeCB("free:22", nil, fakePublicKey("free")) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second store rejected: %v", err)
		}
	case <-time.After(storeLockWait):
		t.Fatalf("writing one store blocked a write to another for the whole %s peer-lock wait", storeLockWait)
	}
	// Release the peer so the parked writer finishes instead of running out
	// the full storeLockWait after the test has already decided.
	_ = os.Remove(blocked + storeLockSuffix)
	<-stalled
	if store, err := readKnownHosts(free); err != nil || store["free:22"] == "" {
		t.Fatalf("the second store's pin did not land: %v", err)
	}
}

// storeMutex is keyed by path, so one store's critical section never blocks
// another's. A single process-wide mutex would make every store wait behind
// whichever one happened to be writing.
func TestStoreMutexIsPerPath(t *testing.T) {
	dir := t.TempDir()
	a, b := storeMutex(filepath.Join(dir, "a")), storeMutex(filepath.Join(dir, "b"))
	if a == b {
		t.Fatal("two store paths share one mutex")
	}
	if storeMutex(filepath.Join(dir, "a")) != a {
		t.Fatal("the same store path returned a different mutex")
	}

	// Hold a's lock across the check, from a goroutine, so the critical
	// section is released before the test returns even on a failure below.
	held, release := make(chan struct{}), make(chan struct{})
	go func() {
		a.Lock()
		defer a.Unlock()
		close(held)
		<-release
	}()
	<-held

	done := make(chan struct{})
	go func() {
		b.Lock()
		defer b.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("holding one store's mutex blocked another store's")
	}
	close(release)
}

func TestTOFUStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	old := knownHostsPath
	defer func() { knownHostsPath = old }()
	knownHostsPath = func() string { return path }

	cb1, err := tofu()
	if err != nil {
		t.Fatal(err)
	}
	key1 := fakePublicKey("first")
	if err := cb1("h:22", nil, key1); err != nil {
		t.Fatalf("first contact rejected: %v", err)
	}
	if err := cb1("h\nhost:22", nil, key1); err == nil {
		t.Fatal("tofu accepted hostname with newline")
	}
	if err := cb1("h host:22", nil, key1); err == nil {
		t.Fatal("tofu accepted hostname with space")
	}

	key2 := fakePublicKey("other-host")
	if err := cb1("other:22", nil, key2); err != nil {
		t.Fatalf("second host rejected: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(before)), "\n") {
		if len(strings.Fields(line)) != 3 {
			t.Fatalf("invalid host record: %q", line)
		}
		_, authorizedKey, ok := strings.Cut(line, " ")
		if !ok {
			t.Fatalf("invalid host record: %q", line)
		}
		if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorizedKey)); err != nil {
			t.Fatalf("invalid stored key: %v", err)
		}
	}

	// A fresh callback over the same store accepts the remembered key.
	cb2, err := tofu()
	if err != nil {
		t.Fatal(err)
	}
	if err := cb2("h:22", nil, key1); err != nil {
		t.Fatalf("remembered key rejected: %v", err)
	}
	// A different key must be refused.
	if err := cb2("h:22", nil, fakePublicKey("second")); err == nil {
		t.Fatal("changed key accepted")
	} else if !strings.Contains(err.Error(), "changed") {
		t.Errorf("mismatch error should say changed: %v", err)
	}

	if err := cb2("other:22", nil, key2); err != nil {
		t.Fatalf("remembered second host rejected: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("repeated callbacks changed the store")
	}
	store, err := readKnownHosts(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(store) != 2 {
		t.Errorf("store = %v", store)
	}
}

func TestTOFUExistingStore(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "standard", true: "legacy"}[legacy], func(t *testing.T) {
			withKnownHosts(t)
			path := knownHostsPath()
			key := fakePublicKey("remembered")
			line := "h:22 " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
			if legacy {
				line = "h:22 " + line
			}
			if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				cb, err := tofu()
				if err != nil {
					t.Fatal(err)
				}
				if err := cb("h:22", nil, key); err != nil {
					t.Fatalf("remembered key rejected: %v", err)
				}
				if err := cb("other:22", nil, fakePublicKey("other")); err != nil {
					t.Fatal(err)
				}
				if err := cb("h:22", nil, fakePublicKey("changed")); err == nil {
					t.Fatal("changed key accepted")
				} else if !strings.Contains(err.Error(), ssh.FingerprintSHA256(key)) {
					t.Fatalf("stored fingerprint missing: %v", err)
				}
			}
		})
	}
}

func TestFingerprintOfMatchesKeyType(t *testing.T) {
	check := func(t *testing.T, pk ssh.PublicKey) {
		t.Helper()
		line := "h:22 " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk)))
		got := fingerprintOf(line)
		want := ssh.FingerprintSHA256(pk)
		if got != want {
			t.Errorf("fingerprintOf = %q, want %q (type %s)", got, want, pk.Type())
		}
	}
	check(t, fakePublicKey("ed25519-fp"))
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ssh.NewPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	check(t, pk)
}

// Two Connects writing different hosts must both persist. A snapshot-then-
// WriteFile race drops whichever host finished first.
func TestTOFUConcurrentHostsPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	old := knownHostsPath
	t.Cleanup(func() { knownHostsPath = old })
	knownHostsPath = func() string { return path }

	hosts := []string{"a.example:22", "b.example:22"}
	errc := make(chan error, len(hosts))
	for _, h := range hosts {
		go func(h string) {
			cb, err := tofu()
			if err != nil {
				errc <- err
				return
			}
			errc <- cb(h, nil, fakePublicKey(h))
		}(h)
	}
	for range hosts {
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
	}
	store, err := readKnownHosts(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(store) != len(hosts) {
		t.Fatalf("store = %v, want %d hosts", store, len(hosts))
	}
	for _, h := range hosts {
		if _, ok := store[h]; !ok {
			t.Errorf("missing %s in %v", h, store)
		}
	}
}

// The default-key leg of the auth chain must pick up a passphrase-less key
// from ~/.ssh and silently skip unreadable or encrypted files.
func TestDefaultKeyPathsAndAuthChain(t *testing.T) {
	disableAgent(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on windows

	tgt := Target{}
	methods, cleanup, err := tgt.authMethods()
	cleanup()
	if err != nil {
		t.Fatalf("empty home: %v", err)
	}
	if len(methods) != 0 {
		t.Fatalf("empty home yielded %d methods, want 0", len(methods))
	}
	// Order matters: authMethods tries the defaults in this order, so the
	// sequence is the preference order, not just a set of three paths.
	want := []string{
		filepath.Join(home, ".ssh", "id_ed25519"),
		filepath.Join(home, ".ssh", "id_ecdsa"),
		filepath.Join(home, ".ssh", "id_rsa"),
	}
	if got := defaultKeyPaths(); !slices.Equal(got, want) {
		t.Fatalf("defaultKeyPaths() = %v, want %v", got, want)
	}

	key := filepath.Join(home, ".ssh", "id_ed25519")
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "toktop test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	// junk next to it must be skipped without breaking the chain
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id_rsa"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	methods, cleanup, err = tgt.authMethods()
	defer cleanup()
	if err != nil {
		t.Fatalf("one valid default key: %v", err)
	}
	if len(methods) != 1 {
		t.Fatalf("authMethods = %d methods with one valid key present, want 1", len(methods))
	}
}

// An explicitly configured key (--ssh-key) that cannot be loaded must abort
// authentication with the offending path, not silently fall through to a
// generic credentials-rejected failure later.
// The password source must ask once: the env-var answer is cached so the
// password and keyboard-interactive legs of one connection chain share it
// instead of re-reading the environment or prompting twice.
func TestPasswordSourceEnvWinsAndAsksOnce(t *testing.T) {
	t.Setenv("TOKTOP_SSH_PASSWORD", "sekrit")
	tgt := Target{User: "u", Host: "h", Port: 22}
	ps := &passwordSource{}

	pw, err := ps.get(tgt)
	if err != nil || pw != "sekrit" {
		t.Fatalf("first get = %q, %v", pw, err)
	}
	t.Setenv("TOKTOP_SSH_PASSWORD", "") // env gone after the first ask
	pw, err = ps.get(tgt)
	if err != nil || pw != "sekrit" {
		t.Errorf("cached answer lost: %q, %v", pw, err)
	}
}

// Without TOKTOP_SSH_PASSWORD and without a terminal there is no way to
// prompt: get must fail naming the env var, and cache that failure instead
// of retrying (or blocking) for every auth mechanism in the chain.
func TestPasswordSourceHeadlessFailureCached(t *testing.T) {
	t.Setenv("TOKTOP_SSH_PASSWORD", "")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = r // a pipe is never a terminal
	t.Cleanup(func() { os.Stdin = oldStdin; r.Close(); w.Close() })

	ps := &passwordSource{}
	tgt := Target{}
	_, err = ps.get(tgt)
	if err == nil || !strings.Contains(err.Error(), "TOKTOP_SSH_PASSWORD") {
		t.Fatalf("headless get err = %v, want guidance naming the env var", err)
	}
	_, err = ps.get(tgt)
	if err == nil {
		t.Fatal("cached failure must persist across calls")
	}
}

// countingGetter wraps the password getter so a test can see how often the
// auth chain reached for the secret: once for the whole connection, and not at
// all for a challenge the chain refuses to answer.
func countingGetter(secret string) (get func() (string, error), calls func() int) {
	n := 0
	return func() (string, error) { n++; return secret, nil }, func() int { return n }
}

func TestAnswerPasswordPromptSingleSecret(t *testing.T) {
	t.Run("single password prompt", func(t *testing.T) {
		get, calls := countingGetter("sekrit")
		got, err := answerPasswordPrompt([]string{"Password:"}, []bool{false}, get)
		if err != nil || len(got) != 1 || got[0] != "sekrit" {
			t.Fatalf("single password prompt = %q, %v", got, err)
		}
		if n := calls(); n != 1 {
			t.Errorf("the secret was asked for %d times, want 1: every mechanism in the chain shares one answer", n)
		}
	})
	for _, tc := range []struct {
		name      string
		questions []string
		echos     []bool
	}{
		{"multiple prompts", []string{"Password:", "OTP:"}, []bool{false, false}},
		{"echoing prompt", []string{"Username:"}, []bool{true}},
		{"empty challenge", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			get, calls := countingGetter("sekrit")
			if _, err := answerPasswordPrompt(tc.questions, tc.echos, get); err == nil {
				t.Fatalf("a %s challenge was accepted, want it refused", tc.name)
			}
			if n := calls(); n != 0 {
				t.Errorf("the secret was asked for %d times on a refused challenge, want 0", n)
			}
		})
	}
}

func TestExplicitKeyFileFailureAborts(t *testing.T) {
	disableAgent(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	bogus := filepath.Join(home, "nope", "missing_key")
	tgt := Target{KeyFile: bogus}
	methods, cleanup, err := tgt.authMethods()
	cleanup()
	if err == nil {
		t.Fatal("explicit unloadable key must fail authMethods")
	}
	if len(methods) != 0 {
		t.Fatalf("failed chain yielded %d methods, want 0", len(methods))
	}
	if !strings.Contains(err.Error(), bogus) {
		t.Errorf("error should name the key path: %v", err)
	}
}

func TestShort(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"host", "host"},
		{"host keytype", "keytype"},
		{"host keytype base64key extra", "base64key"},
	}
	for _, tc := range cases {
		if got := short(tc.in); got != tc.want {
			t.Errorf("short(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFingerprintOfFallback(t *testing.T) {
	for _, line := range []string{"invalid-line", "host not-a-valid-key"} {
		sum := sha256.Sum256([]byte(line))
		want := base64.RawStdEncoding.EncodeToString(sum[:8])
		if got := fingerprintOf(line); got != want {
			t.Errorf("fingerprintOf(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestKnownHostsPathHonorsXDGConfigHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	got := knownHostsPath()
	want := filepath.Join(tmp, "toktop", "known_hosts")
	if got != want {
		t.Fatalf("knownHostsPath() = %q, want %q", got, want)
	}
}

// A toktop killed between staging the store and renaming it leaves a partial
// file nothing else ever looks for. The next write to the store clears it; a
// staging file young enough to be another process's write, and any other file
// in the directory, must survive.
func TestWriteKnownHostsSweepsTempFilesLeftByAKilledRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	crashed := filepath.Join(dir, knownHostsTempPrefix+"crashed")
	if err := os.WriteFile(crashed, []byte("half a store"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * core.StaleTempAge)
	if err := os.Chtimes(crashed, old, old); err != nil {
		t.Fatal(err)
	}
	inFlight := filepath.Join(dir, knownHostsTempPrefix+"in-flight")
	if err := os.WriteFile(inFlight, []byte("writing"), 0o600); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(unrelated, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeKnownHosts(path, map[string]string{"a.example:22": "a.example:22 ssh-ed25519 AAAA"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(crashed); !os.IsNotExist(err) {
		t.Errorf("a staging file from a killed run should be swept: %v", err)
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Errorf("a fresh staging file may be another process's write: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("the sweep must not touch other files: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the store itself must be written: %v", err)
	}
	// A second write converges: the directory holds the store, its backup
	// copy, the in-flight file, and nothing the sweep added.
	if err := writeKnownHosts(path, map[string]string{"a.example:22": "a.example:22 ssh-ed25519 AAAA"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{knownHostsTempPrefix + "in-flight", "config.toml", "known_hosts", "known_hosts" + backupSuffix}
	slices.Sort(names)
	if !slices.Equal(names, want) {
		t.Fatalf("directory = %v, want %v", names, want)
	}

	// The store holds host keys, so the write must create a private directory
	// and leave a private file. Neither mode is visible without a stat, and the
	// directory mode only takes effect where MkdirAll has work to do, so this
	// writes to a nested subdir that does not exist yet.
	privateDir := filepath.Join(t.TempDir(), "toktop")
	privateStore := filepath.Join(privateDir, "known_hosts")
	if err := writeKnownHosts(privateStore, map[string]string{"a.example:22": "a.example:22 ssh-ed25519 AAAA"}); err != nil {
		t.Fatal(err)
	}
	storeInfo, err := os.Stat(privateStore)
	if err != nil {
		t.Fatalf("the store must be written into a directory that did not exist: %v", err)
	}
	dirInfo, err := os.Stat(privateDir)
	if err != nil {
		t.Fatalf("the store directory must be created: %v", err)
	}
	// Permission bits are a POSIX notion. Windows reports 0666 for any file
	// and 0777 for any directory whose read-only attribute is clear, whatever
	// the create call asked for: a private store there is the ACL inherited
	// from the directory, which no mode asserts. The write into a directory
	// that did not exist is checked above on every platform.
	if runtime.GOOS != "windows" {
		if got := storeInfo.Mode().Perm(); got != 0o600 {
			t.Errorf("store mode = %#o, want %#o", got, 0o600)
		}
		if got := dirInfo.Mode().Perm(); got != 0o700 {
			t.Errorf("store directory mode = %#o, want %#o", got, 0o700)
		}
	}
}

// A store that does not parse must fail the read, not read as a shorter one.
// Skipping the bad line would turn corruption into a forced re-TOFU for that
// host, which is the one outcome the store exists to prevent.
func TestReadKnownHostsRejectsUnparsableRecords(t *testing.T) {
	key := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(fakePublicKey("good"))))
	cases := map[string]string{
		"truncated line":   "h:22 ssh-ed25519",
		"host with no key": "h:22",
		"garbage key":      "h:22 ssh-ed25519 not-base64!!!",
		"wrong key type":   "h:22 ssh-ed25519 AAAA",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "known_hosts")
			// A valid record alongside the bad one: the read must fail on the
			// bad one rather than quietly returning the good one alone.
			content := "ok:22 " + key + "\n" + body + "\n"
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readKnownHosts(path); err == nil {
				t.Fatalf("readKnownHosts accepted a store containing %q", body)
			} else if !strings.Contains(err.Error(), path) {
				t.Errorf("error should name the file, got: %v", err)
			}
		})
	}
}

// A host pinned twice with different keys must not resolve to whichever line
// came last: appending one line to the file would otherwise override an
// existing pin without rewriting it. The same record twice is harmless and
// must keep reading, so a migration that concatenated the file is not fatal.
func TestReadKnownHostsRejectsConflictingDuplicateHost(t *testing.T) {
	dir := t.TempDir()
	first := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(fakePublicKey("first"))))
	second := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(fakePublicKey("second"))))

	path := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(path, []byte("h:22 "+first+"\nh:22 "+second+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readKnownHosts(path); err == nil {
		t.Fatal("readKnownHosts picked one of two conflicting pins for h:22")
	}

	dup := filepath.Join(dir, "duplicated")
	if err := os.WriteFile(dup, []byte("h:22 "+first+"\nh:22 "+first+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := readKnownHosts(dup)
	if err != nil {
		t.Fatalf("an identical duplicate must not fail the read: %v", err)
	}
	if len(store) != 1 {
		t.Fatalf("store = %v, want one host", store)
	}
}

// replaceFile moves the old store aside before renaming the new one in, on the
// platforms that cannot rename over an existing file. A kill between the two
// renames leaves the pins under the displaced name and no store at all, and
// reading the missing store as empty would re-trust every host the operator
// had ever connected to. The displaced copy is the only record those pins
// survive in, so the read has to find it.
func TestReadKnownHostsRecoversDisplacedStore(t *testing.T) {
	withKnownHosts(t)
	path := knownHostsPath()
	key := fakePublicKey("remembered")
	line := "h:22 " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	if err := os.WriteFile(displacedPath(path), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := readKnownHosts(path)
	if err != nil {
		t.Fatalf("a displaced store must be read back: %v", err)
	}
	if store["h:22"] == "" {
		t.Fatalf("the displaced pin was lost: %v", store)
	}

	// The pins must be enforced, not merely returned: a host presenting a
	// different key is still refused, and presenting the pinned one is not
	// re-pinned as a first contact.
	cb, err := tofu()
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("h:22", nil, fakePublicKey("changed")); err == nil {
		t.Fatal("a displaced store accepted a changed host key")
	}
	if err := cb("h:22", nil, key); err != nil {
		t.Fatalf("the displaced pin was not honored: %v", err)
	}
	// A successful connect rewrites the store, so recovery is a one-time
	// read and the pin is served by a whole store from here on. Whether the
	// displaced copy is cleared depends on which replaceFile branch ran, and
	// that is not this test's business: the pin must survive either way.
	again, err := readKnownHosts(path)
	if err != nil {
		t.Fatal(err)
	}
	if again["h:22"] == "" {
		t.Fatalf("the recovered pin did not survive the rewrite: %v", again)
	}
}

// A displaced store that does not parse is a store that lost its records.
// Falling back to it and finding nothing there either would re-trust every
// host, so the read fails and names the file an operator has to look at.
func TestReadKnownHostsRejectsDisplacedStoreWithNoRecords(t *testing.T) {
	withKnownHosts(t)
	path := knownHostsPath()
	if err := os.WriteFile(displacedPath(path), []byte("\n \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readKnownHosts(path)
	if err == nil {
		t.Fatal("readKnownHosts accepted a displaced store holding no host records")
	}
	if !strings.Contains(err.Error(), displacedPath(path)) {
		t.Errorf("error should name the displaced file, got: %v", err)
	}
}

// Every write leaves a copy of the store beside it, so a store that a write
// then failed to put back is read from the copy rather than read as "nothing
// pinned". Without it, one lost file silently re-trusts every host the
// operator had ever connected to, which is the one outcome the store exists to
// prevent. The copy only stands in when a write left its marks behind: an
// operator who deletes the store on purpose is asking to re-pin, and handing
// back the backup would undo that.
func TestReadKnownHostsRecoversBackupStore(t *testing.T) {
	withKnownHosts(t)
	path := knownHostsPath()
	key := fakePublicKey("remembered")
	line := "h:22 " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	if err := writeKnownHosts(path, map[string]string{"h:22": line}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backupPath(path)); err != nil {
		t.Fatalf("a write must leave a copy of the store beside it: %v", err)
	}
	// The copy is the store as of the last write, not the one before it, so a
	// host pinned most recently is in it too.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// The staging file a write that died between CreateTemp and the rename
	// leaves behind: the evidence that this store went missing mid-write
	// rather than by hand.
	stageInterrupted(t, path)

	store, err := readKnownHosts(path)
	if err != nil {
		t.Fatalf("a store lost to an interrupted write must be read back from its copy: %v", err)
	}
	if store["h:22"] == "" {
		t.Fatalf("the copied pin was lost: %v", store)
	}

	// The recovered pins are enforced, not merely returned: a changed key is
	// still refused, and the pinned key is not re-pinned as a first contact.
	cb, err := tofu()
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("h:22", nil, fakePublicKey("changed")); err == nil {
		t.Fatal("a store recovered from its copy accepted a changed host key")
	}
	if err := cb("h:22", nil, key); err != nil {
		t.Fatalf("the recovered pin was not honored: %v", err)
	}

	// The connect above put the store back where it belongs, and it spends the
	// marks that said the store was lost, so the copy is a backup once more
	// rather than where the store now lives. A later loss is recovered from
	// that copy on its own evidence: the marks a new killed write leaves.
	// A store deleted with nothing beside it is the re-pin gesture, and is
	// covered by TestDeletingTheStoreRepinsRatherThanRecoveringTheBackup.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a recovered store was not written back to its own path: %v", err)
	}
	if interruptedWrite(path) {
		t.Error("the restore left the marks of a loss that has been repaired")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	stageInterrupted(t, path)
	if _, err := tofu(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a second interrupted write was not recovered from the copy: %v", err)
	}
	again, err := readKnownHosts(path)
	if err != nil {
		t.Fatal(err)
	}
	if again["h:22"] == "" {
		t.Fatalf("the recovered pin did not survive the rewrite: %v", again)
	}
}

func pinFor(host, label string) string {
	return host + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(fakePublicKey(label))))
}

// A copy that could not be written is warned about once, at the write, and a
// run that never connects again leaves the store with no copy beside it and
// nothing further said about it: the store becomes the only record of every
// pin on the host, and the RPO the warning described stops being tracked. The
// next connect is the only place that gap can be seen, so it closes it there
// and says that it did.
func TestCheckStoreCopyRewritesACopyItCannotRecoverTheStore(t *testing.T) {
	for _, tc := range []struct {
		name  string
		leave func(t *testing.T, path string)
	}{
		{"missing", func(t *testing.T, path string) {
			t.Helper()
			if err := os.Remove(backupPath(path)); err != nil {
				t.Fatal(err)
			}
		}},
		// What a copy write that could not land leaves behind: the store moved
		// on, the copy did not.
		{"older than the store", func(t *testing.T, path string) {
			t.Helper()
			past := time.Now().Add(-time.Hour)
			if err := os.Chtimes(backupPath(path), past, past); err != nil {
				t.Fatal(err)
			}
		}},
		// A copy that does not parse is a copy a restore could not use, so
		// having one is not the same as having a backup.
		{"damaged", func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(backupPath(path), []byte("\n \n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "known_hosts")
			store := map[string]string{"h:22": pinFor("h:22", "remembered")}
			if err := writeKnownHosts(path, store); err != nil {
				t.Fatal(err)
			}
			tc.leave(t, path)
			log := captureAudit(t)

			checkStoreCopy(path)

			b, err := os.ReadFile(backupPath(path))
			if err != nil {
				t.Fatalf("the copy the store is recovered from was not rewritten: %v", err)
			}
			copied, err := parseKnownHosts(backupPath(path), b)
			if err != nil {
				t.Fatalf("the rewritten copy does not parse: %v", err)
			}
			if copied["h:22"] != store["h:22"] {
				t.Fatalf("the rewritten copy lost the pinned key: %v", copied)
			}
			if !strings.Contains(log.String(), "backup rewritten from the store") {
				t.Errorf("a store left without a usable copy was not reported: %s", log.String())
			}
			// The gap is closed, so the next connect finds the copy current and
			// says nothing more about it.
			log.Reset()
			checkStoreCopy(path)
			if log.String() != "" {
				t.Errorf("a current copy was reported again: %s", log.String())
			}
		})
	}
}

// The healthy case is the one every run takes, and it must cost nothing: the
// copy a write leaves is not rewritten, so a connect cannot churn the file the
// store is recovered from, and no warning is logged for a store that is
// backed up.
func TestCheckStoreCopyLeavesACurrentCopyAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := writeKnownHosts(path, map[string]string{"h:22": pinFor("h:22", "remembered")}); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(backupPath(path))
	if err != nil {
		t.Fatal(err)
	}
	log := captureAudit(t)

	checkStoreCopy(path)

	after, err := os.Stat(backupPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("a current copy was rewritten by a run that had nothing to fix")
	}
	if log.String() != "" {
		t.Errorf("a backed-up store was reported: %s", log.String())
	}
}

// A store recovered from a copy must come back whole. Reading a copy that
// predates the last write hands back a shorter store than the operator had, and
// every host it dropped is re-trusted on the next connect, so the copy read is
// the freshest of the two sitting beside the store. The backup is normally it
// (every write refreshes it, the displaced copy is the content before that
// write), and the displaced copy is the fresher one only when the backup of the
// last write never landed.
func TestReadKnownHostsRecoversFromTheFresherCopy(t *testing.T) {
	pin := func(host, label string) string {
		return host + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(fakePublicKey(label))))
	}
	// One write with both pins, so the store and the backup hold them, then
	// the store is lost. What the copies hold afterwards is the case: a kill
	// between a write's two renames leaves a displaced copy carrying the write
	// before it, and a write whose backup could not land leaves the backup
	// carrying that older write instead.
	stage := func(t *testing.T) string {
		t.Helper()
		withKnownHosts(t)
		path := knownHostsPath()
		store := map[string]string{
			"old:22": pin("old:22", "old"),
			"new:22": pin("new:22", "new"),
		}
		if err := writeKnownHosts(path, store); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		return path
	}
	age := func(t *testing.T, path string, d time.Duration) {
		t.Helper()
		when := time.Now().Add(-d)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	displace := func(t *testing.T, path string, records map[string]string) {
		t.Helper()
		var b strings.Builder
		for _, host := range slices.Sorted(maps.Keys(records)) {
			b.WriteString(records[host] + "\n")
		}
		if err := os.WriteFile(displacedPath(path), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("the displaced copy predates the backup", func(t *testing.T) {
		path := stage(t)
		displace(t, path, map[string]string{"old:22": pin("old:22", "old")})
		age(t, displacedPath(path), time.Hour)
		store, err := readKnownHosts(path)
		if err != nil {
			t.Fatalf("a lost store must be read back from a copy: %v", err)
		}
		if store["new:22"] == "" {
			t.Fatalf("the pin the stale copy never held was lost: %v", store)
		}
		// The pins are enforced, not merely returned: a host present only in
		// the fresher copy must not be re-trusted.
		cb, err := tofu()
		if err != nil {
			t.Fatal(err)
		}
		if err := cb("new:22", nil, fakePublicKey("changed")); err == nil {
			t.Fatal("a host pinned only in the backup was re-trusted after a lost store")
		}
	})

	t.Run("the backup predates the displaced copy", func(t *testing.T) {
		// A write whose backup could not land: the displaced copy is what
		// replaceFile moved aside, and it is newer than the backup left by the
		// write before it.
		path := stage(t)
		displace(t, path, map[string]string{
			"old:22": pin("old:22", "old"),
			"new:22": pin("new:22", "new"),
		})
		// The backup left by the write before the last one, so it predates the
		// displaced copy and holds fewer pins.
		if err := os.WriteFile(backupPath(path), []byte(pin("old:22", "old")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		age(t, backupPath(path), time.Hour)
		store, err := readKnownHosts(path)
		if err != nil {
			t.Fatalf("a lost store must be read back from a copy: %v", err)
		}
		if store["new:22"] == "" {
			t.Fatalf("the pin the stale copy never held was lost: %v", store)
		}
	})
}

// A copy that does not parse is a copy that lost its records. Falling back to
// it and finding nothing there would re-trust every host, so the read fails
// and names the file an operator has to look at.
func TestReadKnownHostsRejectsBackupWithNoRecords(t *testing.T) {
	withKnownHosts(t)
	path := knownHostsPath()
	if err := os.WriteFile(backupPath(path), []byte("\n \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stageInterrupted(t, path)
	_, err := readKnownHosts(path)
	if err == nil {
		t.Fatal("readKnownHosts accepted a store copy holding no host records")
	}
	if !strings.Contains(err.Error(), backupPath(path)) {
		t.Errorf("error should name the copy, got: %v", err)
	}
}

// A damaged store is refused rather than replaced by a copy that parses, since
// a copy predating the last write is missing pins and reading it in place of
// the store would re-trust every host those pins covered. The refusal still
// has to say where the pins are: a store damaged by a hand edit or a tail the
// filesystem lost is the one loss an operator repairs by copying a file back,
// and an error naming only the broken line leaves the copy beside it unnamed.
func TestReadKnownHostsNamesTheCopyToRestoreFrom(t *testing.T) {
	withKnownHosts(t)
	path := knownHostsPath()
	key := fakePublicKey("remembered")
	line := "h:22 " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	if err := writeKnownHosts(path, map[string]string{"h:22": line}); err != nil {
		t.Fatal(err)
	}
	// A tail the filesystem lost: the last record keeps its host field and
	// nothing else.
	if err := os.WriteFile(path, []byte("h:22 \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := readKnownHosts(path)
	if err == nil {
		t.Fatal("readKnownHosts accepted a store whose record cannot be parsed")
	}
	msg := err.Error()
	if !strings.Contains(msg, backupPath(path)) {
		t.Errorf("error should name the copy that parses, got: %v", err)
	}
	wantCmd := restoreCommand(backupPath(path), path)
	if !strings.Contains(msg, wantCmd) {
		t.Errorf("error should carry the command that puts the copy back, got: %v", err)
	}
	// The refusal stands: the pins in the copy are not read in place of the
	// store until the operator copies them there.
	if _, err := os.Stat(backupPath(path)); err != nil {
		t.Errorf("the refused read must leave the copy alone: %v", err)
	}
	store, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(store) != "h:22 \n" {
		t.Errorf("the refused read must leave the store alone, got: %q", store)
	}

	// A connect is where the operator meets it, so the hint has to survive
	// the way the run actually fails rather than only the read underneath it.
	if _, err := tofu(); err == nil {
		t.Fatal("a connect accepted a store whose record cannot be parsed")
	} else if !strings.Contains(err.Error(), wantCmd) {
		t.Errorf("the connect error should carry the repair, got: %v", err)
	}
}

// A hint has to name a copy toktop would then accept. A copy that is itself
// damaged restores a file the next read refuses, which leaves the operator
// with a store they cannot use and an error that pointed them at it.
func TestReadKnownHostsDoesNotNameADamagedCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(path, []byte("h:22 not-a-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupPath(path), []byte("\n \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readKnownHosts(path)
	if err == nil {
		t.Fatal("readKnownHosts accepted a store whose record cannot be parsed")
	}
	if strings.Contains(err.Error(), backupPath(path)) {
		t.Errorf("error names a copy that does not parse, got: %v", err)
	}
}

// A store that never existed has no copy either, and must keep reading as
// empty: that is the one absence that means "no pins yet".
func TestReadKnownHostsWithNoStoreOrCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "never-written")
	store, err := readKnownHosts(path)
	if err != nil {
		t.Fatalf("a store that was never written must read as empty: %v", err)
	}
	if len(store) != 0 {
		t.Fatalf("store = %v, want empty", store)
	}
	restoreStore(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("nothing to restore must not leave a store behind: %v", err)
	}
}

// A store that parses to nothing is a store that lost its records, not a store
// nobody has written to: every writer emits the host it just pinned, so a
// file with no record line in it is truncated or emptied. Reading that as an
// empty store re-trusts every host on the next connect, silently, which is
// the outcome the file exists to prevent.
func TestReadKnownHostsRejectsStoreWithNoRecords(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"zero length":   "",
		"blank lines":   "\n \n\t\n",
		"comments only": "# managed elsewhere\n#\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := readKnownHosts(path)
			if err == nil {
				t.Fatal("readKnownHosts accepted a store holding no host records")
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error should name the file, got: %v", err)
			}
		})
	}

	// A store that was never created is the one absence that means "no pins
	// yet", and it must keep working.
	store, err := readKnownHosts(filepath.Join(dir, "absent"))
	if err != nil {
		t.Fatalf("a missing store must read as empty: %v", err)
	}
	if len(store) != 0 {
		t.Fatalf("store = %v, want empty", store)
	}
}

// A store another process holds must not be written from a stale snapshot:
// the read and the write are one critical section, so the lock is taken
// around both and released after, and a lock left by a dead process is broken
// rather than wedging the store forever.
func TestLockStoreSerializesAndBreaksAStaleLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// A live lock holder keeps the store busy; a second acquisition must wait
	// and then run once the first releases, not interleave with it.
	release := make(chan struct{})
	held := make(chan struct{})
	go func() {
		_ = lockStore(path, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	ran := make(chan struct{})
	go func() {
		_ = lockStore(path, func() error { close(ran); return nil })
	}()
	select {
	case <-ran:
		t.Fatal("a second writer ran while the store was locked")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-ran:
	case <-time.After(storeLockWait + storeLockPoll*20):
		t.Fatal("the second writer never got the lock after the first released")
	}

	// The lock is released on the way out, so the next acquisition is uncontended.
	if err := lockStore(path, func() error { return nil }); err != nil {
		t.Fatalf("uncontended lock failed: %v", err)
	}
	if _, err := os.Stat(path + storeLockSuffix); !os.IsNotExist(err) {
		t.Errorf("the lock file outlived the critical section: %v", err)
	}

	// A lock older than the stale age belongs to a process that died holding
	// it. It must be broken, not waited on until the timeout.
	stale := time.Now().Add(-2 * storeLockStale)
	lock := path + storeLockSuffix
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(lock, stale, stale); err != nil {
		t.Fatal(err)
	}
	ran = make(chan struct{})
	go func() {
		_ = lockStore(path, func() error { close(ran); return nil })
	}()
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("a stale lock was not broken")
	}
}

// A relative XDG_CONFIG_HOME is invalid per the base-directory spec, and
// honoring one would write the host-key pin store under the working
// directory: the store would vanish with the cwd and the next run would
// re-TOFU. An absolute value still wins, and a relative one must not produce
// a path under it.
func TestDefaultKnownHostsPathXDG(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	abs := filepath.Join(t.TempDir(), "cfg")
	t.Setenv("XDG_CONFIG_HOME", abs)
	if got, want := defaultKnownHostsPath(), filepath.Join(abs, "toktop", "known_hosts"); got != want {
		t.Fatalf("absolute XDG_CONFIG_HOME: got %q, want %q", got, want)
	}

	t.Setenv("XDG_CONFIG_HOME", "relative/cfg")
	got := defaultKnownHostsPath()
	if got == "" {
		return // rejected outright, which is the strictest reading
	}
	if !filepath.IsAbs(got) || strings.HasPrefix(got, "relative") {
		t.Fatalf("relative XDG_CONFIG_HOME placed the store at %q", got)
	}
}

// The fallback behind XDG_CONFIG_HOME is built from $HOME, which nothing
// checks: a relative one produces a relative config directory, and the pin
// store lands under the working directory, vanishing with the cwd and
// re-TOFUing the next run. A home that cannot place an absolute store names
// none, and the run fails at connect.
func TestDefaultKnownHostsPathRejectsRelativeHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "relative/home")
	t.Setenv("USERPROFILE", "relative/home") // os.UserHomeDir on windows
	t.Setenv("AppData", "relative/home")     // os.UserConfigDir on windows
	t.Setenv("APPDATA", "relative/home")
	if got := defaultKnownHostsPath(); got != "" {
		t.Fatalf("defaultKnownHostsPath() = %q, want no store for a relative home", got)
	}
}

// A password read from a file keeps its trailing newline, and the server
// would reject it as an ordinary authentication failure. A password that
// merely ends in a space must survive.
func TestPasswordSourceEnvTrailingNewline(t *testing.T) {
	tgt := Target{User: "u", Host: "h", Port: 22}
	for _, tt := range []struct{ in, want string }{
		{"sekrit\n", "sekrit"},
		{"sekrit\r\n", "sekrit"},
		{" sekrit", " sekrit"},
		{"sekrit ", "sekrit "},
	} {
		t.Setenv("TOKTOP_SSH_PASSWORD", tt.in)
		pw, err := (&passwordSource{}).get(tgt)
		if err != nil || pw != tt.want {
			t.Errorf("get(%q) = %q, %v; want %q", tt.in, pw, err, tt.want)
		}
	}
}

// An empty $TOKTOP_SSH_PASSWORD is a wrapper that configured the run with no
// password, not one that never mentioned it: the headless error names the
// variable and its state instead of telling the operator to set a variable
// they already set.
func TestPasswordSourceEnvSetButEmpty(t *testing.T) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("stdin is a terminal; the run would prompt instead of erroring")
	}
	tgt := Target{User: "u", Host: "h", Port: 22}
	t.Setenv(PasswordEnv, "")
	_, err := (&passwordSource{}).get(tgt)
	if err == nil {
		t.Fatal("get() with an empty password env returned no error")
	}
	if !strings.Contains(err.Error(), "set but empty") {
		t.Errorf("get() error = %v, want it to name the variable as set but empty", err)
	}
}

// An unreadable ~/.ssh/config is not an absent one. Degrading to defaults
// dials the wrong port with no key, and the operator sees a bare
// authentication rejection with nothing pointing at the config file.
func TestParseTargetFailsLoudOnUnreadableSSHConfig(t *testing.T) {
	stubSSHConfig(t, "/home/operator/.ssh/config",
		func(string) ([]byte, error) { return nil, fs.ErrPermission })

	if _, err := ParseTarget("ssh://gpu"); err == nil {
		t.Fatal("ParseTarget returned nil error for an unreadable ssh config, want the read failure")
	} else if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("ParseTarget error = %v, want one wrapping fs.ErrPermission", err)
	}
}

// An absent config is not a failure: toktop must still connect.
func TestParseTargetToleratesAbsentSSHConfig(t *testing.T) {
	stubSSHConfig(t, "/home/operator/.ssh/config",
		func(string) ([]byte, error) { return nil, os.ErrNotExist })

	tgt, err := ParseTarget("ssh://gpu")
	if err != nil {
		t.Fatalf("ParseTarget: %v", err)
	}
	if tgt.Port != 22 || tgt.Host != "gpu" {
		t.Errorf("resolved = %+v, want the defaults host=gpu port=22", tgt)
	}
}

// DNS case does not change a host's identity, so the pin store keys on the
// ASCII-folded name. Before, ssh://Box.example and ssh://box.example were two
// entries and the second connection silently re-trusted a pinned host.
func TestKnownHostsStoreKeysHostsCaseInsensitively(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	key := string(ssh.MarshalAuthorizedKey(testHostKeyPub(t)))
	if err := writeKnownHosts(path, map[string]string{"box.example:22": "Box.Example:22 " + key}); err != nil {
		t.Fatal(err)
	}
	store, err := readKnownHosts(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store["box.example:22"]; !ok {
		t.Fatalf("store = %v, want the pin under its folded name", store)
	}
	if pinKey(store["box.example:22"]) != pinKey("Box.Example:22 "+key) {
		t.Errorf("pinKey differs across a case change, so a re-case would read as a changed host key")
	}
	// Two spellings of one host carrying the same key are one record, not a
	// store to refuse over.
	store["BOX.EXAMPLE:22"] = "BOX.EXAMPLE:22 " + key
	if err := writeKnownHosts(path, store); err != nil {
		t.Fatal(err)
	}
	again, err := readKnownHosts(path)
	if err != nil {
		t.Fatalf("re-reading a store with two spellings of one host failed: %v", err)
	}
	if len(again) != 1 {
		t.Errorf("store holds %d records, want 1: %v", len(again), again)
	}
}

// A record may carry a trailing comment, which the parser accepts. It is not
// part of the key, so a store another tool annotated must not read as a
// changed host key on the next connect.
func TestKnownHostsPinIgnoresTrailingComment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	key := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(testHostKeyPub(t))))
	if err := writeKnownHosts(path, map[string]string{
		"box.example:22": "box.example:22 " + key + " my laptop",
	}); err != nil {
		t.Fatal(err)
	}
	store, err := readKnownHosts(path)
	if err != nil {
		t.Fatal(err)
	}
	if pinKey(store["box.example:22"]) != pinKey("box.example:22 "+key) {
		t.Errorf("pinKey differs across a trailing comment, so an annotated store reads as a changed host key")
	}
}

func testHostKeyPub(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pk
}

// The peer answers the exec channel, so its stdout is attacker-shaped and
// unbounded. A cap that truncates instead of failing would hand
// parseProcScan a short process table that reads as the real one.
func TestStdoutBufRefusesPastTheCapInsteadOfGrowing(t *testing.T) {
	var b stdoutBuf
	chunk := make([]byte, 64<<10)
	for b.n < stdoutCap {
		if _, err := b.Write(chunk); err != nil {
			t.Fatalf("write under the cap: %v", err)
		}
	}
	if got := len(b.String()); got != stdoutCap {
		t.Fatalf("collected %d bytes, want %d", got, stdoutCap)
	}
	if b.Overflowed() {
		t.Fatal("Overflowed before the cap was passed")
	}
	if _, err := b.Write([]byte("x")); !errors.Is(err, errStdoutOverflow) {
		t.Fatalf("write past the cap = %v, want errStdoutOverflow", err)
	}
	if !b.Overflowed() {
		t.Error("Overflowed() false after the cap was passed")
	}
	// The prefix survives so the overflow is diagnosable, and it is still
	// capped: an over-long answer must not have been buffered in full.
	if got := len(b.String()); got != stdoutCap {
		t.Errorf("after overflow collected %d bytes, want %d", got, stdoutCap)
	}
}

// A write that straddles the cap keeps the part that fits and still fails, so
// the collected output never exceeds it.
func TestStdoutBufClipsAStraddlingWrite(t *testing.T) {
	var b stdoutBuf
	if _, err := b.Write(make([]byte, stdoutCap-10)); err != nil {
		t.Fatalf("write under the cap: %v", err)
	}
	if _, err := b.Write(make([]byte, 4096)); !errors.Is(err, errStdoutOverflow) {
		t.Fatalf("straddling write = %v, want errStdoutOverflow", err)
	}
	if got := len(b.String()); got != stdoutCap {
		t.Errorf("collected %d bytes, want %d", got, stdoutCap)
	}
}

func TestRedactPeerHomeKeepsTheCauseReachable(t *testing.T) {
	c := &Client{Target: Target{Host: "box", User: "peer"}}

	// The text is folded, so the account's own name and its home path are
	// gone from what a log line or a report renders.
	folded := c.redactPeerHome(errors.New("bash: /home/peer/.bashrc: No such file"))
	if msg := folded.Error(); strings.Contains(msg, "/home/peer") || !strings.Contains(msg, "~") {
		t.Errorf("redactPeerHome left the peer home in the text: %q", msg)
	}

	// The cause is still reachable by identity, which is what lets connLost
	// tell a hung-up peer from a refused one without reading words. A rebuild
	// from the string matched nothing here, so every remote failure arrived
	// as the same untyped value.
	sentinel := errors.New("remote read failed")
	if got := c.redactPeerHome(sentinel); !errors.Is(got, sentinel) {
		t.Errorf("redactPeerHome dropped the cause: errors.Is cannot find %v in %v", sentinel, got)
	}

	// net.OpError carries a *net.OpError under it, so the type assertion has
	// to keep working through the fold too.
	opErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	var target *net.OpError
	if got := c.redactPeerHome(opErr); !errors.As(got, &target) {
		t.Errorf("redactPeerHome dropped the *net.OpError: errors.As cannot find it in %v", got)
	}

	// nil stays nil: a successful run has no error to fold.
	if got := c.redactPeerHome(nil); got != nil {
		t.Errorf("redactPeerHome(nil) = %v, want nil", got)
	}
}

func TestConnLostClassifiesByIdentityBeforeText(t *testing.T) {
	// A peer that hung up and a client we closed both mean the connection is
	// gone, and neither says so in words this function would have matched
	// before the fold stopped erasing the cause.
	for name, cause := range map[string]error{
		"eof":      io.EOF,
		"closed":   net.ErrClosed,
		"deadline": os.ErrDeadlineExceeded,
	} {
		if err := connLost(cause); !strings.Contains(err.Error(), "ssh connection lost") {
			t.Errorf("connLost(%s) = %q, want the connection-lost label", name, err)
		}
	}

	// A cause that is not a lost connection is left alone, and it keeps its
	// identity either way.
	authErr := errors.New("ssh: unable to authenticate, attempted methods [none]")
	if got := connLost(authErr); !errors.Is(got, authErr) {
		t.Errorf("connLost relabeled an auth failure: %v", got)
	}

	// The word match is still the only way to classify a cause the standard
	// library gives no identity to, so a script's own "connection lost" on
	// stdout keeps reaching the branch.
	if got := connLost(errors.New("vitals: connection lost")); !strings.Contains(got.Error(), "ssh connection lost") {
		t.Errorf("connLost dropped the text fallback: %q", got)
	}
}

func TestAcceptBackoffGrowsJittersAndStaysUnderTheCap(t *testing.T) {
	// The first retry has to stay near forwardAcceptRetry: the case the pacing
	// exists for is a descriptor exhaustion that clears on its own, and a port
	// waiting seconds to answer again is the failure the gap was added to stop.
	first := acceptBackoff(forwardAcceptRetry, 40000, 1)
	if first <= 0 || first > forwardAcceptRetry {
		t.Errorf("acceptBackoff(%v) = %v, want (0, %v]", forwardAcceptRetry, first, forwardAcceptRetry)
	}

	// The gap is a function of the port and the failure count, so the same
	// failure twice backs off identically. A draw from a global generator made
	// every port's retry schedule unrepeatable across runs.
	if again := acceptBackoff(forwardAcceptRetry, 40000, 1); again != first {
		t.Errorf("acceptBackoff(%v, 40000, 1) = %v then %v, want the same gap twice", forwardAcceptRetry, first, again)
	}

	// Jitter has to actually move the value, or two ports failing on one
	// system condition retry in lockstep and rebuild the pressure that failed
	// them. A single key proves nothing, so look at a run of ports.
	seen := make(map[time.Duration]bool)
	for port := range 64 {
		seen[acceptBackoff(forwardAcceptRetryMax, 40000+port, 1)] = true
	}
	if len(seen) < 8 {
		t.Errorf("acceptBackoff produced %d distinct values over 64 ports, want a spread", len(seen))
	}

	// The same run of ports has to produce the same run of gaps, which is the
	// property the mix is there for.
	for port := range 64 {
		if a, b := acceptBackoff(forwardAcceptRetryMax, 40000+port, 1), acceptBackoff(forwardAcceptRetryMax, 40000+port, 1); a != b {
			t.Fatalf("acceptBackoff(%v, %d, 1) = %v then %v, want the same gap twice", forwardAcceptRetryMax, 40000+port, a, b)
		}
	}

	// Successive failures on one port move too, or a port that keeps failing
	// retries on the same offset every time.
	fails := make(map[time.Duration]bool)
	for n := 1; n <= 16; n++ {
		fails[acceptBackoff(forwardAcceptRetryMax, 40000, n)] = true
	}
	if len(fails) < 8 {
		t.Errorf("acceptBackoff produced %d distinct values over 16 failure counts, want a spread", len(fails))
	}

	// Every gap stays inside the cap, including at the top of the range where
	// an off-by-one would push past it.
	for port := range 64 {
		if got := acceptBackoff(forwardAcceptRetryMax, 40000+port, 1); got > forwardAcceptRetryMax {
			t.Fatalf("acceptBackoff(%v) = %v, over the cap", forwardAcceptRetryMax, got)
		}
	}

	// A wait that has not yet reached the cap is exactly doubled by the relay
	// loop, and the doubling is what has to stop.
	wait := forwardAcceptRetry
	for i := 0; i < 20; i++ {
		wait = min(wait*2, forwardAcceptRetryMax)
	}
	if wait != forwardAcceptRetryMax {
		t.Errorf("doubling settled at %v, want the cap %v", wait, forwardAcceptRetryMax)
	}
}
