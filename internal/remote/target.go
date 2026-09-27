package remote

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/maci0/toktop/internal/core"
)

// Target is one ssh-reachable host to monitor, fully resolved: explicit URL
// fields win over ~/.ssh/config, which wins over defaults.
type Target struct {
	User string
	Host string
	Port int // 22 when unset
	// KeyFile optionally names a private key used ahead of agent/default keys.
	KeyFile string
}

// ParseTarget parses ssh://[user@]host[:port] and applies ~/.ssh/config
// overrides for everything the URL leaves unset. A password in the URL is
// rejected: it would be visible in process listings, and this parser would
// otherwise ignore it. A path, query, or fragment is rejected rather than
// ignored.
func ParseTarget(raw string) (Target, error) {
	t, err := parseURLTarget(raw)
	if err != nil {
		return t, err
	}
	if t.KeyFile == "" && t.Host != "" {
		cfg, cerr := lookupSSHConfig(t.Host)
		// An unreadable config is not an absent one: falling through to
		// defaults connects to the wrong port with the wrong key and the
		// failure surfaces later as a bare authentication rejection.
		if cerr != nil {
			return Target{}, cerr
		}
		if cfg != nil {
			if t.User == "" {
				t.User = cfg.User
			}
			if t.Port == 0 {
				t.Port = cfg.Port
			}
			if t.KeyFile == "" {
				t.KeyFile = cfg.IdentityFile
			}
			if cfg.HostName != "" {
				t.Host = cfg.HostName
			}
		}
	}
	if t.Port == 0 {
		t.Port = 22
	}
	if strings.ContainsAny(t.Host, " \t\r\n\x00") {
		return Target{}, fmt.Errorf("bad ssh host %q", t.Host)
	}
	if strings.ContainsAny(t.User, " \t\r\n\x00") {
		return Target{}, fmt.Errorf("bad ssh user %q", t.User)
	}
	return t, nil
}

func parseURLTarget(raw string) (Target, error) {
	if !strings.HasPrefix(raw, "ssh://") {
		return Target{}, fmt.Errorf("target %q must start with ssh://", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		// url.Parse may echo userinfo, which is the password when one was
		// embedded; do not wrap that text into our error.
		return Target{}, errors.New("bad ssh target")
	}
	if u.User != nil {
		if _, set := u.User.Password(); set {
			return Target{}, errors.New("ssh target must not contain a password; set TOKTOP_SSH_PASSWORD or use --ssh-key")
		}
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return Target{}, errors.New("ssh target must be ssh://[user@]host[:port]")
	}
	t := Target{Host: u.Hostname()}
	if u.User != nil {
		t.User = u.User.Username()
	}
	if p := u.Port(); p != "" {
		t.Port, err = strconv.Atoi(p)
		if err != nil || t.Port <= 0 || t.Port > 65535 {
			return Target{}, fmt.Errorf("bad ssh port %q", p)
		}
	}
	if t.Host == "" {
		return Target{}, fmt.Errorf("ssh target missing host")
	}
	return t, nil
}

func (t Target) userHost() string {
	if t.User == "" {
		return t.Host
	}
	return t.User + "@" + t.Host
}

// sshConfigEntry is the subset of ~/.ssh/config toktop understands.
type sshConfigEntry struct {
	User         string
	HostName     string
	Port         int
	IdentityFile string
}

var sshConfigPath = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ssh", "config")
}

// configReader is swappable in tests.
var configReader = func(path string) ([]byte, error) { return os.ReadFile(path) }

// lookupSSHConfig finds the first Host block matching name and returns the
// values it defines. Like OpenSSH, the first obtained value wins. A config
// that is absent yields (nil, nil); one that exists but cannot be read is an
// error, so the caller never mistakes a permission failure for no config.
func lookupSSHConfig(name string) (*sshConfigEntry, error) {
	path := sshConfigPath()
	if path == "" {
		return nil, nil
	}
	b, err := configReader(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("cannot read ssh config %s: %w", core.RedactHome(path), err)
	}
	if len(b) == 0 {
		return nil, nil
	}
	return parseSSHConfig(b, name), nil
}

// parseSSHConfig scans an ssh_config byte slice for the first Host block matching
// name and returns the values it defines.
func parseSSHConfig(b []byte, name string) *sshConfigEntry {
	entry := &sshConfigEntry{}
	matched := false
	inBlock := false
	for line := range strings.SplitSeq(string(b), "\n") {
		key, val, ok := cutConfigField(line)
		if !ok {
			continue
		}
		switch strings.ToLower(key) {
		case "host":
			inBlock = false
			negated := false
			for pat := range strings.FieldsSeq(val) {
				pat = strings.ToLower(pat)
				if strings.HasPrefix(pat, "!") {
					if patternMatch(strings.TrimPrefix(pat, "!"), strings.ToLower(name)) {
						negated = true
						break
					}
					continue
				}
				if patternMatch(pat, strings.ToLower(name)) {
					inBlock = true
				}
			}
			inBlock = inBlock && !negated
			matched = matched || inBlock
		case "hostname":
			if inBlock && entry.HostName == "" {
				entry.HostName = val
			}
		case "user":
			if inBlock && entry.User == "" {
				entry.User = val
			}
		case "port":
			if inBlock && entry.Port == 0 {
				if p, err := strconv.Atoi(val); err == nil && p > 0 && p < 65536 {
					entry.Port = p
				}
			}
		case "identityfile":
			if inBlock && entry.IdentityFile == "" {
				entry.IdentityFile = expandTilde(val)
			}
		}
	}
	if !matched {
		return nil
	}
	return entry
}

// cutConfigField splits an ssh_config line into keyword and argument,
// handling "key=value", space and tab separation, double-quoted arguments and
// '#' comments.
func cutConfigField(line string) (key, val string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	i := strings.IndexAny(line, " \t=")
	if i < 0 {
		return "", "", false
	}
	rest := strings.TrimSpace(line[i:])
	rest = strings.TrimPrefix(rest, "=")
	return line[:i], unquote(strings.TrimSpace(rest)), true
}

// unquote drops the surrounding double quotes ssh_config puts around an
// argument containing spaces, and any comment trailing the closing quote.
// Windows IdentityFile paths are quoted routinely, since a user directory
// under C:\Users almost always has a space in it.
//
// Only the quotes go: a backslash inside the quotes stays a backslash, because
// on Windows that is the path separator and eating it would break the quoted
// paths this exists to read.
func unquote(s string) string {
	if !strings.HasPrefix(s, `"`) {
		return s
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '"':
			return b.String() // whatever follows the closing quote is a comment
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// patternMatch implements ssh_config glob matching ('*' and '?') through
// path.Match, whose character-class and escape extensions real host patterns
// do not use.
func patternMatch(pat, s string) bool {
	matched, err := path.Match(pat, s)
	return err == nil && matched
}

func expandTilde(p string) string {
	if p == "~" {
		// A failed home lookup leaves p unchanged, matching the ~/ branch
		// below, rather than expanding to an empty key path that would
		// silently drop the credential ssh config named.
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return home
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// ResolveKeyFile expands a leading tilde in file and checks that the result
// names a regular file. --ssh-key uses this at startup so a typo or a
// directory fails before any ssh dial, rather than as a generic auth
// rejection after discovery has already run.
//
// Every diagnostic this returns names a file under $HOME, so the home is
// rewritten to "~" on each of them, like Connect: the reason the key could
// not be used is worth more to the operator than the account name, and these
// lines are what gets pasted into issues.
func ResolveKeyFile(file string) (string, error) {
	file = expandTilde(strings.TrimSpace(file))
	if file == "" {
		return "", errors.New("empty path")
	}
	fi, err := os.Stat(file)
	if err != nil {
		return "", errors.New(core.RedactHome(err.Error()))
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", core.RedactHome(file))
	}
	return file, nil
}
