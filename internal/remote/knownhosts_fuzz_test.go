package remote

import (
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/maci0/toktop/internal/core"
)

// FuzzParseKnownHosts throws arbitrary store bytes at parseKnownHosts, the
// parser behind the SSH host-key pin store. A corrupted or hand-edited store
// is read on every remote connect, and the two failure modes that matter are
// both silent: a store that parses to fewer pins than it holds re-trusts a
// host, and an error message that quotes an arbitrary tail reaches a
// terminal as attacker-shaped bytes. So the invariants are asserted rather
// than the parse: a store either reads whole or fails, a failure never
// returns pins alongside it, every returned record re-parses as an authorized
// key, and the error names the file inside a bounded, render-safe length.
// A store that does parse is written back and read again, so what the parser
// retained is what the writer kept.
func FuzzParseKnownHosts(f *testing.F) {
	good := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(fakePublicKey("good"))))
	other := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(fakePublicKey("other"))))
	for _, seed := range []string{
		"box.example:22 " + good + "\n",
		"# comment\n\nbox.example:22 " + good + "\nBox.Example:22 " + good + " trailing note\n",
		"box.example:22 " + good + " box.example:22 " + good + "\n",
		"box.example:22\n",
		"box.example:22 " + good + "\nbox.example:22 " + other + "\n",
		"box.example:22 ssh-ed25519\n",
		"box.example:22 ssh-ed25519 not-base64!!!\n",
		"",
		"\n\n\n# only comments\n",
		strings.Repeat("a", 4000) + " ssh-ed25519 AAAA\n",
		strings.Repeat("h:22 "+good+"\n", 200),
		"box.example:22 " + good + "\n\x1b]0;pwned\x07\n",
	} {
		f.Add([]byte(seed))
	}

	const storePath = "/store/known_hosts"
	f.Fuzz(func(t *testing.T, data []byte) {
		store, err := parseKnownHosts(storePath, data)
		if err != nil {
			// A store that does not parse must not read as a shorter one.
			if store != nil {
				t.Fatalf("error %v returned %d pins alongside it", err, len(store))
			}
			msg := err.Error()
			if !strings.Contains(msg, storePath) {
				t.Fatalf("error does not name the store: %q", msg)
			}
			if !utf8Safe(msg) {
				t.Fatalf("error carries non-renderable bytes: %q", msg)
			}
			// Every path quotes at most one line through the snippet cap, so
			// an error cannot grow with the size of the hostile input.
			if n := len([]rune(msg)); n > len(storePath)+2*core.SnippetCap+256 {
				t.Fatalf("error is %d characters for %d input bytes: %q", n, len(data), msg)
			}
			return
		}
		if len(store) == 0 {
			t.Fatalf("an empty store parsed without error: %q", data)
		}
		for key, record := range store {
			host, rest, ok := strings.Cut(record, " ")
			if !ok || strings.TrimSpace(rest) == "" {
				t.Fatalf("record %q has no key after the host", record)
			}
			if key != core.FoldASCII(host) {
				t.Fatalf("record %q indexed under %q, which is not its folded host", record, key)
			}
			if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(rest)); err != nil {
				t.Fatalf("record %q was retained but does not parse as a key: %v", record, err)
			}
		}
		// What the parser kept must survive the round trip through the
		// writer: pins are matched by key material on the next connect, so a
		// rewrite that changed a record would read as a changed host key.
		path := filepath.Join(t.TempDir(), "known_hosts")
		if err := writeKnownHosts(path, store); err != nil {
			t.Fatalf("writeKnownHosts: %v", err)
		}
		back, err := readKnownHosts(path)
		if err != nil {
			t.Fatalf("readKnownHosts after write: %v", err)
		}
		if !maps.EqualFunc(back, store, func(a, b string) bool { return pinKey(a) == pinKey(b) }) {
			t.Fatalf("store did not survive the write/read round trip:\n%v\n%v", store, back)
		}
	})
}

// utf8Safe reports whether s is valid UTF-8 carrying nothing a terminal acts
// on: the same guarantee core.SanitizeText gives the event feed.
func utf8Safe(s string) bool { return core.SanitizeText(s) == s }
