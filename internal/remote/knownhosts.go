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
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/maci0/toktop/internal/core"
)

// knownHostsFile is the trust-on-first-use store. Overridable in tests.
//
// XDG_CONFIG_HOME is honored only when absolute. The XDG base-directory spec
// calls a relative value invalid, and honoring one would place the host-key
// pin store under whatever directory the run happens to start in: the store
// would vanish with the cwd, and a second run would re-TOFU. A relative value
// falls through to os.UserConfigDir, which rejects it by name, so the run
// fails at connect instead of quietly writing pins somewhere else.
var knownHostsPath = defaultKnownHostsPath

func defaultKnownHostsPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "toktop", "known_hosts")
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "toktop", "known_hosts")
}

// knownHostsMu serializes TOFU reads and writes inside this process. Handshake
// callbacks from one connection are sequential; the mutex covers concurrent
// connections and the file itself. It says nothing about other processes,
// which is what lockStore is for.
var knownHostsMu sync.Mutex

// The mutex only serializes this process. Two toktop processes (a dashboard
// and `toktop update`, or two dashboards) each snapshot the store, add a
// different host, and last-write the other away, losing a pin and forcing a
// re-TOFU on the next connect. The lock file below closes that window across
// processes: it is held for the whole read-modify-write, not just the write.
const (
	// storeLockSuffix names the lock file beside the store.
	storeLockSuffix = ".lock"
	// storeLockWait bounds how long a callback waits for another process to
	// finish its read-modify-write. The critical section is a read of a small
	// file and a rename, so this is generous; exceeding it means a peer died
	// holding the lock, which the stale check then breaks.
	storeLockWait = 5 * time.Second
	// storeLockPoll is the gap between attempts to take the lock.
	storeLockPoll = 20 * time.Millisecond
	// storeLockStale is how old a lock file has to be before it is assumed to
	// belong to a process that died mid-write and is broken. A live critical
	// section is a few milliseconds, so this is orders of magnitude clear of
	// it; without the break, one kill would wedge the store permanently.
	storeLockStale = time.Minute
)

// lockStore takes the cross-process store lock, runs fn, and releases it. The
// lock is a file created exclusively and removed on release: creation is
// atomic on every filesystem toktop runs on, which a flock over the store
// itself is not on Windows. A lock left by a dead process is broken once it
// is older than storeLockStale, so the store cannot wedge.
func lockStore(path string, fn func() error) error {
	lock := path + storeLockSuffix
	deadline := time.Now().Add(storeLockWait)
	for {
		f, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			f.Close()
			defer os.Remove(lock)
			return fn()
		}
		if !os.IsExist(err) {
			// The directory is unwritable, or the filesystem has no exclusive
			// create. The write inside fn fails on its own with a clearer
			// error, so report that rather than a lock error the operator
			// cannot act on.
			return fn()
		}
		if info, serr := os.Stat(lock); serr == nil && time.Since(info.ModTime()) > storeLockStale {
			_ = os.Remove(lock)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s is locked by another toktop; giving up after %s", path, storeLockWait)
		}
		time.Sleep(storeLockPoll)
	}
}

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
	knownHostsMu.Lock()
	_, err := readKnownHosts(path)
	knownHostsMu.Unlock()
	if err != nil {
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
		// The read and the write are one critical section, across processes
		// too: a store another toktop is rewriting under the read would make
		// this write resurrect the snapshot and drop whatever that process
		// had just pinned.
		return lockStore(path, func() error {
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
		})
	}, nil
}

// A record is only a pin if its key parses. Lines that are blank or comments
// are skipped; anything else must be `host key-type base64`, optionally with a
// trailing comment, and must parse as an authorized key.
//
// Two shapes are refused rather than skipped, because skipping either one
// silently drops a pin and the next connection re-TOFUs the host, which is
// exactly what the store exists to prevent:
//
//   - A malformed record (a truncated last line from a hand edit or a
//     filesystem that lost the tail). Reading it as an empty store turns
//     corruption into a forced re-trust.
//   - A host recorded twice with different keys. The map would keep the last
//     one, so appending a line to the file overrides an existing pin without
//     rewriting it, and the original pin is gone with no trace.
//
// The same host repeated verbatim is a no-op, not an error: re-running a
// migration that concatenated the file must not brick the store.
func readKnownHosts(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for n, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		host, rest, ok := strings.Cut(line, " ")
		if !ok {
			return nil, malformedPin(path, n, line, "no key after the host")
		}
		rest = strings.TrimSpace(rest)
		// An older writer emitted the host twice on one line. The second copy
		// is dropped before parsing, so such a record still reads as a pin.
		rest = strings.TrimSpace(strings.TrimPrefix(rest, host+" "))
		if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(rest)); err != nil {
			return nil, malformedPin(path, n, line, err.Error())
		}
		record := host + " " + rest
		if prev, dup := out[host]; dup && prev != record {
			return nil, fmt.Errorf("%s: host %s is recorded twice with different keys; refusing to pick one", path, host)
		}
		out[host] = record
	}
	return out, nil
}

// malformedPin names the file and the line so a store that must be repaired
// by hand says which one. The line is quoted through the snippet cap: a
// truncated record can be an arbitrary tail, and it is attacker-shaped input
// reaching a terminal.
func malformedPin(path string, n int, line, why string) error {
	return fmt.Errorf("%s: line %d is not a valid host record (%s): %s", path, n+1, why, core.Snippet([]byte(line)))
}

func writeKnownHosts(path string, store map[string]string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var b strings.Builder
	for _, host := range slices.Sorted(maps.Keys(store)) {
		b.WriteString(store[host] + "\n")
	}
	sweepStaleTempFiles(dir)
	tmp, err := os.CreateTemp(dir, knownHostsTempPrefix+"*")
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

// knownHostsTempPrefix names the staging file the store is written to before
// it is renamed into place.
const knownHostsTempPrefix = ".known_hosts-"

// staleTempAge is how old a leftover staging file has to be before the next
// write removes it. A kill between CreateTemp and the rename leaves one in
// the config directory forever, since nothing else ever looks for it. The age
// gate keeps the sweep from deleting a store another toktop is writing.
const staleTempAge = 24 * time.Hour

// sweepStaleTempFiles removes staging files an earlier write did not get to
// rename away. Callers hold knownHostsMu, so within one process only the
// crashed runs of earlier sessions are ever this old. Anything it cannot
// remove is left alone.
func sweepStaleTempFiles(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleTempAge)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), knownHostsTempPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// replaceFile renames tmpName over path.
//
// Unix rename replaces atomically and is tried first. Windows refuses to
// clobber an existing destination, so the store is renamed aside and put back
// if the replacement fails, rather than removed: a crash between the two
// renames then leaves the previous pins on disk under the displaced name
// instead of no store at all. Removing the destination outright would turn
// that window into a total loss of pins, which is a silent re-TOFU for every
// host the operator had ever connected to.
//
// Callers serialize writers (knownHostsMu).
func replaceFile(tmpName, path string) error {
	err := os.Rename(tmpName, path)
	if err == nil {
		syncDir(filepath.Dir(path))
		return nil
	}
	displaced := path + ".displaced"
	_ = os.Remove(displaced) // a leftover from a run that died mid-replace
	if derr := os.Rename(path, displaced); derr != nil && !os.IsNotExist(derr) {
		return err
	}
	if rerr := os.Rename(tmpName, path); rerr != nil {
		if berr := os.Rename(displaced, path); berr != nil {
			return fmt.Errorf("%w (could not restore the previous store: %w)", rerr, berr)
		}
		return rerr
	}
	_ = os.Remove(displaced)
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir flushes the directory entry a rename created. The file contents are
// already fsynced, but without this the rename itself can be lost to a crash,
// which would silently restore the previous store and drop the pin just
// written. Best effort by design: a directory that cannot be opened or synced
// (Windows, some network filesystems) leaves the file whole either way, and
// failing the write over it would cost the operator the pin instead.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
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
