# toktop state and recovery

What toktop keeps on disk, what survives each kind of loss, and how to put
it back. Everything here is a description of code in this repository; the
file references are the authority, and a claim that stops matching them is a
claim to fix first.

## What state exists

toktop is a terminal dashboard. What it writes is the ssh pin store and,
during an update, files next to the binary. Everything else it touches is
somebody else's data.

| State | Where | Written by |
| --- | --- | --- |
| ssh host-key pin store | `$XDG_CONFIG_HOME/toktop/known_hosts` when `XDG_CONFIG_HOME` is absolute, otherwise `os.UserConfigDir()/toktop/known_hosts`; a config directory that is itself unusable names no store, and the run fails at connect (`internal/remote/knownhosts.go`, `defaultKnownHostsPath`) | `writeKnownHosts` |
| a copy of the store, refreshed by every write, and rewritten from the store by the next connect that finds it missing, damaged or older than the store | the same path plus `.bak` (`writeBackup`, `checkStoreCopy`) | `writeBackup` |
| the store a killed Windows update left behind | the store path plus `.displaced` (`replaceFile`), removed by the replacement that supersedes it, or by the restore that recovered the store from it (`clearInterruptedWrite`) | `replaceFile` |
| the cross-process write lock, while a write holds it | the store path plus `.lock` (`storeLockSuffix`), removed on release, broken when older than a minute | `lockStore` |
| a download being installed | a `.toktop-update-*` file beside the binary (`internal/selfupdate/install.go`, `updateTempPrefix`), removed on success and swept on the next run; a failed run that could not delete it says where it is | `install` |
| the previous binary, during a Windows install | the installed binary plus `.old` (`internal/selfupdate/install.go`, `installDisplacing`) | `installDisplacing` |
| the installed binary | the running executable's own path | `install` |
| the store a killed write staged, and never renamed | a `.known_hosts-*` file beside the store (`internal/remote/knownhosts.go`, `knownHostsTempPrefix`), removed by the rename, by the failure that reports it, or by the restore that recovered the store it belonged to (`clearInterruptedWrite`) | `atomicWriteFile` |
| a store staging file older than 24 hours | the same directory, removed by prefix and age on the next store write (`internal/core/fs.go`, `SweepStaleTemps`, `StaleTempAge`) | `writeKnownHosts` |

The two files beside the binary hold no state worth recovering: both are
rebuilt by running `toktop update` again. Everything else toktop touches is
read-only, and belongs to something else:

- agent transcripts and session databases (`--agents`): JSONL and zstd files
  under `~/.claude`, `~/.codex`, `~/.kimi-code` and the rest, plus SQLite
  stores: opencode's, and crush's `.crush/crush.db` inside each watched
  project. toktop reads them and never writes them, so a lost agent
  session is that agent's loss, not a toktop recovery. See
  [PRIVACY.md](PRIVACY.md) for the full list.
- `~/.gauntlet/agents.json`: read to resolve `--agents`, never written.
- the Cloudflare Worker in `site/`: a stateless worker with static assets
  and no store behind it (`site/wrangler.jsonc`). A bad deploy is undone
  with `make site-rollback`, which needs no data restore, and the details are
  in [Rolling the site back](#rolling-the-site-back).

There is no database, no queue, no cache directory and no uploaded file.
Nothing to back up beyond the pin store, which is why this file is short.

## What toktop deletes

Every removal in the tree names its victim, so no path can take a store or an
installed binary with it:

- `known_hosts.bak` is never unlinked: every write of the store replaces it
  through the same staged, fsynced, renamed write the store itself uses
  (`writeBackup`, `atomicWriteFile`).
- `known_hosts.displaced` is cleared before `replaceFile` moves a store aside,
  and a copy that could not be cleared fails the write by name, so the
  operator learns which file has to be deleted by hand. One that survives a
  replacement that did land is reported the same way, and one a restore has
  already read the store back from is removed by that restore
  (`clearInterruptedWrite`).
- `known_hosts.lock` is removed on release and broken as stale after a minute
  (`lockStore`). It is the only file toktop creates and deletes in one write.
- `.known_hosts-*` and `.toktop-update-*` are removed by the rename that
  supersedes them, by the failure that reports where one was left, by the
  restore that recovered the store the staging file belonged to, and by
  `SweepStaleTemps` once they are older than `StaleTempAge` (24 hours). A
  staging file younger than that belongs to a write in progress, and the age
  gate is what keeps the sweep from taking it.
- `toktop.old` is removed by the next update, before the next one displaces
  the binary again (`applyTo`, `installDisplacing`). It is the previous
  binary, never the installed one.

## RPO and RTO

| Question | Answer |
| --- | --- |
| RPO for pinned host keys | zero, provided the pin store is copied with its directory. A store that loses its last write costs the pins added since the copy, and the copy is a write behind whenever a write reported that it could not refresh it: the store is durable at that point, so the failure is a warning naming the path, not an error (`writeKnownHosts`). A copy that stayed behind stays behind only until the next connect: `checkStoreCopy` finds a copy that is missing, damaged, or older than the store, rewrites it from the store, and logs that it did, so the gap closes itself instead of waiting to be noticed. |
| RTO for the pin store | seconds: it is one text file, restored by copying it back. Nothing to replay, reconcile or rebuild. |
| RTO for a lost install | one download from the release page. There is no install state to recover. |
| RPO for a bad release | the installed binary, and only the binary: it is the one file an update replaces. The pin store is a file beside it, not a record inside it, so no pin is lost with a bad release. The release page is the other copy, and it is not the only one: the tag rebuilds the same bytes ([Rebuilding a release instead of downloading it](#rebuilding-a-release-instead-of-downloading-it)). |
| What a lost pin store actually costs | a forced re-trust, not a dashboard outage, and that happens whenever the store is gone, because deleting it is how a host is re-pinned on purpose. The copies beside it are read back only where a write did not finish: a leftover staging file or a displaced copy beside the store is the evidence (`interruptedWrite`), and without one the store is treated as holding no pins, so the next connect accepts whatever key that host presents. Losing the store mid-write therefore costs nothing, while a store removed by hand drops the protection against a key change as well as against a first-contact interception, and that is the price of the re-pin gesture. It is also why the read path treats a store it cannot trust as an error rather than as "nothing pinned". |

## Restoring the pin store

The copy beside the store is what a restore reads, and the one to read is the
newer of the two, since a copy that predates the last write is missing the pins
that write added. `store` below is the store's own directory, which is not
always the default: an absolute `XDG_CONFIG_HOME` puts the store under
`$XDG_CONFIG_HOME/toktop`, and every error, warning and `first use` line
toktop prints names the path in force, so read it off the last run rather than
assuming `~/.config` (macOS `~/Library/Application Support/toktop`, Windows
`%AppData%\toktop`).

```sh
store=~/.config/toktop        # or $XDG_CONFIG_HOME/toktop
ls -t --time-style=long-iso "$store/known_hosts.bak" \
                     "$store/known_hosts.displaced"
cp "$store/known_hosts.bak" "$store/known_hosts"
```

`known_hosts.bak` is the copy to copy unless the listing says otherwise: every
write of the store refreshes it, and `known_hosts.displaced` is what a killed
Windows write moved aside, so it holds the store as it was *before* that write.
The exception is a write whose backup could not be written, which leaves
`known_hosts.bak` a whole write behind; the warning toktop prints then, naming
the path and the error, is the signal to read the `.displaced` copy instead.
`readKnownHosts` makes the same choice on its own, freshest copy first
(`copiesByRecency`, `internal/remote/knownhosts.go`), but only where a write
was interrupted: a staging file or the displaced copy has to be sitting beside
the store for the copies to be read at all (`interruptedWrite`). A store you
removed yourself is the re-pin gesture, so the run after an `rm` re-pins
rather than reading a copy back.

A restore spends the marks that justified it (`clearInterruptedWrite`). The
store is back and carries every pin, so the marks are the evidence of a loss
that has already been repaired, and a mark left in place would keep the next
`rm` from being a re-pin: for as long as it survived, deleting the store to
accept a host's new key would read as another interrupted write and hand the
rejected key back. A new killed write leaves new marks, so a later loss is
still recovered, on its own evidence.

One `cp`, because the store is a text file, one record per line, in the
`host key-type base64` form OpenSSH uses. `ssh-keygen -l -f` reads it, and
so does toktop. Concatenating a shared `~/.ssh/known_hosts` is not the way
to get one: toktop matches the host field literally, so a hashed host
(`|1|...`) or a pattern list in a line never matches the host it stands
for, and the pins it appears to hold are pins on names nothing will ever
present.

A store that is missing with no interrupted-write marks beside it reads as an
empty one, because a deletion toktop cannot tell from a deliberate one. The
copy beside it is not read back for exactly that reason: restoring it would
undo the re-pin gesture. What toktop does do is say so. When a copy still
parses, the connect warns that the store was removed, that every pin it held
is dropped, names the copy and prints the `cp` that puts it back
(`warnDeletedStore`, `internal/remote/knownhosts.go`). So a store removed by a
config reset, a cleanup script or a sync is not silent: the operator learns
that the next connection to a host they had pinned will be trusted on first
contact, and has the command to undo it before making it. A store that was
never written is not a loss and warns about nothing.

Without that copy, the pins are not recoverable from this machine. Re-pin each
host by connecting to it once and judging the fingerprint printed at first use,
or restore the directory from whatever backs it up. Deleting the store is a
deliberate way to do exactly that, and the next connect pins each host again
and says so on stderr.

## Restoring a store that is present but damaged

A store that an interrupted write left missing is read back from a copy on its
own, so that case needs no `cp`. A
store that is *there* and does not parse is the other case, and toktop refuses
it rather than reading a copy in its place: a copy predating the last write is
missing pins, and standing those in silently re-trusts every host they
covered. The refusal is the connect failing, with an error naming the line it
could not read.

The error also names the copy that does parse, and the command to put it back,
so the repair is the `cp` above against the file it names:

```sh
cp "$store/known_hosts.bak" "$store/known_hosts"
```

Read the copy first if the store was hand-edited to something worth keeping,
since the copy is the store as of the last write and carries no hand edit.
A copy that does not parse is not named, because restoring it leaves a store
toktop refuses the same way. The only other way out is deletion: a store
removed on purpose re-pins on the next connect, at the cost of judging every
fingerprint again.

`TestReadKnownHostsNamesTheCopyToRestoreFrom` and
`TestReadKnownHostsDoesNotNameADamagedCopy` in
`internal/remote/remote_test.go` pin both halves, so a hint that starts
naming a copy toktop would refuse fails there.

## Verifying a restore

A restored store is verified by what it refuses, not by what it accepts:

```sh
ssh-keygen -l -f "$store/known_hosts"
```

The last field of each printed line is the SHA-256 fingerprint, the same
one toktop prints for a first contact and for a refused change. Compare it
against the fingerprint the host presents today. If it differs, the host
was rebuilt or the line is wrong: leave the store alone and work out which,
rather than replacing the pin to make the connect succeed.

There is no operator-facing restore check to run, but the round trip is
pinned by tests rather than by inspection, in
`internal/remote/remote_test.go`: `TestReadKnownHostsRecoversBackupStore`
and `TestReadKnownHostsRecoversDisplacedStore` write a store, delete it, and
read it back from each copy, and
`TestReadKnownHostsRecoversFromTheFresherCopy` pins that the copy read is
the freshest of the two, so a restore cannot hand back fewer pins than the
operator had. `go test ./internal/remote/` runs them.

The copy the store is recovered from is checked on every connect rather than
only at the write that would have refreshed it:
`TestCheckStoreCopyRewritesACopyItCannotRecoverTheStore` removes the copy,
ages it behind the store and damages it in turn, and pins that each is
rewritten from the store, logged, and not reported again once it is current;
`TestCheckStoreCopyLeavesACurrentCopyAlone` pins that a backed-up store costs
a connect nothing. A copy that is silently gone is therefore a state the next
run repairs, not one that has to be noticed.

The two halves of the re-pin rule are pinned together in
`internal/remote/knownhosts_repin_test.go`, because one is only true while the
other is:
`TestDeletingTheStoreRepinsRatherThanRecoveringTheBackup` holds that a store
removed by hand re-pins, and
`TestRestoringTheStoreClearsTheEvidenceItActedOn` holds that a restore clears
the marks it acted on, so the re-pin works again on the very next run instead
of a later one.

The half that makes an accidental deletion recoverable is
`TestDeletingTheStoreWarnsWhenACopyStillHoldsItsPins`: the store is removed by
hand with its copy intact, and the warning is the only thing that tells the
operator the copy is there. It also holds that a store which was never written
warns about nothing, so the first connect on a fresh install stays quiet.

Every write to the store is atomic (staged, fsynced, renamed, with the
directory entry flushed afterwards) and cross-process serialized by a lock
file beside the store (`lockStore`), so a store that exists is a whole one.
The reads that can find it damaged say so: an unparsable record, a host
recorded twice with different keys, and a file holding no records at all
are each an error naming the file, not a shorter store.

## Recovering from a bad release

An update renames a verified binary over the installed one and changes nothing
else. The pin store is a file beside that binary rather than a record inside
it, so a release that breaks the dashboard costs a dashboard, not data.

What the host still holds afterwards is the platform's business, and the two
answers differ:

- Windows: `installDisplacing` moved the running binary to `toktop.exe.old`
  before the new one went in, and cannot delete it while the dashboard is
  running it. The next update clears it. Until then the previous release is one
  `mv` away, once the dashboard has quit.
- Every other platform: rename replaces the file, which drops the name of the
  binary that was there, and nothing keeps a copy of it. A downgrade is not
  something the tree can do for itself.

A Windows host killed between the two renames of an update is left with no
binary at the installed path, and the only copy under the displaced name
(`restoreDisplaced` puts it back, but only on a run of the binary that is
missing). Put it back by hand:

```sh
mv toktop.exe.old toktop.exe
toktop --version
```

The file is the previous release, verified the way `toktop update` verified
it, so the fingerprint is not in question; what is missing is the name, and
that is what the `mv` restores. In PowerShell the same move is
`Move-Item toktop.exe.old toktop.exe`.

A zero-length `toktop.exe.old` is not moved: it is not a binary any platform
can run, and a host whose install path holds one cannot execute the update
meant to repair it, so `restoreDisplaced` leaves it where it is and the
checksummed download becomes the installed binary instead. Delete the empty
file by hand once the next update has landed.

On those platforms the previous release is installed by hand and verified the
way `toktop update` verifies. A release publishes the asset
`toktop_<version>_<goos>_<goarch>` (plus `.exe` on Windows) beside the archive
`toktop_<version>_checksums.tar.gz`, which holds a `checksums.txt` of
`<hex>  <name>` lines (`AssetName`, `checksumsName`, `ChecksumListing`, which
is `sha256sum` output). Download both from the tag's release page, then:

```sh
tar -xzf toktop_<version>_checksums.tar.gz checksums.txt
# The two digests have to be the same line. `sha256sum FILE` is GNU coreutils
# and `shasum -a 256 FILE` is the same digest under the name macOS ships, so
# both spellings are shown rather than one that is missing on the host.
sha256sum toktop_<version>_<goos>_<goarch>        # shasum -a 256 on macOS
grep ' toktop_<version>_<goos>_<goarch>$' checksums.txt
install -m 0755 toktop_<version>_<goos>_<goarch> "$(command -v toktop)"
toktop --version
```

The listing holds an entry for all six platform binaries and the other five
are not on this host, which is why the digest is computed for the one file
rather than checked with `sha256sum -c`.

On Windows, the same steps in PowerShell (`tar` is bsdtar and ships with
Windows 10 and later, and no host has either digest tool by that name):

```powershell
tar -xzf toktop_<version>_checksums.tar.gz checksums.txt
Get-Content checksums.txt | Select-String "toktop_<version>_windows_<goarch>\.exe"
(Get-FileHash toktop_<version>_windows_<goarch>.exe -Algorithm SHA256).Hash.ToLower()
Copy-Item toktop_<version>_windows_<goarch>.exe (Get-Command toktop).Source -Force
toktop --version
```

The hash `Get-FileHash` prints has to equal the one on the line `Select-String`
picked out; the file stays where it is until it does.

The last line is the check that the replacement took: a binary that will not
run cannot report its own version, so a dashboard that starts again is the
first evidence the old one is back. `toktop update` always fetches the latest
release, so this only applies while that release is the broken one; the
project does not support a downgrade as a channel (see
[../CHANGELOG.md](../CHANGELOG.md)).

## Rebuilding a release instead of downloading it

The release page is where to get the bytes from, and it is not the only place
they exist: the tag builds them. Every input to the bytes is in the
repository, so a release whose assets are gone (deleted, or unreachable
because the network is what is broken) is rebuilt from its own tag:

```sh
git clone https://github.com/maci0/toktop && cd toktop
git checkout v<version>
make test-dist VERSION=<version>
install -m 0755 "dist/toktop_<version>_<goos>_<goarch>" "$(command -v toktop)"
```

`make test-dist` builds the six release platforms with the release flags and
nothing else, so it needs no token, no `gh` and no network beyond the Go
toolchain and module cache `go.mod` names. The binary it writes is the
published one, byte for byte, which is a property the build is arranged to
have rather than one to hope for: `-trimpath -buildvcs=false -buildid=`
(`GO_BUILDFLAGS`, `LDFLAGS` in the Makefile) leave no source path, checkout
or build id in the output, `GOTOOLCHAIN` pins the compiler to the `go` line
of `go.mod`, and `repro-check` builds every release platform twice from two
different source paths with two cold build caches and fails on any diff. That
gate runs in CI on every PR (`repro-check-pair`, and `make pr` locally), so a
release that shipped is one that gate passed.

Comparing the rebuilt file against the published digest is what makes it the
release rather than a build from the same tree: the line
`toktop_<version>_checksums.tar.gz` lists for it is the digest to compare
against, and `make release-verify VERSION=<version>` fetches every published
asset and re-verifies all of them. A release page that cannot be reached
costs that comparison, not the binary. A rebuilt binary with no published
digest to compare against is one no installed host would accept either, since
`toktop update` refuses a download it cannot check; keep the file to compare
against once the page is back.

## Rolling the site back

`make site-rollback` calls `wrangler rollback` with no version, which puts
back the deployment before the most recent one, whoever shipped it, and then
waits for `https://toktop.ai/health` to answer `ok` (the body names no
version, so that is an availability check and not a confirmation of what is
serving).

The target refuses to run unless `dist/site.deployed` exists, the marker
`site-deploy` leaves behind. That marker is what stops a second rollback from
undoing the first and putting the broken deployment back, and it lives under
`dist/`, which `make clean` removes. After a clean, a rollback of a deploy
from this tree is refused with "nothing to roll back" while the bad
deployment is still live. The way out is `wrangler rollback` at the pin the
Makefile names (4.126.0), once, having checked the deployment list in the
Cloudflare dashboard for what the first rollback undid.

A rollback reaches one version back. Further back than the platform's
deployment history is a redeploy, not a rollback, and its source is git:
`site/worker.js` and `site/public` at the commit or tag that last served
correctly, uploaded with `make site-deploy`. The history on the platform is
the only copy of a Worker that was never pushed anywhere else, so a
deployment has to be made from a checkout rather than from a note.

## Failure domains

- The store and both copies share a directory, and all three are removed by
  anything that removes the config directory. The copies cover a store that
  is damaged, emptied, or overwritten by another tool; they do not cover a
  lost home directory. Backing the directory up is the operator's half, and
  is the only backup step in this project.
- Both copies are written under the same user credential as the store.
  A credential that can delete the store can delete the copy, so a backup
  under a different account or a different medium is the only thing that
  survives a compromised account.
- The site's only history is the deployment list on one Cloudflare account,
  and a credential holding it can delete it as well as the Worker. A
  rollback that depends on that list is therefore no more durable than the
  account, which is why `site/worker.js` and `site/public` live in git: the
  checkout is the copy that survives the account, and it is the only one
  there is.
- A published release's only copy is the release page on one repository, and a
  credential holding that repository can delete the assets as well as the tag.
  The bytes do not depend on the page, though: the build is reproducible and
  gated on it, so the tag rebuilds them and the checkout that holds the tag is
  the copy that survives the page, in the same way `site/` survives the
  deployment list. What the page alone holds is the published digest, and that
  is what the rebuild is checked against.
