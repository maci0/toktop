package remote

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/maci0/toktop/internal/core"
	"golang.org/x/text/unicode/norm"
)

// foldHost is the fold every host name is keyed and matched by: case-folded
// like DNS, and NFC-composed first. FoldASCII alone leaves every non-ASCII
// byte alone, which is right for a name that is ASCII by construction and
// wrong for a hostname, which may carry an IDN in U-label form. A macOS
// terminal hands the operator the NFD spelling of an accented host; DNS and
// ssh_config both speak the composed one, so without the composition
// ssh://café.example typed one way is a second target (ParseTargets keys
// on it), a second ssh_config block that never matches, and a second
// trust-on-first-use pin for a host the operator already pinned. This is
// the same pairing internal/bearer uses for a host name it compares.
func foldHost(s string) string {
	return core.FoldASCII(norm.NFC.String(s))
}

// portMax is the highest port a target or an ssh_config entry may name.
const portMax = 65535

// Target is one ssh-reachable host to monitor, fully resolved. Explicit URL
// user, port and key win over ~/.ssh/config, which wins over defaults; a
// HostName from config always replaces the URL host, as OpenSSH does.
type Target struct {
	User string
	Host string
	Port int // 22 when unset
	// KeyFile optionally names a private key used ahead of agent/default keys.
	KeyFile string
	// NoIdentityFiles records that the matching ~/.ssh/config block wrote
	// "IdentityFile none", which turns the ~/.ssh default identities off. The
	// agent is still consulted, the way ssh consults it unless IdentitiesOnly
	// is set, so this suppresses the default key files alone.
	NoIdentityFiles bool
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
				t.NoIdentityFiles = cfg.NoIdentityFiles
			}
			if cfg.HostName != "" {
				t.Host = cfg.HostName
			}
		}
	}
	if t.Port == 0 {
		t.Port = 22
	}
	if err := validTargetField(t.Host); err != nil {
		return Target{}, fmt.Errorf("bad ssh host %q: %w", t.Host, err)
	}
	if err := validTargetField(t.User); err != nil {
		return Target{}, fmt.Errorf("bad ssh user %q: %w", t.User, err)
	}
	return t, nil
}

// ParseTargets resolves every raw target, in order, keeping the first of any
// host named more than once and reporting the rest as duplicates. Attaching a
// host twice opens a second ssh connection, forwards the same remote ports
// onto a second set of local listeners, and lists that host's engines a second
// time under different local addresses: the UI keys rates by endpoint, so the
// header and chart totals would add one engine's tokens to themselves. Host
// case is folded for the same reason the host-key store folds it; the user and
// port are compared as written, so two accounts or two ports on one host stay
// two targets.
func ParseTargets(raws []string) (targets, duplicates []Target, err error) {
	seen := make(map[string]bool, len(raws))
	for _, raw := range raws {
		t, err := ParseTarget(raw)
		if err != nil {
			return nil, nil, err
		}
		key := foldHost(t.Host) + "\x00" + t.User + "\x00" + strconv.Itoa(t.Port) + "\x00" + t.KeyFile + "\x00" + strconv.FormatBool(t.NoIdentityFiles)
		if seen[key] {
			duplicates = append(duplicates, t)
			continue
		}
		seen[key] = true
		targets = append(targets, t)
	}
	return targets, duplicates, nil
}

func parseURLTarget(raw string) (Target, error) {
	if !strings.HasPrefix(raw, "ssh://") {
		// The shape is named, not the value: a bare user@host (the form a
		// ~/.ssh/config Host or a default login suggests) is a person, and
		// this line is the first thing the run prints.
		return Target{}, fmt.Errorf("target must start with ssh:// (got %q)", targetWithoutUser(raw))
	}
	u, err := url.Parse(raw)
	if err != nil {
		// url.Parse may echo userinfo, which is the password when one was
		// embedded; do not wrap that text into our error. The shape is named
		// instead, which is what the reader needs to fix the argument.
		return Target{}, errors.New("ssh target is not a URL toktop can parse (a space, quote or control character); expected ssh://[user@]host[:port]")
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
		if err != nil || t.Port <= 0 || t.Port > portMax {
			return Target{}, fmt.Errorf("bad ssh port %q", p)
		}
	}
	if t.Host == "" {
		return Target{}, fmt.Errorf("ssh target missing host")
	}
	return t, nil
}

// UserHost names the target the way the operator wrote it, minus anything
// the key path would add. It is what goes into the ssh argv and what the
// message on the operator's own terminal carries.
func (t Target) UserHost() string {
	if t.User == "" {
		return t.Host
	}
	return t.User + "@" + t.Host
}

// LogHost is UserHost without the account name, for an audit line. The line
// outlives the run and is the copy kept by whatever ran toktop, quoted into a
// bug report and read by whoever handles it, and a login names a person on
// the host rather than a port to check. logcfg.HomeHandler folds $HOME out of
// every line, and a user@host is not a path under it, so the account is
// dropped here instead. The host alone still tells a reader which target a
// line is about; the port beside it separates two of them on one host.
func (t Target) LogHost() string {
	return t.Host
}

// RedactUser folds the account name out of msg, for the error texts a dial
// prefixes with the target. The host survives, so the line still names the
// peer it is about, and a user the operator did not type (a ~/.ssh/config
// User, the local login name) is dropped with the rest.
func (t Target) RedactUser(msg string) string {
	if t.User == "" {
		return msg
	}
	return strings.ReplaceAll(msg, t.User+"@", "@")
}

// sshConfigEntry is the subset of ~/.ssh/config toktop understands.
type sshConfigEntry struct {
	User         string
	HostName     string
	Port         int
	IdentityFile string
	// NoIdentityFiles records an "IdentityFile none" line, the keyword
	// argument that turns the default identities off rather than naming one.
	NoIdentityFiles bool
}

// warnOncePerRun makes a configuration warning fire once for the process
// rather than once per ssh:// target that reads the same config file. Vars so
// a test can install its own and read the line it wrote.
var (
	warnSSHConfigInclude = sync.OnceFunc(func() {
		audit().Warn("toktop: ssh config uses Include; the included files are not read, so the values they define are not applied")
	})
	warnSSHConfigRelativeKey = sync.OnceFunc(func() {
		audit().Warn("toktop: ssh config IdentityFile is a relative path; it is resolved against the working directory, so the key it names depends on where toktop was started")
	})
)

// homeDir is the home directory the home-relative paths here are built from:
// ~/.ssh/config and the default identity files. It carries the absolute-only
// rule agentusage.HomeDir and defaultKnownHostsPath already apply: a relative
// $HOME would place both under the directory the run started in, where a
// missing config is not an error and a missing key is not either, so the
// connection would use the wrong port with the wrong identity and name
// neither. An unusable home names no path at all, and is said once so the
// cause is not a connect failure with nothing in it.
func homeDir() string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if !filepath.IsAbs(dir) {
		warnRelativeHome()
		return ""
	}
	return dir
}

var warnRelativeHome = sync.OnceFunc(func() {
	audit().Warn("toktop: the home directory is not an absolute path; ~/.ssh/config and the default ssh keys are not read")
})

var sshConfigPath = func() string {
	home := homeDir()
	if home == "" {
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
	// The first IdentityFile in the matching block decides, as OpenSSH's
	// first-obtained-value rule does: "none" is one of the values, so it has
	// to be remembered alongside the path form rather than as a path.
	identitySeen := false
	for line := range strings.SplitSeq(string(b), "\n") {
		key, val, ok := cutConfigField(line)
		if !ok {
			continue
		}
		switch core.FoldASCII(key) {
		case "host":
			inBlock = false
			negated := false
			// The pattern is folded the same way the name is. Folding only
			// the name left an accented Host pattern the macOS editor wrote
			// decomposed ("e" plus U+0301) compared against a composed name,
			// and the matcher compares runes, so the block never matched: its
			// HostName, User, Port and IdentityFile were all skipped and the
			// dial went to the default port with the default key.
			want := foldHost(name)
			for pat := range strings.FieldsSeq(val) {
				pat = foldHost(pat)
				if strings.HasPrefix(pat, "!") {
					if patternMatch(strings.TrimPrefix(pat, "!"), want) {
						negated = true
						break
					}
					continue
				}
				if patternMatch(pat, want) {
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
				p, err := strconv.Atoi(val)
				switch {
				case err != nil:
					// The port falls back to 22, so the failure surfaces as a
					// connection error on a port the operator never wrote.
					// Say which value was refused.
					audit().Warn("toktop: ssh config Port is not a number; using the default port 22",
						"port", core.RedactHome(core.Snippet([]byte(val))))
				case p <= 0 || p > portMax:
					audit().Warn("toktop: ssh config Port is out of range; using the default port 22",
						"port", core.RedactHome(core.Snippet([]byte(val))))
				default:
					entry.Port = p
				}
			}
		case "identityfile":
			if inBlock && !identitySeen {
				identitySeen = true
				if core.FoldASCII(val) == "none" {
					// The keyword argument ssh_config documents for "load no
					// identity files". Read as a path it is a file named "none",
					// which is missing, and a missing required key aborts the
					// whole auth chain: every connection to this host fails
					// with a key error naming a file the operator never wrote.
					entry.NoIdentityFiles = true
					break
				}
				if !filepath.IsAbs(core.ExpandHome(val)) {
					warnSSHConfigRelativeKey()
				}
				entry.IdentityFile = core.ExpandHome(val)
			}
		case "include":
			// Include names other files this reader does not open. The Host
			// block it defines is then absent from this run: the user, port,
			// hostname and key in it are not applied, and the connect fails or
			// lands on the wrong one with nothing naming the cause.
			warnSSHConfigInclude()
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
		return cutTrailingComment(s)
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

// cutTrailingComment drops an unquoted ssh_config comment. A '#' starts one
// only at the start of the argument or after whitespace, so a '#' inside a
// value (a path fragment, a token) stays part of it. Without this the comment
// rides along into User, which validTargetField then refuses, and into Port
// and IdentityFile, which then fail to parse and are silently lost.
func cutTrailingComment(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] != '#' {
			continue
		}
		if i > 0 && s[i-1] != ' ' && s[i-1] != '\t' {
			continue
		}
		return strings.TrimRight(s[:i], " \t")
	}
	return s
}

// validTargetField rejects a host or user that cannot be passed to ssh or
// printed. Whitespace and NUL break the ssh argv; a C0 control or DEL would
// be written to the operator's terminal by the first-use, forwarding-failure,
// and connection-lost messages, which is a terminal escape the operator
// never typed.
//
// Bidi controls, zero-width spaces, tag characters and variation selectors
// are refused as well. They are not C0, but they carry no part of a host
// name or a user name: they exist to render one string as another. A host
// spelled with a right-to-left override is written into the trust-on-first-use
// store and shown in the first-use prompt, the host label and the audit log
// as the host the operator believes it to be, while the bytes ssh dials are
// the ones on the command line. core.SanitizeText names the set, so the
// check and the sanitizer the rest of the tree renders with cannot drift.
func validTargetField(s string) error {
	// Empty is legal: an absent user means ssh's own default.
	if strings.ContainsAny(s, " \t\r\n\x00") {
		return errors.New("contains whitespace or newline")
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f {
			return errors.New("contains a control character")
		}
	}
	if core.SanitizeText(s) != s {
		return errors.New("contains a bidi control, zero-width character or other invisible formatting character")
	}
	return nil
}

// patternMatch matches an ssh_config Host pattern against a host name. The
// pattern language is exactly '*' (any run of characters) and '?' (one
// character), matched against the whole name, and nothing else.
//
// path.Match is not that language, and the difference decides which host
// toktop dials. Its '[' opens a character class and its '\' escapes, so
// `Host box[12]` would match box1 and box2, and `Host web\1` would match
// web1: a block the operator wrote for one machine would answer for two
// others, and the block's HostName, User, Port and IdentityFile would be
// applied to a host ssh itself would not have picked. The wrong host's
// IdentityFile is offered to it and the wrong host's key is pinned on
// first use, both silently.
//
// Every other byte in a pattern is a literal, including a '[', a '\' and
// a '*' that is not a wildcard, so only the two wildcards below are
// special.
//
// Both wildcards consume one character, not one byte: a host name is UTF-8,
// so `Host cafe?` has to match "café" rather than stop inside its last rune
// and report no match. The star's backtrack has to move by a character for
// the same reason. Advancing it by a byte slides the wildcard into the middle
// of a rune, where the byte compare reads a continuation byte (0x80-0xBF)
// that no ASCII pattern byte equals, so the star can never resume past the
// rune it is stuck in: `Host a*a*` reported no match for "aa*é", and an
// ssh_config block written for an accented host silently stopped applying,
// leaving the dial on the default port with the default key. On an ill-formed
// name there is no character to take, so the byte stands in and the match is
// decided on the bytes that arrived.
func patternMatch(pat, s string) bool {
	p := []rune(pat)
	t := []rune(s)
	pIdx, sIdx := 0, 0
	starIdx := -1
	sTmpIdx := -1

	for sIdx < len(t) {
		switch {
		case pIdx < len(p) && p[pIdx] == '?':
			pIdx++
			sIdx++
		case pIdx < len(p) && p[pIdx] == '*':
			starIdx = pIdx
			sTmpIdx = sIdx
			pIdx++
		case pIdx < len(p) && p[pIdx] == t[sIdx]:
			pIdx++
			sIdx++
		case starIdx != -1:
			pIdx = starIdx + 1
			sTmpIdx++
			sIdx = sTmpIdx
		default:
			return false
		}
	}

	for pIdx < len(p) && p[pIdx] == '*' {
		pIdx++
	}

	return pIdx == len(p)
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
	file = core.ExpandHome(strings.TrimSpace(file))
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

// targetWithoutUser drops the userinfo from a target spelled the way a bare
// host is ("user@host"), for the messages that name a mistyped argument. The
// account is dropped before the value is echoed, not after: a value printed
// whole reaches the operator's terminal and whatever captures it.
func targetWithoutUser(raw string) string {
	at := strings.Index(raw, "@")
	slash := strings.IndexAny(raw, `/\`)
	if at < 0 || (slash >= 0 && slash < at) {
		return raw
	}
	return raw[at+1:]
}
