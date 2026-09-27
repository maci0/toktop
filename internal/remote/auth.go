package remote

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/term"
)

// defaultKeyPaths lists the per-user private keys tried after any explicitly
// configured one, approximating ssh's default identities.
func defaultKeyPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	names := []string{"id_ed25519", "id_ecdsa", "id_rsa"}
	paths := make([]string, 0, len(names))
	for _, n := range names {
		paths = append(paths, filepath.Join(home, ".ssh", n))
	}
	return paths
}

// loadSigner reads one private key file. An encrypted key yields a named
// error rather than a prompt, so a headless run never blocks; whether that
// error aborts the auth chain or just drops the key is keyFileAuth's call.
func loadSigner(path string) (ssh.Signer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := ssh.ParsePrivateKey(b)
	if _, ok := errors.AsType[*ssh.PassphraseMissingError](err); ok {
		return nil, fmt.Errorf("%s: encrypted; use a passphrase-less key or ssh-agent", filepath.Base(path))
	}
	return s, err
}

// keyFileAuth builds an AuthMethod from one key file. required marks the
// target's own key (--ssh-key, or IdentityFile from ~/.ssh/config): its load
// failure must reach the operator instead of degrading into a confusing
// generic auth rejection; the ~/.ssh defaults are best effort and may return
// an error the caller ignores.
func keyFileAuth(path string, required bool) (ssh.AuthMethod, error) {
	if path == "" {
		return nil, nil
	}
	s, err := loadSigner(path)
	if err != nil {
		if !required {
			return nil, nil
		}
		return nil, err
	}
	return ssh.PublicKeys(s), nil
}

// passwordSource yields a secret at most once per connection attempt chain:
// TOKTOP_SSH_PASSWORD for headless runs, otherwise a terminal prompt. It
// remembers the answer so password and keyboard-interactive mechanisms share
// it without asking twice, and remembers why no answer was produced so an
// aborted prompt surfaces as its real cause instead of a generic auth failure.
type passwordSource struct {
	mu    sync.Mutex
	pw    string
	asked bool
	err   error // set when asked but no password could be obtained
}

var interactivePassword = func(t Target) (string, error) {
	// The prompt rides stderr, not stdout: stdout carries the dashboard (or
	// a capture of it), and diagnostics must never mix into either.
	fmt.Fprintf(os.Stderr, "toktop: password for %s: ", t.userHost())
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if len(b) == 0 {
		return "", fmt.Errorf("empty password")
	}
	return string(b), nil
}

// PasswordEnv is the variable holding the ssh password for headless runs.
// Exported so the startup warning that names a variable this run cannot use
// spells it the same way as the code that reads it, the way logcfg.LevelEnv
// is shared with the top-level command.
const PasswordEnv = "TOKTOP_SSH_PASSWORD"

// sshPasswordEnv reads TOKTOP_SSH_PASSWORD with the line ending a file-read
// leaves behind removed: `export TOKTOP_SSH_PASSWORD=$(cat id_rsa.pass)` keeps
// the newline, and the server would reject the password with a plain
// "permission denied" that names nothing about the cause. Only a trailing
// newline is stripped, so a password that genuinely ends in a space still
// authenticates.
func sshPasswordEnv() string {
	return strings.TrimRight(os.Getenv(PasswordEnv), "\r\n")
}

func (p *passwordSource) get(t Target) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.asked {
		if p.pw != "" {
			return p.pw, nil
		}
		return "", p.err
	}
	p.asked = true
	if v := sshPasswordEnv(); v != "" {
		p.pw = v
		return v, nil
	}
	// A wrapper that exports the variable empty is a misconfiguration the
	// generic message would hide by telling the operator to set a variable
	// they already set. Named in both branches: headless as the cause, on a
	// terminal as the reason the prompt is asking at all.
	_, set := os.LookupEnv(PasswordEnv)
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		if set {
			p.err = fmt.Errorf("$%s is set but empty, and stdin is not a terminal to prompt on", PasswordEnv)
		} else {
			p.err = fmt.Errorf("no password available (stdin is not a terminal; set %s)", PasswordEnv)
		}
		return "", p.err
	}
	if set {
		fmt.Fprintf(os.Stderr, "toktop: $%s is set but empty; prompting for the password instead\n", PasswordEnv)
	}
	v, err := interactivePassword(t)
	if err != nil {
		p.err = fmt.Errorf("password prompt failed: %w", err)
		return "", p.err
	}
	p.pw = v
	return v, nil
}

// authCallbacks turns a passwordSource into the two standard mechanisms so
// servers preferring either password or keyboard-interactive both work.
func (p *passwordSource) authCallbacks(t Target) []ssh.AuthMethod {
	get := func() (string, error) { return p.get(t) }
	pw := ssh.PasswordCallback(get)
	ki := ssh.KeyboardInteractive(func(_ string, _ string, questions []string, echos []bool) ([]string, error) {
		return answerPasswordPrompt(questions, echos, get)
	})
	return []ssh.AuthMethod{pw, ki}
}

// agentDialTimeout bounds the wait for the ssh-agent to answer a dial. It is
// one constant because the bound is the contract on every platform: a wedged
// ssh-agent (full backlog, unresponsive daemon) must not pin Connect past it,
// or a machine with an agent configured and no agent running never reaches the
// password prompt at all.
const agentDialTimeout = 2 * time.Second

// platformAgentSock is the platform's default agent endpoint when
// SSH_AUTH_SOCK is unset. Unix has none; Windows OpenSSH uses a named pipe
// and does not set the env var. Tests may replace it.
var platformAgentSock = defaultAgentSock

func agentSock() string {
	if s := os.Getenv("SSH_AUTH_SOCK"); s != "" {
		return s
	}
	return platformAgentSock()
}

// answerPasswordPrompt fills one keyboard-interactive challenge with the
// SSH password. A hostile or 2FA server can ask several questions; answering
// all of them with the same secret would send the password to every prompt
// (and fail real OTP). An echoing prompt is not a password field.
func answerPasswordPrompt(questions []string, echos []bool, get func() (string, error)) ([]string, error) {
	if len(questions) != 1 {
		return nil, fmt.Errorf("keyboard-interactive: refusing %d prompts (want a single password prompt)", len(questions))
	}
	if len(echos) > 0 && echos[0] {
		return nil, errors.New("keyboard-interactive: refusing an echoing prompt")
	}
	s, err := get()
	if err != nil {
		return nil, err
	}
	return []string{s}, nil
}

// dialAgent is swappable in tests.
var dialAgent = func(sock string) (agent.Agent, func(), error) {
	c, err := dialAgentConn(sock)
	if err != nil {
		return nil, nil, err
	}
	return agent.NewClient(c), func() { c.Close() }, nil
}

// authMethods assembles the credential chain in preference order: explicit
// key file, config/default keys, then the agent if one is reachable. A
// load failure of the explicitly configured key aborts the chain: dialing
// without it could only end in a misleading credentials-rejected error.
func (t Target) authMethods() ([]ssh.AuthMethod, func(), error) {
	cleanup := func() {}
	var methods []ssh.AuthMethod
	if m, err := keyFileAuth(t.KeyFile, true); err != nil {
		return nil, cleanup, fmt.Errorf("key %s: %w", t.KeyFile, err)
	} else if m != nil {
		methods = append(methods, m)
	}
	for _, p := range defaultKeyPaths() {
		if m, _ := keyFileAuth(p, false); m != nil { // defaults are best effort
			methods = append(methods, m)
		}
	}
	if sock := agentSock(); sock != "" {
		if ag, ac, err := dialAgent(sock); err == nil {
			methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
			old := cleanup
			cleanup = func() { old(); ac() }
		}
	}
	return methods, cleanup, nil
}
