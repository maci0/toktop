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
	"github.com/maci0/toktop/internal/lockfile"
	"github.com/maci0/toktop/internal/logcfg"
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
	// A staging file the sweep could not unlink does not fail the update: the
	// install below writes and renames in this same directory and fails with
	// its own cause if it cannot. It is reported rather than dropped, so an
	// operator whose install directory is filling up with leftovers learns
	// which one refused to go instead of finding them by listing it.
	if serr := core.SweepStaleTemps(dir, updateTempPrefix, time.Now()); serr != nil {
		logcfg.Logger().Warn("toktop: staging file beside the binary not removed",
			"path", logcfg.Field(core.RedactHome(self), logcfg.FieldCap),
			"error", logcfg.RedactedField(serr.Error(), logcfg.FieldCap))
	}
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
		return "", fmt.Errorf("cannot fetch %s: %w", want, err)
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

// lockInstall runs fn with the cross-process lock held beside the installed
// binary, so a peer does not replace it mid-replace.
func lockInstall(self string, fn func() error) error {
	return lockfile.With(self+installLockSuffix, self, lockfile.Policy{
		Wait:  installLockWait,
		Poll:  installLockPoll,
		Stale: installLockStale,
	}, core.RedactHome, fn)
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
		if err := core.SyncDir(filepath.Dir(self)); err != nil {
			return fmt.Errorf("installed %s, but the rename is not durable and a crash can leave the previous version: %w",
				core.RedactHome(self), err)
		}
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
	if err := core.SyncDir(filepath.Dir(self)); err != nil {
		return fmt.Errorf("installed %s, but the rename is not durable and a crash can leave the previous version: %w",
			core.RedactHome(self), err)
	}
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
//
// An empty one is left where it is and the install carries on. A zero-length
// file is not a binary any platform can run, so promoting it would leave a
// host that cannot execute the update meant to repair it, which is the state
// this function exists to end. A downloaded release is checksummed before it
// is renamed into place, so that path ends with something that runs.
func restoreDisplaced(self, displaced string) error {
	// Only a path that is not there is restored over. A Stat that failed for
	// any other reason (no permission on the directory, an immutable entry)
	// says nothing about what is at self, and treating it as "not missing"
	// let the rename below replace a binary whose state nobody had read.
	if _, err := os.Stat(self); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("cannot tell whether %s is missing: %w", core.RedactHome(self), err)
	}
	if fi, err := os.Lstat(displaced); err == nil {
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("refusing to restore %s: not a regular file (mode %s)", core.RedactHome(displaced), fi.Mode())
		}
		if fi.Size() == 0 {
			return nil
		}
	}
	if err := os.Rename(displaced, self); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cannot restore %s from %s: %w", core.RedactHome(self), core.RedactHome(displaced), err)
	}
	if err := core.SyncDir(filepath.Dir(self)); err != nil {
		return fmt.Errorf("restored %s, but the rename is not durable and a crash can lose the only copy: %w",
			core.RedactHome(self), err)
	}
	return nil
}
