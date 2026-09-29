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
dashboard runs. Engine matching and the engine process's own `--port` flag
read no further in, so nothing past the cut changes what is reported, and a
browser, an Electron app
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

- `$XDG_CONFIG_HOME/toktop/known_hosts` when that variable holds an absolute
  path, otherwise `toktop/known_hosts` under the platform's user config
  directory (`~/.config` on Linux, `~/Library/Application Support` on macOS,
  `%AppData%` on Windows) (mode 0600): ssh host keys, trust on
  first use, for `ssh://` targets only. `known_hosts.lock` sits beside it
  while a host key is being added and is removed when the write finishes.
  Each write goes to a short-lived `.known_hosts-*` staging file that is
  renamed into place, and a Windows replacement that had to move the old store
  aside leaves a `known_hosts.displaced` copy behind. Every write also leaves
  a `known_hosts.bak` copy of the store, holding the same host keys.
- A `.toktop-update-*` download next to the binary, and the previous binary as
  `<binary>.old`, when `toktop update` replaces it in place.

Agent events live in memory for the life of the process. There is no history
file, no cache and no database. If you want the feed to disappear, quit.

`known_hosts` is the only thing here that outlives a run, and the `.bak` copy
beside it is the only backup toktop keeps: same directory, same credential, so
it covers a store that is damaged, emptied or overwritten, not a config
directory that is lost. The directory is one small file, worth copying into
whatever you already back up. If the store and its copy are both lost, every
host you have connected to with `ssh://` is trusted again on its next
connection, so a store that reads as damaged, truncated or emptied refuses the
connection with an error instead of doing that silently. Deleting the file is
how you ask for that on purpose, and it must then be verified out of band, as
any first contact is.

## Reading it back, changing it, deleting it

There is no account and no store to export from, so the three requests a data
protection law asks for are answered by the same fact rather than by a
feature:

- **Access.** Everything toktop holds is on the screen or in the report you
  asked for: `--json` and `--plain` print the whole feed, and the audit log on
  stderr has every line it wrote. Quitting takes all of it with the process.
- **Correction.** Nothing a sender posts is read back from a file, so there is
  no stored copy to correct. A sender whose event names the wrong thing posts
  the right one; the endpoint keeps a bounded ring, so the wrong one leaves on
  its own.
- **Erasure.** The feed, the histories and the counters are in memory and are
  gone when the process exits. The one file on disk, `known_hosts`, holds ssh
  public host keys, which identify a machine rather than a person; deleting the
  file (and its `.bak` and `.displaced` copies beside it) removes them, and
  `toktop` has no other copy.

A client that pushed events to a dashboard it no longer wants them in has no
way to take them back out: the endpoint answers no query about what it stored.
Asking the operator to quit is the erasure, and until then the events are on
that operator's screen, not on a service's.

## What is sent, and to whom

- **The endpoints you name.** Requests carry the engine bearer token
  (`$TOKTOP_BEARER`, `--bearer`) only to URLs given with `--add`.
  Engines found by port scanning are contacted with no credentials.
- **The remote host you name.** `toktop ssh://user@host` runs fixed scripts
  over one ssh connection: a vitals dump, a `/proc/net/tcp(+6)` read (falling
  back to an active port probe against the well-known list when the kernel
  hides the table), and a process sweep. The process sweep ships each command
  line cut to its first 4096 bytes (the remote script pins `LC_ALL=C`, so
  `cut -c` counts bytes rather than characters), which is all an engine match
  can read; the tail, where an inline prompt or a credential passed as a flag
  sits, never crosses the connection.
- **GitHub**, for `toktop update` only, which is never on the startup path.
  Requests are limited to `github.com`, `api.github.com` and
  `*.githubusercontent.com` over HTTPS.
- **The ingest endpoint** accepts events pushed to it. It binds
  `127.0.0.1:8420` by default, authenticates nobody, and prints a warning
  when bound anywhere else. The `/v1/events` body is `id`, `agent`, `model`,
  `kind`, token counts, a timestamp, a `span_ms` duration, `via_engine` and
  a free-form note, all
  of which are stored in memory and rendered on your terminal. Every
  free-form field has the home directory folded to `~` on the way in, the
  note and the `X-Request-Id` among them: a client that names the session file
  or the directory
  it reports on would otherwise put the account that owns the home into
  whichever field it chose. The fold covers both homes a client can name, the
  one this process runs under and the one the client itself runs under, since
  every peer posting an event names a path under an account that is not the
  dashboard's. A note that
  is nothing but a directory gets the same two components a locally
  watched working directory does, with the home folded to `~`; a note
  carrying any other text is stored as sent, with the home folded to `~`.
  The directories above the checkout are where a client's name and a
  project index sit, so they are dropped on the way in rather than
  rendered.

The audit log `toktop` writes to stderr (`$TOKTOP_LOG_LEVEL`) records the
request id, method, path, status, and a peer address reduced to `loopback` or
`remote`. Event fields are not logged. It also records the run's active
configuration (the interval, the listen addresses, the log floor, the mode
flags, and the ssh target count, with the bearer token named as `set` and
never by its value), the address the ingest endpoint actually bound, and the
model id a failing probe was sent to. Every line has the home directory folded to `~`, in the
message and in every attribute, so a request path or an error text carrying a
path under `$HOME` cannot name the account: a logger built outside `logcfg` is
the only way to write a line that has not been through that fold.

Agent stores name a working directory with the path's separators turned into
`-` (`/home/<user>/Desktop/vllm` becomes `home-<user>-Desktop-vllm`), so a
transcript path under one is not a path any prefix fold can see. The
transcript read, walk, decode and attribution lines fold that spelling to `~`
as well, and print the project directory it belongs to.

Diagnostics name the file that failed, but the home directory is rewritten to
`~` first: an absolute path under `$HOME` names the account, and these lines
are what gets pasted into issues. An `ssh://` target is named in the audit log
by host and port, without the account: a login names a person on the host, and
the home fold above does not reach one, so it is dropped where the line is
written. The host alone says which target a line is about, and the account
still appears in the message toktop prints to your own terminal, next to the
password prompt when one is needed.

What a remote host prints is folded the same way, against the remote's own
account: a vitals script that fails in the peer's login shell reports the
path of the account it ran as, and that text rides the stderr tail into the
frame, the `--json` report and the audit log. The account named in the target
is the one toktop logs about, so a home spelling that account (`/home/<user>`,
`/Users/<user>`, `<drive>:\Users\<user>`, `/var/home/<user>`,
`/export/home/<user>`, `/nfs/home/<user>` or `/srv/homes/<user>`, in any
case) becomes `~` before the
line is built. The ssh login and the peer address are dropped from the same
text before it is stored, not only where it is logged, so the copy the frame
and both reports publish is the folded one. A longer name that merely starts
with the account, such as
`/home/<user>-old`, is a different home and is left alone.

## The website

[toktop.ai](../site/worker.js) is one static page. It sets no cookies, runs
no scripts, loads nothing third-party, and stores nothing. The Worker runs with
Cloudflare's invocation logs off, so no request is recorded with a visitor's IP
address or user agent; the console lines it writes about its own failures
carry a ray, a method and a path instead, never the client. That setting is in
[site/wrangler.jsonc](../site/wrangler.jsonc), and Cloudflare keeps whatever
its own edge logs, which is outside this repository.

## Reporting a problem

Open an issue on
[GitHub](https://github.com/maci0/toktop/issues). Security reports follow
[SECURITY.md](../SECURITY.md).
