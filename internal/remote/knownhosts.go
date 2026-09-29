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
	"github.com/maci0/toktop/internal/lockfile"
	"github.com/maci0/toktop/internal/logcfg"
)

// XDGConfigHomeEnv is the XDG base directory the host-key store lives under.
// Exported so the startup warning that names a value it cannot use spells it
// the way defaultKnownHostsPath reads it, the way PasswordEnv is shared with
// the top-level command.
const XDGConfigHomeEnv = "XDG_CONFIG_HOME"

// knownHostsFile is the trust-on-first-use store. Overridable in tests.
//
// XDGConfigHomeEnv is honored only when absolute. The XDG base-directory spec
// calls a relative value invalid, and honoring one would place the host-key
// pin store under whatever directory the run happens to start in: the store
// would vanish with the cwd, and a second run would re-TOFU. A relative value
// falls through to os.UserConfigDir, which rejects it by name, so the run
// fails at connect instead of quietly writing pins somewhere else.
//
// The fallback gets the same check. os.UserConfigDir builds its answer from
// $HOME when XDGConfigHomeEnv is unset, and a relative $HOME yields a relative
// config directory, which is the store that vanishes with the cwd all over
// again. An unusable home therefore names no store, and the run fails at
// connect rather than writing pins under the working directory.
var knownHostsPath = defaultKnownHostsPath

func defaultKnownHostsPath() string {
	if dir := os.Getenv(XDGConfigHomeEnv); filepath.IsAbs(dir) {
		return filepath.Join(dir, "toktop", "known_hosts")
	}
	dir, err := os.UserConfigDir()
	if err != nil || !filepath.IsAbs(dir) {
		return ""
	}
	return filepath.Join(dir, "toktop", "known_hosts")
}

// HostKeyStorePath is the store the next ssh connect reads and writes, or ""
// when this environment places none. Exported so the startup warning in
// cmd/toktop resolves the store the same way the connect does, rather than
// describing a fallback the reader does not take.
func HostKeyStorePath() string { return knownHostsPath() }

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

// lockStore runs fn with the cross-process lock held beside path, so a peer
// process cannot take the store away mid read-modify-write. A lock left by a
// dead process is broken once it is older than storeLockStale, so the store
// cannot wedge.
func lockStore(path string, fn func() error) error {
	lock := path + storeLockSuffix
	// The lock lives beside the store, and the store's directory is created by
	// the write inside fn. On a first remote connect the directory does not
	// exist yet, the exclusive create fails with ENOENT rather than EEXIST, and
	// lockfile.With would run the read-modify-write with no lock at all: two
	// first contacts racing on a fresh install, the loser renaming away the
	// winner's pin, and the next connect re-trusting that host silently.
	// Create the directory first so the lock is always real.
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		return err
	}
	return lockfile.With(lock, path, lockfile.Policy{
		Wait:  storeLockWait,
		Poll:  storeLockPoll,
		Stale: storeLockStale,
	}, lockfile.Raw, fn)
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
	var err error
	func() {
		mu.Lock()
		defer mu.Unlock()
		_, err = readKnownHosts(path)
	}()
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
	checkStoreCopy(path)
	clearSupersededCopy(path)
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
// and, when it is not there *because a write was interrupted*, the two copies
// written beside it in turn:
//
//   - the backup copy, which every write refreshes, so a store lost, emptied
//     or overwritten by something else is read back rather than read as
//     "nothing pinned";
//   - the displaced copy, which replaceFile leaves when a kill lands between
//     the two renames it makes on Windows.
//
// The copies are consulted only where interruptedWrite finds the marks of a
// write that did not finish. A store that is simply gone is a different state:
// deleting it is how an operator re-pins a host on purpose, after a key change
// they have checked, and handing back the backup would silently undo that and
// pin them to the very key they just rejected. The backup exists on every
// write, so its presence alone proves nothing; a leftover staging file or a
// displaced copy is the evidence that this store went missing mid-write rather
// than by hand.
//
// The two copies are read freshest first (copiesByRecency), so a store
// recovered from whichever one was written last does not lose the pins the
// other had.
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
//
// A file that does not parse is still refused rather than replaced by a copy
// that does: a copy predating the last write is missing pins, and reading it
// in place of a damaged store would re-trust every host the missing pins
// covered. The error names a copy that parses, because a damaged store is the
// one loss the operator repairs by copying a file back, and naming the
// unbroken one is what turns the refusal into a repair.
func readKnownHosts(path string) (map[string]string, error) {
	candidates := []string{path}
	if interruptedWrite(path) {
		candidates = append(candidates, copiesByRecency(path)...)
	}
	for _, candidate := range candidates {
		b, err := os.ReadFile(candidate)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		store, err := parseKnownHosts(candidate, b)
		if err != nil {
			return nil, withRecoveryHint(err, path)
		}
		return store, nil
	}
	return map[string]string{}, nil
}

// withRecoveryHint appends the copy of the store that still parses to a read
// that refused the store, and the one command that puts it back. The error is
// wrapped rather than replaced, so the record that could not be read stays the
// part an operator reads first.
func withRecoveryHint(err error, path string) error {
	copy, ok := freshestParsedCopy(path)
	if !ok {
		return err
	}
	return fmt.Errorf("%w; %s still parses, so the pins this file held can be restored with: %s",
		err, copy, restoreCommand(copy, path))
}

// freshestParsedCopy returns the copy beside the store that a read would
// accept, in the order a read would try them, and false when none of them
// parses. A copy that is missing, unreadable, or damaged is not named: the
// hint is a command an operator is meant to run, and one that restores a file
// toktop would then refuse is worse than no hint.
func freshestParsedCopy(path string) (string, bool) {
	for _, candidate := range copiesByRecency(path) {
		b, rerr := os.ReadFile(candidate)
		if rerr != nil {
			continue
		}
		if _, perr := parseKnownHosts(candidate, b); perr == nil {
			return candidate, true
		}
	}
	return "", false
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

// copiesByRecency returns the copies beside the store, freshest first.
//
// A store recovered from a copy costs a pin for every host that copy predates,
// so the copy carrying the most pins is the one to read, and a fixed order gets
// that wrong. The backup is normally the fresher file, being the content of the
// last write, while the displaced copy is whatever replaceFile moved aside,
// which is the content before it. mtime decides rather than that assumption,
// because the case where the assumption fails is the one that costs pins: a
// write whose backup could not land leaves a backup older than the displaced
// copy, and reading the backup hands back the store as it was before the
// failure. A copy that cannot be dated sorts last, and the backup stays ahead of
// the displaced copy on a tie, which is the order a store written without a
// kill prefers.
func copiesByRecency(path string) []string {
	type copy struct {
		path string
		at   time.Time
	}
	copies := []copy{{path: backupPath(path)}, {path: displacedPath(path)}}
	for i := range copies {
		if info, err := os.Stat(copies[i].path); err == nil {
			copies[i].at = info.ModTime()
		}
	}
	slices.SortStableFunc(copies, func(a, b copy) int { return b.at.Compare(a.at) })
	paths := make([]string, len(copies))
	for i, c := range copies {
		paths[i] = c.path
	}
	return paths
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

// atomicWriteFile writes contents to path through a temp file in the same
// directory, so a reader never sees a half-written store. The temp file is
// owner-only from the moment it is created, and [core.DiscardStaged] removes it
// unless the rename landed.
func atomicWriteFile(path, contents string) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), knownHostsTempPrefix+"*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		err = core.DiscardStaged(tmpName, err)
	}()
	if _, err := tmp.WriteString(contents); err != nil {
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

func writeKnownHosts(path string, store map[string]string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var b strings.Builder
	for _, host := range slices.Sorted(maps.Keys(store)) {
		b.WriteString(store[host] + "\n")
	}
	core.SweepStaleTemps(dir, knownHostsTempPrefix, time.Now())
	if err := atomicWriteFile(path, b.String()); err != nil {
		return err
	}
	// The store is durable at this point, so a copy that does not land is a
	// warning rather than a failed write: the pin is pinned either way, and
	// reporting an error here would leave the operator with a pin to distrust.
	if err := writeBackup(path, b.String()); err != nil {
		audit().Warn("toktop: host key store backup not written",
			"path", logcfg.RedactedField(core.RedactHome(path), logcfg.FieldCap),
			"error", logcfg.RedactedField(err.Error(), logcfg.FieldCap))
	}
	return nil
}

// interruptedWrite reports whether the store is missing because a write did
// not finish, which is the only state where the copies beside it stand in for
// it. A leftover staging file is a write that died between creating the
// temporary and renaming it over the store; a leftover displaced copy is a
// Windows replaceFile that died between its two renames. Either one is
// evidence, and neither can be produced by an operator removing the file, so
// a store deleted on purpose reads as empty and re-pins on the next connect.
func interruptedWrite(path string) bool {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return false
	}
	displaced := filepath.Base(displacedPath(path))
	for _, e := range entries {
		if !e.IsDir() && interruptedMark(e.Name(), displaced) {
			return true
		}
	}
	return false
}

// interruptedMark reports whether a file beside the store is one of the marks
// a write that did not finish leaves. It is the one definition of the set, so
// the marks clearInterruptedWrite removes cannot drift from the ones that
// make a copy stand in for the store: a mark the check ignores and the clear
// removes leaves a loss nothing will ever read, and a mark the clear leaves
// and the check counts outlives the recovery that spent it.
func interruptedMark(name, displaced string) bool {
	return strings.HasPrefix(name, knownHostsTempPrefix) || name == displaced
}

// clearInterruptedWrite removes the marks a killed write left beside a store
// that has just been recovered: the staging files it created and never
// renamed, and the copy replaceFile moved aside and never replaced.
//
// The evidence is spent by the restore that acted on it. The store is back and
// carries every pin, and writeKnownHosts has left a refreshed copy beside it,
// so nothing here is the last record of anything. Left in place, a mark keeps
// answering for a loss already repaired, and the re-pin gesture stops being
// one: deleting the store to accept a host's new key finds a mark beside it,
// and the copy is handed back with the rejected key in it. A mark outlives
// its repair until the next write sweeps it, which is StaleTempAge away at the
// earliest.
//
// Callers hold the store lock, which no writer of the store can be inside, so
// every mark found here belongs to a write that is already dead.
func clearInterruptedWrite(path string) {
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	displaced := filepath.Base(displacedPath(path))
	for _, e := range entries {
		if e.IsDir() || !interruptedMark(e.Name(), displaced) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
	core.SyncDir(dir)
}

// restoreStore rewrites a store that an interrupted write left present only
// under one of the copies beside it. It does nothing when the store is where it
// belongs, when its absence is deliberate, or when a write left no staging
// marks (interruptedWrite), and it takes the cross-process lock rather than
// the bare in-process mutex, because a peer toktop may be mid-write: a restore
// that raced one would undo the pin that write had just recorded.
//
// A restore spends the marks that justified it (clearInterruptedWrite): once
// the store is back, they are the evidence of a loss that no longer happened,
// and leaving them would turn the next deletion into a second, unwanted one.
func restoreStore(path string) {
	// Only a store that is actually gone is worth a lock. Taking one on every
	// connect would make a dashboard pay a peer's full storeLockWait to learn
	// there is nothing to restore, which is a cost the connect cannot justify
	// for a state that is already correct.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		return
	}
	// A store an operator deleted is a re-pin request, not a loss to repair:
	// writing the backup back here would re-pin the key they just rejected.
	if !interruptedWrite(path) {
		warnDeletedStore(path)
		return
	}
	err := lockStore(path, func() error {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			return nil
		}
		if !interruptedWrite(path) {
			return nil
		}
		mu := storeMutex(path)
		mu.Lock()
		defer mu.Unlock()
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			return nil
		}
		store, err := readKnownHosts(path)
		if err != nil {
			return err
		}
		if len(store) == 0 {
			audit().Warn("toktop: host key store was missing and its backup holds no pins",
				"path", logcfg.RedactedField(core.RedactHome(path), logcfg.FieldCap))
			return nil
		}
		audit().Warn("toktop: host key store was missing, pins recovered from its backup",
			"path", logcfg.RedactedField(core.RedactHome(path), logcfg.FieldCap))
		if err := writeKnownHosts(path, store); err != nil {
			return err
		}
		clearInterruptedWrite(path)
		return nil
	})
	if err != nil {
		audit().Warn("toktop: host key store not restored from its backup",
			"path", logcfg.RedactedField(core.RedactHome(path), logcfg.FieldCap),
			"error", logcfg.RedactedField(err.Error(), logcfg.FieldCap))
	}
}

// warnDeletedStore reports a store that is gone while a copy of it still
// parses beside it.
//
// The copy is not read back. An operator who removed the store to accept a
// host's new key must not have that key handed to them again by the copy, and
// the absence of the store is the whole signal that they meant it. What the
// warning carries is what the removal cost, and the command that undoes it if
// the removal was not theirs.
//
// Without it the deletion is silent. Nothing else logs a store that is simply
// absent, because absent is the re-pin gesture, so an accidental removal (a
// config reset, a cleanup script, a sync that dropped it) is first reported by
// the next connect trusting a host the operator had already pinned, which is
// the interception the store exists to refuse. The copy sitting beside the
// missing store is the only evidence left of what was lost, and it is evidence
// nobody would otherwise look at.
//
// Nothing is said when no copy parses: there is nothing to restore, and a
// first run on a host with no store yet is not a loss.
func warnDeletedStore(path string) {
	copy, ok := freshestParsedCopy(path)
	if !ok {
		return
	}
	audit().Warn("toktop: host key store was removed, so every pin it held is dropped",
		"path", logcfg.RedactedField(core.RedactHome(path), logcfg.FieldCap),
		"copy", logcfg.RedactedField(core.RedactHome(copy), logcfg.FieldCap),
		"restore", logcfg.RedactedField(core.RedactHome(restoreCommand(copy, path)), logcfg.FieldCap))
}

// checkStoreCopy re-creates the copy the store is recovered from when that
// copy is missing, damaged, or older than the store, and reports it.
//
// A copy that could not be written is warned about once, at the write, by
// writeKnownHosts. That is the only word the operator gets: a run that never
// connects again leaves the store with no copy beside it and nothing further
// said about it, so the store silently becomes the only record of every pin
// on the host. The gap is a state rather than a moment, and the next connect
// is where it can be seen and closed.
//
// The store is the source, never a copy: it is the file readKnownHosts parsed
// on this connect, so the pins copied are the pins in force, and copying it
// is the one write that cannot lower the RPO. A copy that parses and is not
// older than the store is left alone, which is the case every healthy run
// takes and the only one that does no work.
func checkStoreCopy(path string) {
	why := staleStoreCopy(path)
	if why == "" {
		return
	}
	rebuilt := false
	err := lockStore(path, func() error {
		// Re-read the state under the lock: a peer toktop that rewrote the
		// store and its copy between the check above and this write has
		// already closed the gap, and saying so was not this run's finding.
		if staleStoreCopy(path) == "" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// A store that does not parse is readKnownHosts's error, and the
		// connect has already failed by the time this runs, so there is
		// nothing here to copy.
		if _, err := parseKnownHosts(path, b); err != nil {
			return nil
		}
		rebuilt = true
		return writeBackup(path, string(b))
	})
	if err != nil {
		audit().Warn("toktop: host key store backup not rewritten from the store",
			"path", logcfg.RedactedField(core.RedactHome(path), logcfg.FieldCap),
			"error", logcfg.RedactedField(err.Error(), logcfg.FieldCap))
		return
	}
	if rebuilt {
		audit().Warn("toktop: host key store backup rewritten from the store",
			"path", logcfg.RedactedField(core.RedactHome(path), logcfg.FieldCap),
			"reason", why)
	}
}

// clearSupersededCopy removes a displaced copy whose replacement landed, which
// is the leftover replaceFile warned about when its removal failed.
//
// Left beside a store that is there, the copy is evidence of nothing: it holds
// the pins from before a write the store already carries, so it cannot recover
// a loss the store did not have. Left in place, though, its name keeps
// answering interruptedWrite, and a store the operator later deletes on purpose
// reads as a write that died rather than as the re-pin it is. That reads the
// copy back and hands over the key the deletion was meant to reject, and it
// does so for as long as the leftover survives: nothing else removes it, since
// the write that could not already happened.
//
// So every connect tries again, once the store is known to parse. A store that
// is missing, or one that does not parse, keeps the copy: there it is the
// fallback readKnownHosts would recover the store from, and a copy nobody could
// delete is reported rather than retried in silence.
func clearSupersededCopy(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if _, err := parseKnownHosts(path, b); err != nil {
		return
	}
	displaced := displacedPath(path)
	if err := os.Remove(displaced); err != nil {
		if !os.IsNotExist(err) {
			audit().Warn("toktop: superseded host key copy beside the store not removed",
				"path", logcfg.RedactedField(core.RedactHome(displaced), logcfg.FieldCap),
				"error", logcfg.RedactedField(err.Error(), logcfg.FieldCap))
		}
		return
	}
	core.SyncDir(filepath.Dir(path))
}

// staleStoreCopy returns why the copy beside the store cannot recover it, and
// an empty string when it can: missing, unreadable, not parsing, or older
// than the store are all a store whose pins survive nothing.
//
// A store that is not there is not a stale copy: it is what restoreStore is
// for, and there is nothing to copy from until it has been put back. The
// backup copy is the one checked, since every write of the store refreshes it.
// The displaced copy is not: only a Windows write leaves one, it is a
// leftover of the replacement rather than a copy the store is written, and
// warning about an absent one would fire on every platform but that one.
func staleStoreCopy(path string) string {
	store, err := os.Stat(path)
	if err != nil {
		return ""
	}
	bak, err := os.Stat(backupPath(path))
	if err != nil {
		if os.IsNotExist(err) {
			return "the copy beside it is missing"
		}
		return "the copy beside it cannot be read"
	}
	if bak.ModTime().Before(store.ModTime()) {
		return "the copy beside it is older than the store"
	}
	b, err := os.ReadFile(backupPath(path))
	if err != nil {
		return "the copy beside it cannot be read"
	}
	if _, err := parseKnownHosts(backupPath(path), b); err != nil {
		return "the copy beside it does not parse"
	}
	return ""
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
	// The copy names the same host keys, so it is as private as the store.
	return atomicWriteFile(backupPath(path), b)
}

// knownHostsTempPrefix names the staging file the store is written to before
// it is renamed into place.
const knownHostsTempPrefix = ".known_hosts-"

// displacedSuffix names the copy replaceFile moves the old store to before
// renaming the new one in. readKnownHosts reads it back when the store itself
// is gone, so a kill between the two renames costs nothing.
const displacedSuffix = ".displaced"

func displacedPath(path string) string { return path + displacedSuffix }

// replaceFile renames tmpName over path.
//
// Rename replaces the destination atomically and is tried first, on Windows
// as much as on Unix: os.Rename is MoveFileEx with MOVEFILE_REPLACE_EXISTING
// there. What it cannot do on Windows is replace a destination another
// process holds open, which an indexer or an antivirus scanner does to a
// config directory, so the store is renamed aside and put back when the
// replacement fails, rather than removed: a crash between the two renames
// then leaves the previous pins on disk under the displaced name instead of
// no store at all. Removing the destination outright would turn that window
// into a total loss of pins, which is a silent re-TOFU for every host the
// operator had ever connected to. readKnownHosts is what makes that leftover
// recoverable rather than merely present.
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
			"path", logcfg.RedactedField(core.RedactHome(displaced), logcfg.FieldCap),
			"error", logcfg.RedactedField(rerr.Error(), logcfg.FieldCap))
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
