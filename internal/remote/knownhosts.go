package remote

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
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
	"github.com/maci0/toktop/internal/logcfg"
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

// storeMu serializes TOFU reads and writes inside this process, per store
// path. Handshake callbacks from one connection are sequential; the mutex
// covers concurrent connections and the file itself. It says nothing about
// other processes, which is what lockStore is for.
//
// The mutex is keyed by path so two stores cannot block each other, and it is
// taken inside lockStore rather than held across it: lockStore sleeps for up
// to storeLockWait waiting on a peer process, and a mutex held across that
// sleep would stall every concurrent handshake in the process, not just the
// one for the contended host. Holding it briefly around a read, as the
// trust-on-first-use probe does, costs no such wait.
var storeMu sync.Map // path -> *sync.Mutex

func storeMutex(path string) *sync.Mutex {
	if m, ok := storeMu.Load(path); ok {
		return m.(*sync.Mutex)
	}
	m, _ := storeMu.LoadOrStore(path, new(sync.Mutex))
	return m.(*sync.Mutex)
}

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
// lockStore runs fn with an exclusive lock beside path. A lock that cannot
// be released is reported rather than dropped: the next connect spends
// storeLockWait on it before breaking it as stale, and an operator who never
// learns why has no way to act. The result is named so the deferred release
// can fold its own failure into whatever fn returned.
func lockStore(path string, fn func() error) (err error) {
	lock := path + storeLockSuffix
	// The lock lives beside the store, and the store's directory is created by
	// the write inside fn. On a first remote connect the directory does not
	// exist yet, the exclusive create fails with ENOENT rather than EEXIST, and
	// the fallback below would run the read-modify-write with no lock at all:
	// two first contacts racing on a fresh install, the loser renaming away
	// the winner's pin, and the next connect re-trusting that host silently.
	// Create the directory first so the lock is always real.
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		return err
	}
	deadline := time.Now().Add(storeLockWait)
	for {
		f, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if cerr := f.Close(); cerr != nil {
				os.Remove(lock)
				return fmt.Errorf("%s: cannot write the lock: %w", lock, cerr)
			}
			defer func() {
				if rerr := os.Remove(lock); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
					err = errors.Join(err, fmt.Errorf("cannot release the lock at %s: %w", lock, rerr))
				}
			}()
			return fn()
		}
		if !os.IsExist(err) {
			// The directory is unwritable, or the filesystem has no exclusive
			// create. The write inside fn fails on its own with a clearer
			// error, so report that rather than a lock error the operator
			// cannot act on.
			return fn()
		}
		// A stale lock is only retried once the break actually took. A lock
		// that cannot be unlinked (read-only config dir, a peer recreating
		// it between the Stat and the Remove) would otherwise keep the stale
		// arm true and spin here with no sleep and no deadline check.
		if info, serr := os.Stat(lock); serr == nil && time.Since(info.ModTime()) > storeLockStale {
			if os.Remove(lock) == nil {
				continue
			}
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
	// A read-only probe takes the same per-path mutex the writer holds, so
	// it never observes this process half way through its own rewrite. It
	// waits for a rewrite in progress (a read and a rename) but never for
	// another process's lock: the store is replaced by rename, so a reader
	// sees either the old or the new file, never a partial one.
	mu := storeMutex(path)
	mu.Lock()
	_, err := readKnownHosts(path)
	mu.Unlock()
	if err != nil {
		return nil, err
	}
	// A store that survived only under one of the copies beside it is put
	// back on the next connect, so the copies are backups again rather than
	// where the store has permanently moved to. Best effort: readKnownHosts
	// reads them on every run either way, so a restore that cannot land costs
	// nothing but the tidiness, and a store that cannot be parsed at all is
	// the probe's error to report, not this one's to swallow.
	restoreStore(path)
	return func(hostname string, _ net.Addr, key ssh.PublicKey) error {
		if strings.ContainsAny(hostname, " \t\r\n\x00") {
			return fmt.Errorf("invalid hostname %q: contains whitespace or newline", hostname)
		}
		// The store is keyed case-insensitively, as DNS is: without this a
		// target spelled ssh://Box.example and the same host spelled
		// ssh://box.example are two entries, and the second connection
		// silently re-trusts a host the operator already pinned. The composed
		// spelling is the other half of the same key: an accented host typed
		// in the NFD form a macOS terminal supplies is one host, and
		// readKnownHosts folds it the same way.
		storeKey := foldHost(hostname)
		line := hostname + " " + string(ssh.MarshalAuthorizedKey(key))
		line = strings.TrimSpace(line)
		// The read and the write are one critical section, across processes
		// too: a store another toktop is rewriting under the read would make
		// this write resurrect the snapshot and drop whatever that process
		// had just pinned. lockStore serializes on the lock file; the mutex
		// is taken inside it so this process's wait for a peer never blocks
		// the file-lock wait of another host.
		return lockStore(path, func() error {
			mu.Lock()
			defer mu.Unlock()
			store, err := readKnownHosts(path)
			if err != nil {
				return err
			}
			if old, ok := store[storeKey]; ok {
				// Compare the key material, not the whole line: a pinned
				// host whose target changed case is the same pin, not a
				// changed key.
				if pinKey(old) == pinKey(line) {
					return nil
				}
				return fmt.Errorf(
					"host key for %s changed!\n  stored:    %s (%s)\n  presented: %s (%s)\nrefusing to connect; remove the stale line from %s if this host was rebuilt",
					hostname,
					short(old), fingerprintOf(old),
					short(line), fingerprintOf(line),
					path)
			}
			store[storeKey] = line
			if err := writeKnownHosts(path, store); err != nil {
				return err
			}
			// First contact is the one moment the operator can still judge the
			// key. OpenSSH says so; a silent trust means a fresh config dir, a
			// different account, or a container with no store accepts whatever
			// key is presented, with nothing in the output to notice.
			fmt.Fprintf(os.Stderr, "toktop: first use of %s, host key %s pinned to %s\n",
				hostname, short(line), fingerprintOf(line))
			return nil
		})
	}, nil
}

// readKnownHosts returns the pins the store holds, reading the store itself
// and, when it is not there, the two copies written beside it in turn:
//
//   - the displaced copy, which replaceFile leaves when a kill lands between
//     the two renames it makes on Windows;
//   - the backup copy, which every write refreshes, so a store lost, emptied
//     or overwritten by something else is read back rather than read as
//     "nothing pinned".
//
// Either way the pins are enforced, not merely returned: the caller compares
// them against the presented key. A store that is gone with neither copy
// beside it has nothing to recover, and the empty map it returns is the one
// honest reading left.
//
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
// A file that parses to no records at all is refused for the same reason: it
// is not a store anybody can have written, because every writer emits at
// least the host it just pinned. Zero length, or comments and blanks alone, is
// a store that lost its contents, and reading it as "nothing pinned yet"
// re-trusts every host on the next connect with nothing in the output.
//
// The same host repeated verbatim is a no-op, not an error: re-running a
// migration that concatenated the file must not brick the store.
func readKnownHosts(path string) (map[string]string, error) {
	for _, candidate := range []string{path, displacedPath(path), backupPath(path)} {
		b, err := os.ReadFile(candidate)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return parseKnownHosts(candidate, b)
	}
	return map[string]string{}, nil
}

// parseKnownHosts turns store bytes into pins, naming path in every error so
// a store that must be repaired by hand says which file it is looking at.
func parseKnownHosts(path string, b []byte) (map[string]string, error) {
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
			// The library's message quotes the blob it choked on, raw, so it
			// carries store bytes to a terminal on its own. It goes through
			// the same snippet as the line.
			return nil, malformedPin(path, n, line, core.Snippet([]byte(err.Error())))
		}
		record := host + " " + rest
		key := foldHost(host)
		if prev, dup := out[key]; dup && pinKey(prev) != pinKey(record) {
			return nil, fmt.Errorf("%s: host %s is recorded twice with different keys; refusing to pick one", path, core.Snippet([]byte(host)))
		}
		out[key] = record
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: holds no host records; an emptied or truncated store cannot re-trust these hosts, so restore it from a copy or delete it to pin them again on purpose", path)
	}
	return out, nil
}

// pinKey returns the key material of a stored record, which is everything
// after the host field, less any trailing comment the record is allowed to
// carry. Two records with the same pinKey are the same pin however their host
// was spelled; a comment is not part of the key, so a store written by another
// tool that annotates its lines is not read as a changed key.
func pinKey(record string) string {
	_, rest, ok := strings.Cut(record, " ")
	if !ok {
		return record
	}
	rest = cutTrailingComment(strings.TrimSpace(rest))
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(rest))
	if err != nil {
		return rest
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
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
	if err := replaceFile(tmpName, path); err != nil {
		return err
	}
	// The store is durable at this point, so a copy that does not land is a
	// warning rather than a failed write: the pin is pinned either way, and
	// reporting an error here would leave the operator with a pin to distrust.
	if err := writeBackup(path, b.String()); err != nil {
		audit().Warn("toktop: host key store backup not written",
			"path", logcfg.RedactedField(core.RedactHome(path), 256),
			"error", logcfg.RedactedField(err.Error(), 256))
	}
	return nil
}

// restoreStore rewrites a store that is only present under one of the copies
// beside it. It does nothing when the store is where it belongs, and it takes
// the cross-process lock rather than the bare in-process mutex, because a peer
// toktop may be mid-write: a restore that raced one would undo the pin that
// write had just recorded.
func restoreStore(path string) {
	// Only a store that is actually gone is worth a lock. Taking one on every
	// connect would make a dashboard pay a peer's full storeLockWait to learn
	// there is nothing to restore, which is a cost the connect cannot justify
	// for a state that is already correct.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		return
	}
	err := lockStore(path, func() error {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			return nil
		}
		mu := storeMutex(path)
		mu.Lock()
		defer mu.Unlock()
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			return nil
		}
		store, err := readKnownHosts(path)
		if err != nil || len(store) == 0 {
			return nil
		}
		audit().Warn("toktop: host key store was missing, pins recovered from its backup",
			"path", logcfg.RedactedField(core.RedactHome(path), 256))
		return writeKnownHosts(path, store)
	})
	if err != nil {
		audit().Warn("toktop: host key store not restored from its backup",
			"path", logcfg.RedactedField(core.RedactHome(path), 256),
			"error", logcfg.RedactedField(err.Error(), 256))
	}
}

// backupSuffix names the copy of the store kept beside it.
const backupSuffix = ".bak"

func backupPath(path string) string { return path + backupSuffix }

// writeBackup saves a copy of the store next to it, in the same atomic shape
// the store itself is written in, so the copy is never a half-written file
// either.
//
// It lands after the store, not before: a crash between the two leaves a
// store that is complete with a stale or absent copy, and readKnownHosts
// prefers the store, so nothing is lost. The other order would leave a copy
// newer than the store, and a later deletion of the store would hand back
// pins that were never actually enforced.
func writeBackup(path string, b string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, knownHostsTempPrefix+"*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once the rename succeeded
	}()
	if _, err := tmp.WriteString(b); err != nil {
		return err
	}
	// The copy names the same host keys, so it is as private as the store.
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, backupPath(path)); err != nil {
		return err
	}
	core.SyncDir(dir)
	return nil
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
// rename away. Callers hold the store's mutex, so within one process only the
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

// displacedSuffix names the copy replaceFile moves the old store to before
// renaming the new one in. readKnownHosts reads it back when the store itself
// is gone, so a kill between the two renames costs nothing.
const displacedSuffix = ".displaced"

func displacedPath(path string) string { return path + displacedSuffix }

// replaceFile renames tmpName over path.
//
// Unix rename replaces atomically and is tried first. Windows refuses to
// clobber an existing destination, so the store is renamed aside and put back
// if the replacement fails, rather than removed: a crash between the two
// renames then leaves the previous pins on disk under the displaced name
// instead of no store at all. Removing the destination outright would turn
// that window into a total loss of pins, which is a silent re-TOFU for every
// host the operator had ever connected to. readKnownHosts is what makes that
// leftover recoverable rather than merely present.
//
// Callers serialize writers (the store's mutex, taken inside lockStore).
func replaceFile(tmpName, path string) error {
	err := os.Rename(tmpName, path)
	if err == nil {
		core.SyncDir(filepath.Dir(path))
		return nil
	}
	displaced := displacedPath(path)
	if derr := os.Remove(displaced); derr != nil && !os.IsNotExist(derr) {
		// Returning err here would blame the destination rename for a
		// leftover this cleanup could not delete, which is a different
		// problem with a different fix.
		return fmt.Errorf("cannot clear leftover %s: %w", core.RedactHome(displaced), derr)
	}
	if derr := os.Rename(path, displaced); derr != nil && !os.IsNotExist(derr) {
		return fmt.Errorf("cannot move %s aside: %w", core.RedactHome(path), derr)
	}
	if rerr := os.Rename(tmpName, path); rerr != nil {
		if berr := os.Rename(displaced, path); berr != nil {
			return fmt.Errorf("%w (could not restore the previous store: %w)", rerr, berr)
		}
		return rerr
	}
	if rerr := os.Remove(displaced); rerr != nil && !os.IsNotExist(rerr) {
		// The new store is in place, so this is not a failed write and the
		// caller must not be told the write failed. It is not nothing
		// either: readKnownHosts falls back to the displaced copy whenever
		// the store itself is missing, so a leftover nobody could delete
		// would be handed back as the operator's pins on some later run.
		audit().Warn("toktop: known_hosts backup left behind",
			"path", logcfg.RedactedField(displaced, 256),
			"error", logcfg.RedactedField(rerr.Error(), 256))
	}
	core.SyncDir(filepath.Dir(path))
	return nil
}

// short names a stored pin by key type and base64 blob, the shape the change
// warning prints.
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
