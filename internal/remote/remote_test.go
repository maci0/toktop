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
	"io/fs"
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
	useSSHConfig(t, `
# comment
Host gpu
  hostname 192.168.0.212
  user maci
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
	want := Target{User: "maci", Host: "192.168.0.212", Port: 2022}
	if tgt.User != want.User || tgt.Host != want.Host || tgt.Port != want.Port {
		t.Errorf("resolved = %+v, want %+v", tgt, want)
	}
	// expandTilde resolves to $HOME, not the temp dir; verify the suffix only.
	if !strings.HasSuffix(tgt.KeyFile, "gpu_key") {
		t.Errorf("keyfile = %q", tgt.KeyFile)
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
		{"User maci # me", "User", "maci", true},
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

	const freeBudget = 2 * time.Second
	done := make(chan error, 1)
	go func() { done <- freeCB("free:22", nil, fakePublicKey("free")) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second store rejected: %v", err)
		}
	case <-time.After(freeBudget):
		t.Fatalf("writing one store blocked a write to another for over %s", freeBudget)
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

func TestAnswerPasswordPromptSingleSecret(t *testing.T) {
	get := func() (string, error) { return "sekrit", nil }
	got, err := answerPasswordPrompt([]string{"Password:"}, []bool{false}, get)
	if err != nil || len(got) != 1 || got[0] != "sekrit" {
		t.Fatalf("single password prompt = %q, %v", got, err)
	}
	if _, err := answerPasswordPrompt([]string{"Password:", "OTP:"}, []bool{false, false}, get); err == nil {
		t.Fatal("multiple prompts must be refused")
	}
	if _, err := answerPasswordPrompt([]string{"Username:"}, []bool{true}, get); err == nil {
		t.Fatal("echoing prompt must be refused")
	}
	if _, err := answerPasswordPrompt(nil, nil, get); err == nil {
		t.Fatal("empty challenge must be refused")
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
	old := time.Now().Add(-2 * staleTempAge)
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

// Every write leaves a copy of the store beside it, so a store that is then
// lost, emptied or overwritten by something else is read back from the copy
// rather than read as "nothing pinned". Without it, one deleted file silently
// re-trusts every host the operator had ever connected to, which is the one
// outcome the store exists to prevent.
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

	store, err := readKnownHosts(path)
	if err != nil {
		t.Fatalf("a lost store must be read back from its copy: %v", err)
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

	// Connecting again puts the store back where it belongs, so the copy is a
	// backup once more rather than where the store now lives. The connect
	// above rewrites it; this is the path a second run takes.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := tofu(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a recovered store was not written back to its own path: %v", err)
	}
	again, err := readKnownHosts(path)
	if err != nil {
		t.Fatal(err)
	}
	if again["h:22"] == "" {
		t.Fatalf("the recovered pin did not survive the rewrite: %v", again)
	}
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
	_, err := readKnownHosts(path)
	if err == nil {
		t.Fatal("readKnownHosts accepted a store copy holding no host records")
	}
	if !strings.Contains(err.Error(), backupPath(path)) {
		t.Errorf("error should name the copy, got: %v", err)
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
