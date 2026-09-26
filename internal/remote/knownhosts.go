package remote

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// knownHostsFile is the trust-on-first-use store. Overridable in tests.
var knownHostsPath = func() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "toktop", "known_hosts")
}

// knownHostsMu serializes TOFU reads and writes. Two Connects racing would
// otherwise each snapshot the file, add a different host, and last-write
// the other away. Handshake callbacks from one connection are sequential;
// the lock covers concurrent connections and the file itself.
var knownHostsMu sync.Mutex

// tofu returns a HostKeyCallback implementing trust-on-first-use against a
// small local store: first contact is remembered, changed keys are refused
// with both fingerprints so the operator can judge.
func tofu() (ssh.HostKeyCallback, error) {
	path := knownHostsPath()
	if path == "" {
		return nil, fmt.Errorf("cannot resolve config dir for host key store")
	}
	// Fail at Connect, not mid-handshake, if the store is unreadable.
	// A missing file is fine; the callback creates it on first contact.
	if _, err := readKnownHosts(path); err != nil {
		return nil, err
	}
	return func(hostname string, _ net.Addr, key ssh.PublicKey) error {
		if strings.ContainsAny(hostname, " \t\r\n\x00") {
			return fmt.Errorf("invalid hostname %q: contains whitespace or newline", hostname)
		}
		line := hostname + " " + string(ssh.MarshalAuthorizedKey(key))
		line = strings.TrimSpace(line)
		knownHostsMu.Lock()
		defer knownHostsMu.Unlock()
		store, err := readKnownHosts(path)
		if err != nil {
			return err
		}
		if old, ok := store[hostname]; ok {
			if old == line {
				return nil
			}
			return fmt.Errorf(
				"host key for %s changed!\n  stored:    %s (%s)\n  presented: %s (%s)\nrefusing to connect; remove the stale line from %s if this host was rebuilt",
				hostname,
				short(old), fingerprintOf(old),
				short(line), fingerprintOf(line),
				path)
		}
		store[hostname] = line
		return writeKnownHosts(path, store)
	}, nil
}

func readKnownHosts(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if host, rest, ok := strings.Cut(line, " "); ok && rest != "" {
			rest = strings.TrimSpace(rest)
			rest = strings.TrimPrefix(rest, host+" ")
			out[host] = host + " " + strings.TrimSpace(rest)
		}
	}
	return out, nil
}

func writeKnownHosts(path string, store map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	for _, host := range slices.Sorted(maps.Keys(store)) {
		b.WriteString(store[host] + "\n")
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".known_hosts-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once the rename succeeded
	}()
	if _, err := tmp.WriteString(b.String()); err != nil {
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return replaceFile(tmpName, path)
}

// replaceFile renames tmpName over path. Unix rename replaces atomically;
// Windows refuses to clobber, so the destination is removed first. Callers
// serialize writers (knownHostsMu).
func replaceFile(tmpName, path string) error {
	if err := os.Rename(tmpName, path); err == nil {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(tmpName, path)
}

func short(s string) string {
	fields := strings.Fields(s)
	if len(fields) > 2 {
		return fields[2]
	}
	if len(fields) > 1 {
		return fields[len(fields)-1]
	}
	return s
}

// fingerprintOf renders the stored key's SHA-256 fingerprint; unknown shapes
// degrade to a short hash of the raw text. The line is hostname plus an
// authorized-keys blob (any key type): parsing it as written is what
// ssh-keygen -lf would show, so a changed RSA or ECDSA key is comparable
// instead of a hash of the raw line.
func fingerprintOf(line string) string {
	_, rest, ok := strings.Cut(strings.TrimSpace(line), " ")
	if ok {
		if k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(rest)); err == nil {
			return ssh.FingerprintSHA256(k)
		}
	}
	sum := sha256.Sum256([]byte(line))
	return base64.RawStdEncoding.EncodeToString(sum[:8])
}
