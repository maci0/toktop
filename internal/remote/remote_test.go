package remote

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

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
		{"ssh://maci@192.168.0.211", "maci", "192.168.0.211", 22, false},
		{"ssh://root@gpu-box:2222", "root", "gpu-box", 2222, false},
		{"ssh://192.168.1.5", "", "192.168.1.5", 22, false},
		{"ssh://maci@box/", "maci", "box", 22, false},
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

func TestParseTargetSSHConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	if err := os.WriteFile(cfg, []byte(`
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
`), 0o600); err != nil {
		t.Fatal(err)
	}
	oldPath, oldRead := sshConfigPath, configReader
	defer func() { sshConfigPath, configReader = oldPath, oldRead }()
	sshConfigPath = func() string { return cfg }
	configReader = func(path string) ([]byte, error) { return os.ReadFile(path) }

	tgt, err := ParseTarget("ssh://gpu")
	if err != nil {
		t.Fatal(err)
	}
	want := Target{User: "maci", Host: "192.168.0.212", Port: 2022,
		KeyFile: filepath.Join(dir, ".ssh", "gpu_key")}
	// expandTilde resolves to $HOME, not dir; just verify suffix
	if tgt.User != want.User || tgt.Host != want.Host || tgt.Port != want.Port {
		t.Errorf("resolved = %+v, want %+v", tgt, want)
	}
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
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	tabbed := "Host\tgpu\n\tHostName\t10.9.8.7\n\tPort\t2022\n"
	if err := os.WriteFile(cfg, []byte(tabbed), 0o600); err != nil {
		t.Fatal(err)
	}
	oldPath, oldRead := sshConfigPath, configReader
	defer func() { sshConfigPath, configReader = oldPath, oldRead }()
	sshConfigPath = func() string { return cfg }
	configReader = func(path string) ([]byte, error) { return os.ReadFile(path) }

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
	if got := expandTilde("~"); got != home {
		t.Errorf("expandTilde(~) = %q, want %q", got, home)
	}
	want := filepath.Join(home, "rel")
	if got := expandTilde("~/rel"); got != want {
		t.Errorf("expandTilde(~/rel) = %q, want %q", got, want)
	}
	if got := expandTilde(`~\rel`); got != want {
		t.Errorf(`expandTilde(~\rel) = %q, want %q`, got, want)
	}
	if got := expandTilde("/abs/key"); got != "/abs/key" {
		t.Errorf("expandTilde(absolute) = %q, want unchanged", got)
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
	if got, want := len(defaultKeyPaths()), 3; got != want {
		t.Fatalf("defaultKeyPaths = %d paths, want %d", got, want)
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
	// A second write converges: the directory holds the store, the in-flight
	// file, and nothing the sweep added.
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
	want := []string{knownHostsTempPrefix + "in-flight", "config.toml", "known_hosts"}
	slices.Sort(names)
	if !slices.Equal(names, want) {
		t.Fatalf("directory = %v, want %v", names, want)
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
