# toktop state and recovery

What toktop keeps on disk, what survives each kind of loss, and how to put
it back. Everything here is a description of code in this repository; the
file references are the authority, and a claim that stops matching them is a
claim to fix first.

## What state exists

toktop is a terminal dashboard. It writes one file, and there is nothing
else.

| State | Where | Written by |
| --- | --- | --- |
| ssh host-key pin store | `$XDG_CONFIG_HOME/toktop/known_hosts` when `XDG_CONFIG_HOME` is absolute, otherwise `os.UserConfigDir()/toktop/known_hosts` (`internal/remote/knownhosts.go`, `defaultKnownHostsPath`) | `writeKnownHosts` |
| a copy of the store, refreshed by every write | the same path plus `.bak` (`writeBackup`) | `writeBackup` |
| the store a killed Windows update left behind | the store path plus `.displaced` (`replaceFile`) | `replaceFile` |
| the previous binary, during a Windows install | the installed binary plus `.old` (`internal/selfupdate/selfupdate.go`, `installDisplacing`) | `installDisplacing` |
| the installed binary | the running executable's own path | `install` |

Everything else toktop touches is read-only, and belongs to something else:

- agent transcripts and session databases (`--agents`): JSONL, zstd, and
  SQLite files under `~/.claude`, `~/.codex`, `~/.kimi-code`, `~/.crush`
  and the rest. toktop reads them and never writes them, so a lost agent
  session is that agent's loss, not a toktop recovery. See
  [PRIVACY.md](PRIVACY.md) for the full list.
- `~/.gauntlet/agents.json`: read to resolve `--agents`, never written.
- the Cloudflare Worker in `site/`: a stateless worker with static assets
  and no store behind it (`site/wrangler.jsonc`). A bad deploy is undone
  with `make site-rollback`, which needs no data restore.

There is no database, no queue, no cache directory and no uploaded file.
Nothing to back up beyond the pin store, which is why this file is short.

## RPO and RTO

| Question | Answer |
| --- | --- |
| RPO for pinned host keys | zero, provided the pin store is copied with its directory. A store that loses its last write costs the pins added since the copy. |
| RTO for the pin store | seconds: it is one text file, restored by copying it back. Nothing to replay, reconcile or rebuild. |
| RTO for a lost install | one download from the release page. There is no install state to recover. |
| What a lost pin store actually costs | a forced re-trust, not a dashboard outage, but only once the copies are gone too. A store that is missing, emptied or overwritten is read back from `known_hosts.bak` (or the `.displaced` copy) and rewritten on the next connect, so a rogue `rm` of the store alone still refuses a changed key. Losing the store *and* the directory it sits in leaves the next connect accepting whatever key that host presents, which removes the protection against a first-contact interception. That is the reason the copy below exists, and the reason the read path treats a store it cannot trust as an error rather than as "nothing pinned". |

## Restoring the pin store

The copy beside the store is what a restore reads:

```sh
cp ~/.config/toktop/known_hosts.bak ~/.config/toktop/known_hosts
```

One `cp`, because the store is a text file, one record per line, in the
`host key-type base64` form OpenSSH uses. `ssh-keygen -l -f` reads it, and
so does toktop. Concatenating a shared `~/.ssh/known_hosts` is not the way
to get one: toktop matches the host field literally, so a hashed host
(`|1|...`) or a pattern list in a line never matches the host it stands
for, and the pins it appears to hold are pins on names nothing will ever
present.

If `known_hosts.bak` is gone too, and the store is missing, the pins are
not recoverable from this machine. Re-pin each host by connecting to it
once and judging the fingerprint printed at first use, or restore the
directory from whatever backs it up. Deleting the store is a deliberate
way to do exactly that: a missing store with no copy beside it reads as an
empty one, so the next connect pins each host again and says so on stderr.

## Verifying a restore

A restored store is verified by what it refuses, not by what it accepts:

```sh
ssh-keygen -l -f ~/.config/toktop/known_hosts
```

The last field of each printed line is the SHA-256 fingerprint, the same
one toktop prints for a first contact and for a refused change. Compare it
against the fingerprint the host presents today. If it differs, the host
was rebuilt or the line is wrong: leave the store alone and work out which,
rather than replacing the pin to make the connect succeed.

There is no automated restore check to run. Every write to the store is
atomic (staged, fsynced, renamed, with the directory entry flushed
afterwards) and cross-process serialized by a lock file beside the store
(`lockStore`), so a store that exists is a whole one. The reads that can
find it damaged say so: an unparsable record, a host recorded twice with
different keys, and a file holding no records at all are each an error
naming the file, not a shorter store.

## Failure domains

- The store and its copy share a directory, and both are removed by
  anything that removes the config directory. The copy covers a store that
  is damaged, emptied, or overwritten by another tool; it does not cover a
  lost home directory. Backing the directory up is the operator's half, and
  is the only backup step in this project.
- Both copies are written under the same user credential as the store.
  A credential that can delete the store can delete the copy, so a backup
  under a different account or a different medium is the only thing that
  survives a compromised account.
