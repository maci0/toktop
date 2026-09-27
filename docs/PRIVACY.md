# What toktop reads, sends and stores

toktop is a local terminal dashboard. There is no account, no daemon, no
telemetry and no analytics: nothing about your machine or your work leaves it
except the connections you point toktop at yourself. This file lists the
data flows, so the claim is checkable rather than a slogan.

## What is read

| Source | What is taken |
| --- | --- |
| Engine HTTP APIs (`--add`, or engines found on local ports) | model names, version, token counts, load, KV-cache and slot state |
| `/proc`, `ps`, Win32 CIM, `nvidia-smi`, `rocm-smi`, `xpu-smi`, `system_profiler`, `ioreg` | CPU, memory, GPU, power, temperature, process name, port flags, and the leading 4096 bytes of a command line (see below) |
| Agent transcripts, only with `--agents` | the token counters each agent records about itself, plus the working directory it ran in |
| `ssh://` target, only when you name one | the same vitals, the listening ports `/proc/net/tcp(+6)` reports (or an active probe of the well-known list), and the leading 4096 bytes of each process's command line (see below) |

A local process listing keeps the leading 4096 bytes of a command line and
drops the rest, rather than holding the whole line for as long as the
dashboard runs. Engine matching and the `--port` hint read no further in, so
nothing past the cut changes what is reported, and a browser, an Electron app
or an agent started with a long inline script leaves its flags, prompts and
paths on the other side of the cut rather than in a toktop structure.

`--agents` is off by default: it means reading session files nobody pointed
toktop at. The adapters in the
[agentusage](../agentusage/) package parse token counters only.
Prompt text, tool output and file contents in a transcript are not decoded,
copied into events, or displayed. A working directory becomes a note in the
feed with the last two path components and `~` in place of your home
directory, so a username in the path is not rendered.

## What is stored on disk

Nothing about usage, agents or engines. The only files toktop writes are:

- `$XDG_CONFIG_HOME/toktop/known_hosts` (mode 0600): ssh host keys, trust on
  first use, for `ssh://` targets only. `known_hosts.lock` sits beside it
  while a host key is being added and is removed when the write finishes.
  Each write goes to a short-lived `.known_hosts-*` staging file that is
  renamed into place, and a Windows replacement that had to move the old store
  aside leaves a `known_hosts.displaced` copy behind.
- A `.toktop-update-*` download next to the binary, and the previous binary as
  `<binary>.old`, when `toktop update` replaces it in place.

Agent events live in memory for the life of the process. There is no history
file, no cache and no database. If you want the feed to disappear, quit.

`known_hosts` is the only thing here that outlives a run, and toktop backs it
up nowhere: it is one small file, worth copying into whatever you already back
up. If it is lost, every host you have connected to with `ssh://` is trusted
again on its next connection, so a store that reads as damaged, truncated or
emptied refuses the connection with an error instead of doing that silently.
Deleting the file is how you ask for that on purpose, and it must then be
verified out of band, as any first contact is.

## What is sent, and to whom

- **The endpoints you name.** Requests carry the engine bearer token
  (`$TOKTOP_BEARER`, `--bearer`) only to URLs given with `--add`.
  Engines found by port scanning are contacted with no credentials.
- **The remote host you name.** `toktop ssh://user@host` runs fixed scripts
  over one ssh connection: a vitals dump, a `/proc/net/tcp(+6)` read (falling
  back to an active port probe against the well-known list when the kernel
  hides the table), and a process sweep. The process sweep ships each command
  line cut to its first 4096 characters, which is all an engine match can
  read; the tail, where an inline prompt or a credential passed as a flag
  sits, never crosses the connection.
- **GitHub**, for `toktop update` only, which is never on the startup path.
  Requests are limited to `github.com`, `api.github.com` and
  `*.githubusercontent.com` over HTTPS.
- **The ingest endpoint** accepts events pushed to it. It binds
  `127.0.0.1:8420` by default, authenticates nobody, and prints a warning
  when bound anywhere else. The `/v1/events` body is `id`, `agent`, `model`,
  `kind`, token counts, a timestamp, `via_engine` and a free-form note, all
  of which are stored in memory and rendered on your terminal. A note that
  is nothing but a directory gets the same two components a locally
  watched working directory does, with the home folded to `~`; a note
  carrying any other text is stored as sent, with the home folded to `~`.
  The directories above the checkout are where a client's name and a
  project index sit, so they are dropped on the way in rather than
  rendered.

The audit log `toktop` writes to stderr (`$TOKTOP_LOG_LEVEL`) records the
request id, method, path, status, and a peer address reduced to `loopback` or
`remote`. Event fields are not logged. Every line has the home directory
folded to `~`, in the message and in every attribute, so a request path or an
error text carrying a path under `$HOME` cannot name the account: a logger
built outside `logcfg` is the only way to write a line that has not been
through that fold.

Diagnostics name the file that failed, but the home directory is rewritten to
`~` first: an absolute path under `$HOME` names the account, and these lines
are what gets pasted into issues. An `ssh://` target is reported as you typed
it, since that host and user are the ones you pointed toktop at.

## The website

[toktop.ai](../site/worker.js) is one static page. It sets no cookies, runs
no scripts, loads nothing third-party, and stores nothing. Its server logs
whatever Cloudflare logs, which is outside this repository.

## Reporting a problem

Open an issue on
[GitHub](https://github.com/maci0/toktop/issues). Security reports follow
[SECURITY.md](../SECURITY.md).
