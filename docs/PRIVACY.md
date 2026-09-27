# What toktop reads, sends and stores

toktop is a local terminal dashboard. There is no account, no daemon, no
telemetry and no analytics: nothing about your machine or your work leaves it
except the connections you point toktop at yourself. This file lists the
data flows, so the claim is checkable rather than a slogan.

## What is read

| Source | What is taken |
| --- | --- |
| Engine HTTP APIs (`--add`, or engines found on local ports) | model names, version, token counts, load, KV-cache and slot state |
| `/proc`, `ps`, Win32 CIM, `nvidia-smi`, `rocm-smi` | CPU, memory, GPU, power, temperature, process name, port flags |
| Agent transcripts, only with `--agents` | the token counters each agent records about itself, plus the working directory it ran in |
| `ssh://` target, only when you name one | the same vitals, and the leading 4096 bytes of each process's command line (see below) |

`--agents` is off by default: it means reading session files nobody pointed
toktop at. The adapters in
[agentusage/watch.go](../agentusage/watch.go) parse token counters only.
Prompt text, tool output and file contents in a transcript are not decoded,
copied into events, or displayed. A working directory becomes a note in the
feed with the last two path components and `~` in place of your home
directory, so a username in the path is not rendered.

## What is stored on disk

Nothing about usage, agents or engines. The only files toktop writes are:

- `$XDG_CONFIG_HOME/toktop/known_hosts` (mode 0600): ssh host keys, trust on
  first use, for `ssh://` targets only. `known_hosts.lock` sits beside it
  while a host key is being added and is removed when the write finishes.
- The binary itself, when `toktop update` replaces it in place.

Agent events live in memory for the life of the process. There is no history
file, no cache and no database. If you want the feed to disappear, quit.

## What is sent, and to whom

- **The endpoints you name.** Requests carry the engine bearer token
  (`$TOKTOP_BEARER`, `--bearer`) only to URLs given with `--add`.
  Engines found by port scanning are contacted with no credentials.
- **The remote host you name.** `toktop ssh://user@host` runs two fixed
  scripts over that one ssh connection: a vitals dump and a process sweep.
  The process sweep ships each command line cut to its first 4096
  characters, which is all an engine match can read; the tail, where an
  inline prompt or a credential passed as a flag sits, never crosses the
  connection.
- **GitHub**, for `toktop update` only, which is never on the startup path.
  Requests are limited to `github.com`, `api.github.com` and
  `*.githubusercontent.com` over HTTPS.
- **The ingest endpoint** accepts events pushed to it. It binds
  `127.0.0.1:8420` by default, authenticates nobody, and prints a warning
  when bound anywhere else. The `/v1/events` body is `agent`, `model`, token
  counts, a timestamp and a free-form note, all of which are stored in
  memory and rendered on your terminal.

The audit log `toktop` writes to stderr (`$TOKTOP_LOG_LEVEL`) records the
request id, method, path, status, and a peer address reduced to `loopback` or
`remote`. Event fields are not logged.

## The website

[toktop.ai](../site/worker.js) is one static page. It sets no cookies, runs
no scripts, loads nothing third-party, and stores nothing. Its server logs
whatever Cloudflare logs, which is outside this repository.

## Reporting a problem

Open an issue on
[GitHub](https://github.com/maci0/toktop/issues). Security reports follow
[SECURITY.md](../SECURITY.md).
