// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// Apply downloads, verifies, and installs the release over the running
// executable. It returns the path that was replaced.
//
// The new binary is written next to the current one (same filesystem, so the
// rename is atomic) and only renamed after its checksum matches. A failed
// verification leaves the running binary untouched. If the destination
// already matches the release checksum, the asset is not fetched or replaced.
func Apply(ctx context.Context, rel *Release) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return "", err
	}
	return applyTo(ctx, rel, self)
}

// applyTo is Apply with an explicit target, so the install path can be tested
// without replacing the test binary. The results are named so the staging
// file's cleanup can fold its own failure into the error being returned.
func applyTo(ctx context.Context, rel *Release, self string) (installed string, err error) {
	// Recovery before the network. A killed update can leave the binary under
	// the displaced name with nothing at the installed path, and a download
	// that then fails (offline, rate-limited, checksum mismatch) would leave
	// it that way. Restoring first is a no-op unless the installed path is
	// missing.
	if err := restoreDisplaced(self, self+displacedSuffix); err != nil {
		return "", err
	}
	want := AssetName(rel.Version())
	sumsFile := checksumsName(rel.Version())
	assetURL, sumsURL := releaseAssets(rel)
	if assetURL == "" {
		return "", fmt.Errorf("release %s has no asset %s", rel.TagName, want)
	}
	if sumsURL == "" {
		return "", fmt.Errorf("release %s has no %s; refusing to install unverified binary", rel.TagName, sumsFile)
	}
	if !trustedAssetURL(assetURL) || !trustedAssetURL(sumsURL) {
		return "", fmt.Errorf("release %s asset URL is not a GitHub download", rel.TagName)
	}

	var archive bytes.Buffer
	if _, err := fetch(ctx, sumsURL, &archive, maxChecksumsArchive); err != nil {
		return "", fmt.Errorf("cannot fetch checksums: %w", err)
	}
	sums, err := ChecksumListing(archive.Bytes())
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", sumsFile, err)
	}
	expect, ok := ChecksumFor(sums, want)
	if !ok {
		return "", fmt.Errorf("checksums.txt has no entry for %s", want)
	}
	// A binary toktop cannot read is not an up-to-date binary: reporting the
	// read failure names the permission or path problem instead of spending a
	// download on an install that cannot replace the same file anyway.
	have, cerr := fileChecksum(self)
	if cerr != nil {
		if !errors.Is(cerr, fs.ErrNotExist) {
			return "", fmt.Errorf("cannot checksum %s: %w", self, cerr)
		}
	} else if have == expect {
		// Already this release, so nothing is downloaded or installed. The
		// leftover .old a killed or locked install leaves is still cleared
		// here, because install is the only other place that removes it and
		// this path never reaches install. Only on the platform whose install
		// displaces: the file is written by installDisplacing, which only
		// Windows reaches, so clearing it everywhere else removes a path
		// beside the binary that toktop never created and never owned. Best
		// effort: the .old holds a running image there, so a refusal to
		// delete it is a condition the next install retries, not a failed
		// update.
		if runtime.GOOS == "windows" {
			_ = os.Remove(self + displacedSuffix)
		}
		return self, nil
	}

	dir := filepath.Dir(self)
	core.SweepStaleTemps(dir, updateTempPrefix)
	tmp, err := os.CreateTemp(dir, updateTempPrefix+"*")
	if err != nil {
		return "", fmt.Errorf("cannot write next to %s: %w", self, err)
	}
	tmpName := tmp.Name()
	// A failed update that leaves its partial download behind is reported
	// with the failure, not swallowed: the operator otherwise sees the
	// original error with no sign the install directory now holds an
	// unverified file.
	defer func() {
		tmp.Close()
		err = core.DiscardStaged(tmpName, err)
	}()

	sum, err := fetch(ctx, assetURL, tmp, maxAssetBytes)
	if err != nil {
		return "", err
	}
	if sum != expect {
		return "", fmt.Errorf("checksum mismatch for %s: got %s, want %s", want, sum, expect)
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("cannot flush %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("cannot close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, installMode(self)); err != nil {
		return "", fmt.Errorf("cannot make %s executable: %w", tmpName, err)
	}
	// Two `toktop update` runs are not hypothetical: an operator whose first
	// run reported a network failure re-runs it in a second terminal, and a
	// dashboard's own watch makes the replacement visible before the first
	// run has printed anything. Both reach this point with a verified
	// download of the same release, and the check above was taken before
	// either of them got here, so without the install lock the last rename
	// decides which one wins and an update that ran after an older one can
	// leave the older binary installed. Where the install displaces, the
	// two-rename sequence is worse than a lost race: each run renames the
	// installed binary aside and removes the other's, so a run that dies
	// between its two renames leaves the only copy under the displaced name.
	if err := lockInstall(self, func() error { return installIfChanged(tmpName, self, expect) }); err != nil {
		return "", err
	}
	return self, nil
}

// defaultInstallMode is the mode a replacement binary gets when the mode of
// the one it replaces cannot be read.
const defaultInstallMode fs.FileMode = 0o755

// installMode is the permission set the downloaded binary is promoted to: the
// bits the installed binary already carries, not a hardcoded 0755.
//
// An update is supposed to replace the bytes and nothing else. Overwriting
// the mode reverts a choice the operator made, and reverts it toward wider
// access: a build installed 0700 or 0750 because it carries a client name, or
// because the host is shared and the binary is not for every account, comes
// back world-readable and world-executable after the first update, with no
// line in the output saying the mode changed. Carrying the mode forward makes
// the update idempotent in the one respect beyond the bytes.
func installMode(self string) fs.FileMode {
	fi, err := os.Stat(self)
	if err != nil {
		return defaultInstallMode
	}
	if perm := fi.Mode().Perm(); perm != 0 {
		return perm
	}
	return defaultInstallMode
}

// The install lock: one install of one binary at a time, across processes.
//
// It is a file created exclusively beside the binary and removed on release,
// the same construction and for the same reason as the host-key store's in
// internal/remote (lockStore): exclusive create is atomic on every filesystem
// toktop runs on, which a flock over the binary itself is not on Windows, and
// the binary's directory is guaranteed to exist because the binary is in it.
const (
	// installLockSuffix names the lock file beside the installed binary.
	installLockSuffix = ".lock"
	// installLockPoll is how often a waiting install looks for the lock again.
	installLockPoll = 20 * time.Millisecond
	// installLockStale is how old a lock file has to be before it is assumed
	// to belong to a process that died mid-replace and is broken. The
	// download is outside the lock precisely so that this can stay far above
	// the critical section: a stale threshold near the section's own length
	// would break a live lock on a loaded host. Without the break, one kill
	// would wedge every later update.
	installLockStale = time.Minute
)

// installLockWait bounds how long an install waits for a peer to finish its
// replace. The critical section is a checksum of one file and a rename, so this
// is generous; exceeding it means a peer died holding the lock, which the stale
// check then breaks. Var so tests can shrink it.
var installLockWait = 5 * time.Second

// lockInstall runs fn with an exclusive lock beside the installed binary. A
// lock that cannot be released is reported rather than dropped: the next
// update spends installLockWait on it before breaking it as stale, and an
// operator who never learns why has no way to act. The result is named so the
// deferred release can fold its own failure into whatever fn returned.
func lockInstall(self string, fn func() error) (err error) {
	lock := self + installLockSuffix
	deadline := time.Now().Add(installLockWait)
	for {
		f, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if cerr := f.Close(); cerr != nil {
				lerr := fmt.Errorf("cannot write %s: %w", core.RedactHome(lock), cerr)
				if rerr := os.Remove(lock); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
					lerr = errors.Join(lerr, fmt.Errorf("left a lock file at %s that must be deleted: %w",
						core.RedactHome(lock), rerr))
				}
				return lerr
			}
			defer func() {
				if rerr := os.Remove(lock); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
					err = errors.Join(err, fmt.Errorf("cannot release the lock at %s: %w",
						core.RedactHome(lock), rerr))
				}
			}()
			return fn()
		}
		if !os.IsExist(err) {
			// The directory is unwritable, or the filesystem has no exclusive
			// create. The install itself fails on its own with a clearer
			// error, so run it rather than reporting a lock error the operator
			// cannot act on. This is the same rule lockStore follows.
			return fn()
		}
		// A stale lock is only retried once the break actually took. A lock
		// that cannot be unlinked would otherwise keep the stale arm true and
		// spin here with no deadline check. The reason the break failed rides
		// the give-up message: without it an unremovable lock is reported as
		// one another toktop holds, which is not true and leaves the operator
		// with nothing to act on.
		var breakErr error
		// core.Age, not time.Since: the mtime carries no monotonic reading,
		// so this ages a wall clock, and a backward step (an NTP correction, a
		// resumed laptop) makes the age negative. A negative age is not
		// "older than installLockStale", so the lock from a killed update is
		// never broken and the give-up below names a peer that is not running.
		if info, serr := os.Stat(lock); serr == nil && core.Age(time.Now(), info.ModTime()) > installLockStale {
			if breakErr = os.Remove(lock); breakErr == nil {
				continue
			}
		}
		if time.Now().After(deadline) {
			if breakErr != nil {
				return fmt.Errorf("%s is locked by another toktop; the stale lock at %s could not be removed: %w",
					core.RedactHome(self), core.RedactHome(lock), breakErr)
			}
			return fmt.Errorf("%s is locked by another toktop; giving up after %s",
				core.RedactHome(self), installLockWait)
		}
		time.Sleep(installLockPoll)
	}
}

// installIfChanged puts the staged binary in place unless the installed one
// already carries expect.
//
// The checksum is re-read here, not only before the download, because that is
// what makes a duplicate run a no-op instead of a second replace. Two runs of
// the same release both pass the pre-download check (neither has installed
// anything yet), both download, and both arrive here; whichever runs second
// finds the release already installed and leaves the binary alone, so the two
// runs leave the same state as one. On Windows the second run is what would
// otherwise rename the installed binary aside and delete the first run's,
// leaving the only copy of the binary under the displaced name.
//
// A binary that cannot be read is installed over, not refused: the read
// failure is the condition this path exists to recover from, and install
// already refuses anything that does not match the release checksums.
func installIfChanged(tmpName, self, expect string) error {
	if have, cerr := fileChecksum(self); cerr == nil && have == expect {
		return nil
	}
	if err := install(tmpName, self); err != nil {
		return fmt.Errorf("cannot replace %s: %w", self, err)
	}
	return nil
}

// updateTempPrefix names the staging file a download is written to before it
// is renamed over the running binary.
const updateTempPrefix = ".toktop-update-"

// install puts the verified binary in place.
//
// A rename is atomic, and on Unix it works even while the old binary is
// running. Windows refuses to replace a running image but does allow renaming
// it out of the way first, so that is what happens there; the displaced file
// is removed on the next update, since it is still locked during this one.
// A kill between those two renames leaves the binary displaced, and the next
// run puts it back before installing over it (restoreDisplaced).
//
// The caller's fsync covered the download, not the rename: flushing the
// directory is what makes the new binary survive a crash, and without it an
// update that reported success can be gone on the next boot, leaving the
// previous version and a message saying otherwise.
//
// The caller has already checked tmpName against the release checksums, so
// install trusts its input. It is not transactional: a kill between the two
// Windows renames leaves the old binary displaced until the next run, and
// nothing here reverts a rename that already succeeded.
func install(tmpName, self string) error {
	if runtime.GOOS != "windows" {
		if err := os.Rename(tmpName, self); err != nil {
			return err
		}
		core.SyncDir(filepath.Dir(self))
		return nil
	}
	return installDisplacing(tmpName, self)
}

// displacedSuffix names the copy installDisplacing moves the installed binary
// to before renaming the new one in.
const displacedSuffix = ".old"

// installDisplacing is the two-rename install used where rename cannot replace
// a running image. It is separate from install so the sequence can be tested
// on every platform rather than only on the one that needs it.
//
// A kill between the two renames leaves the binary under the displaced name
// and nothing at the original path, which is worse here than for the host-key
// pin store: a store can be rebuilt by hand, but a host with no binary cannot
// run `toktop update` to replace one, so the next run restores the displaced
// file before it does anything else.
func installDisplacing(tmpName, self string) error {
	displaced := self + displacedSuffix
	// Recovery comes first, and its ordering is the whole point: when a
	// killed update left the binary displaced, that file is the only copy of
	// the installed binary, so removing a leftover first would delete the
	// install this run exists to recover. A leftover from a *completed*
	// update is stale, but then the binary is at the installed path and
	// restoreDisplaced does nothing, leaving the removal below to clear it.
	if err := restoreDisplaced(self, displaced); err != nil {
		return err
	}
	// The previous update's .old is still locked during this one, so a failed
	// removal is expected to be transient. Silently proceeding turns it into
	// a rename error naming the running binary, not the undeletable .old.
	if derr := os.Remove(displaced); derr != nil && !os.IsNotExist(derr) {
		return fmt.Errorf("cannot remove previous update %s: %w", displaced, derr)
	}
	if err := os.Rename(self, displaced); err != nil {
		return fmt.Errorf("cannot displace %s: %w", self, err)
	}
	if err := os.Rename(tmpName, self); err != nil {
		// Put the running binary back rather than leaving nothing installed.
		if rerr := os.Rename(displaced, self); rerr != nil {
			return fmt.Errorf("%w (could not restore original: %w)", err, rerr)
		}
		return err
	}
	core.SyncDir(filepath.Dir(self))
	return nil
}

// restoreDisplaced renames a binary left at the displaced path by a killed
// update back to where it belongs. It is a no-op unless that path is missing
// and the displaced one is not: an update that never started, or one that
// completed, leaves nothing to recover and must not have a stale file put
// under a path that is already correct.
//
// The displaced file is one this package renamed aside, so it is always a
// regular file. Anything else at that path is not a leftover of an update and
// is refused rather than promoted: a symlink there would make the install
// path a pointer to whatever it names, and a directory there would make the
// install path unrunnable. This runs before the download and before the
// checksum, so restoring here is the one place in the package that puts
// content at the executable with no verification behind it.
func restoreDisplaced(self, displaced string) error {
	if _, err := os.Stat(self); !os.IsNotExist(err) {
		return nil
	}
	if fi, err := os.Lstat(displaced); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("refusing to restore %s: not a regular file (mode %s)", core.RedactHome(displaced), fi.Mode())
	}
	if err := os.Rename(displaced, self); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cannot restore %s from %s: %w", core.RedactHome(self), core.RedactHome(displaced), err)
	}
	core.SyncDir(filepath.Dir(self))
	return nil
}
