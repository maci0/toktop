# toktop threat model

Living document for the security owner: the whole attack surface on one screen,
with file references so each claim can be re-verified against code. Individual
vulnerabilities and their fixes belong to sec-review; this file records where
they live and what already stands in their way.

- **Last reviewed:** 2026-09-29, against this commit. The mitigations table
  was re-read row by row against the code: every control from M1 through M46
  exists and does what the row says, and the rows that had drifted are
  corrected in place rather than annotated. The one that mattered was the
  host-key pin store: the repair copies beside it are now consulted and
  restored only where `interruptedWrite` finds the marks of a write that did
  not finish, so a store an operator deleted on purpose re-pins on the next
  connect instead of being written back to the key they had just rejected; the
  asset section and M12 say that now. Two things added in the same window are
  recorded here rather than left for a later pass to find: `core.HTTPStatus`
  (M2), which folds an engine's or the GitHub API's reason phrase to one cell
  where it becomes an error text, and the `schema` field on the `--json`
  report, a shape revision a consumer can pin to. Citations into the provider,
  probe, self-update and validation packages now name symbols rather than
  line numbers, per the rule at the end of this file. No risk changed rank.
- **Owner:** none assigned in this repository
- **Review cadence:** none scheduled organizationally; re-run whenever an entry
  point, auth path, or bind default changes

Scope: the `toktop` CLI (single static Go binary), its self-update channel,
the in-repo `agentusage` readers the binary compiles in, and deployment
artifacts in this repository (GitHub Actions workflows, Makefile release
targets, the static site worker at site/worker.js). Out of scope: the
`gauntlet` tool that also imports `agentusage`
(internal/agentwatch/agentwatch.go consumes it here).

## Risk-ranked summary

| # | Risk | Boundary | Where | State |
|---|------|----------|-------|-------|
| 1 | Ingest endpoint accepts unauthenticated events from any local process (any network peer if bound non-loopback via `--ingest`); forged telemetry renders as real agents | local processes -> ingest, network -> ingest | internal/ingest/post.go, internal/ingest/middleware.go, cmd/toktop/main.go | No authentication; both browser sender classes refused (cross-origin by M21, DNS rebinding by M45), plus DoS, injection, mixed-script names, and timestamp forgery mitigated (M3-M7, M21, M26, M30, M45) |
| 2 | Trust-on-first-use accepts a first-contact MITM by design; only key *changes* are refused | operator -> ssh target | internal/remote/knownhosts.go | Documented residual risk (README "Auth" section) |
| 3 | Self-update installs whatever binary the named GitHub repo published: integrity rests on the release's own checksums.txt over TLS; no external signature exists | runtime -> update channel | internal/selfupdate/selfupdate.go, .github/workflows/release.yml | Checksum + size + GitHub-host URL verification present; owner/name validated (M18); signing absent |
| 4 | SSH engine relays bind loopback listeners (`127.0.0.1:0`); any local process can reach the remote engines those listeners front | local processes -> remote engines | internal/remote/client.go | Bound loopback-only; no listener auth |
| 5 | Hot-reload re-execs whatever binary occupies the exe path when its identity changes (Unix); PATH-based vendor CLI lookup executes tools from `$PATH` | build -> runtime, host -> process | internal/selfreload/exec_unix.go; internal/gpu/gpu.go | Windows Restart does not exec (exec_windows.go); `--no-hot-reload` exists |
| 6 | Low: ingest poisoning cannot be reconstructed from retained payloads; raising the log floor also hides successful submissions | B1, response readiness | internal/ingest/middleware.go; internal/collector/collector.go | Request metadata logs at info by default; warn/error suppress successes. No authenticated sender identity or durable event store |

Resolved since 2026-08-25: the previous ranking's "bearer token sent to every
probed endpoint" is closed by origin-scoped token application
(commit 21e3feb); see mitigations M1. The previous claim that `--repo` is
interpolated into the GitHub API path with no owner/name check is closed by
`ValidateRepo` (selfupdate.go, 126, called from `Check` at 231 and from
`toktop update` at cmd/toktop/update.go, 94). Nothing in this table is
demonstrated by attacking anything; every claim cites code read at review time.

## Assets

What is worth stealing, corrupting, or denying:

- **Engine credentials**: one process-wide bearer token
  (internal/bearer/bearer.go), sourced from `--bearer`,
  `OMNIROUTE_API_KEY`, or `TOKTOP_BEARER` (resolveBearer,
  cmd/toktop/validate.go, `resolveBearer` 327, called at
  cmd/toktop/main.go, 205 and 618). Grants API access to gateways like OmniRoute.
- **GitHub credential**: optional `GITHUB_TOKEN`, read from the environment
  during `toktop update` and sent only to api.github.com
  (internal/selfupdate/selfupdate.go). Rate-limit relief, not a secret
  with broad scope, but it leaves the host on every update check that sets it.
- **SSH credentials**: private keys read from `--ssh-key`, the
  `IdentityFile` lines of `~/.ssh/config`, or the fixed default set
  `~/.ssh/id_ed25519`, `~/.ssh/id_ecdsa`, `~/.ssh/id_rsa` (a list, not a
  glob; internal/remote/auth.go, 18-31, applied at 191-194, and the
  `IdentityFile` override at target.go, 54 and 200-201), the ssh-agent
  socket (auth.go, 141-150), and the password
  from `TOKTOP_SSH_PASSWORD` or the terminal prompt (auth.go).
- **Host-key pin store**: `$XDG_CONFIG_HOME/toktop/known_hosts` when that
  variable is set, otherwise `os.UserConfigDir()/toktop/known_hosts`
  (`defaultKnownHostsPath`, internal/remote/knownhosts.go). The env var is
  read first on every platform, so on macOS and Windows it widens the store's
  location past what `UserConfigDir` would pick. This is the only state toktop
  keeps across runs and every write to it leaves a copy of the store at
  `known_hosts.bak` beside it (`writeBackup`, internal/remote/knownhosts.go).
  A copy that is missing, damaged or older than the store is rewritten from
  the store on the next connect and logged (`checkStoreCopy`), so a copy
  write that failed once does not leave the store unprotected for as long as
  nobody noticed. The copies stand in for the store only where the store is
  missing *because a write did not finish*: `readKnownHosts` consults them
  only when `interruptedWrite` finds a leftover staging file
  (`knownHostsTempPrefix`) or a displaced copy beside the store, and
  `restoreStore` rewrites the store from whichever copy is freshest under the
  same condition. Two windows leave those marks: a kill between creating the
  temporary and renaming it over the store, and a kill between the two
  renames `replaceFile` makes on Windows. Their presence is what tells the
  two cases apart: the backup exists on every
  write, so its presence alone proves nothing, and a store an operator deleted
  by hand must read as "nothing pinned" rather than be silently undone into
  the key they just rejected. Deleting the store is therefore the re-pin
  gesture, and the next connect re-TOFUs against whatever the host presents
  then. A truncated or emptied store is a different state again and still
  fails the read loudly rather than re-trusting every host. The copies share
  the store's directory, so they recover a store lost, emptied or overwritten
  inside it, not a config directory that is gone: backing the directory up is
  the operator's half, and a store lost with its directory and no marks beside
  it is a manual repair. The same interrupted-write window in the self-update
  leaves the installed binary under `.old`, which the next install restores
  before replacing it (internal/selfupdate/selfupdate.go, `restoreDisplaced`).
  [RECOVERY.md](RECOVERY.md) is the
  operational half: the state inventory, the RPO and RTO, and the restore
  and its verification.
- **Binary integrity**: the running executable is replaceable by design twice
  over: hot-reload on Unix (internal/selfreload/exec_unix.go) and
  `toktop update` (cmd/toktop/update.go). Whoever controls either channel
  controls code execution as the user.
- **Dashboard integrity**: what the operator sees drives triage decisions.
  Poisoned rows are the main prize of the ingest endpoint.
- **Operator terminal integrity**: toktop renders attacker-shaped text
  (engine model names, remote vitals, event fields) into a TTY; escape-sequence
  injection would hijack clipboard, cursor, or title.
- **Agent session stores** (only with `--agents`): JSONL transcripts under
  agent home dirs. The default set is wider than this file previously named:
  claude (`~/.claude/projects`, agentusage/claude.go, 15), codex
  (`~/.codex/sessions`, agentusage/codex.go, 31), qwen
  (`~/.qwen/projects`), copilot (`~/.copilot/session-state`), kimi
  (`~/.kimi-code/sessions`, one directory per working directory, derived from
  the SHA-256 of its path rather than walked, agentusage/kimi.go), gemini
  (`~/.gemini/tmp`, the project named by `.project_root`), grok
  (`~/.grok/sessions/<encoded cwd>/<id>/updates.jsonl`, the turn_completed
  record), agy
  (`~/.gemini/antigravity-cli/brain/<id>/.../transcript.jsonl`, workspace
  from `history.jsonl` or `cache/last_conversations.json`, only steps that
  carry token counts), clanker
  (its token log inside the repository it runs in, registry.go, 126-133), and
  microagent (`~/.microagent/sessions/<unix-ns>.jsonl`, one record per model
  response carrying that response's counters, the directory it ran in, and
  its `elapsed_ms`, agentusage/microagent.go), and
  the built-in pi, prime-agent, feynman, and omp definitions
  (`~/.pi/agent/sessions`, `~/.prime/agent/sessions`, `~/.feynman/sessions`,
  `~/.omp/agent/sessions`), cursor-agent
  (`~/.cursor/projects/<project>/agent-transcripts`, only lines that carry
  token counts), and kimi
  (`~/.kimi-code/sessions`, agentusage/kimi.go, the default store literal in `kimiStore` 87, relocated by
  `KIMI_CODE_HOME` when that names an absolute path, agentusage/kimi.go, `kimiStore` 82). Beyond plain JSONL, dsh's default
  `session.v<N>.jsonl.zstd` (concatenated zstd frames, agentusage/dsh.go)
  and the same records under `~/.dsh-native/sessions`,
  crush's project database `.crush/crush.db`
  (agentusage/crush_sqlite.go, gated by the `sqlite` build tag and
  no extra flag), and opencode's `~/.local/share/opencode/opencode.db` or
  `$XDG_DATA_HOME/opencode/opencode.db` (agentusage/opencode_sqlite.go,
  gated by `--opencode-db`, on by default with `--agents` and disabled with
  `--opencode-db=false`). Contents are token counts and working-directory
  paths, not prompt text, but they are still the operator's local session
  metadata. The zstd decoder is a parser of untrusted bytes from a writable
  agent store; frame size is capped (`zstdMaxFrameBytes`) and decompressed
  output is capped (`WithDecoderMaxMemory`).
- **Remote engine access via loopback relays**: any local process that finds
  the ephemeral listeners can send traffic to the remote engines the ssh
  session discovered (client.go). Typical engines on those hosts
  authenticate nothing.
- **Monitoring availability**: the dashboard session itself (low value, bounded
  blast radius).
- **Information disclosure via display**: remote host CPU/OS/kernel/GPU
  inventory, engine/model lists, and agent working directories (last two path
  components in event notes, internal/agentwatch/agentwatch.go, 465) appear
  on screen and in scrollback; screen shares and captures leak them. Home
  prefixes are rewritten to `~` so a username in those components does not
  become the note (shortDir, :465, over core.ShortDir). The `--once --json`
  report carries the same fields to a machine consumer, where the sanitization
  and redaction of the display path are the only ones in force: what the
  script does with them next is its own boundary, not this program's
  (gap 11).
- **Diagnostic log lines**: stderr is what the operator pastes into issues
  and bug reports, so anything reaching it is an operator-to-third-party
  disclosure. The process logger folds the home directory out of every
  message and string attribute, and the ingest audit line names a peer only
  as `loopback:<port>` (M35); the fields left are the ones a useful report
  needs. Since the ssh, engine-state and attach audit lines joined this
  stream, the same paste carries the destinations rather than the operator's
  own account: an ssh `target` is written as the host alone
  (`Target.LogHost`, internal/remote/target.go, 166, at every call site in
  client.go and cmd/toktop/attach.go), with the account dropped from the
  error texts the transport prefixes with the target as well
  (`Target.RedactUser`, target.go, 174); the account is a login that names
  a person on the host rather than a port to check, and `user@host` is not a
  path under `$HOME`, so the home fold does not reach it. What is left on
  those lines is an engine `addr` (a `--add` URL, a scanned port,
  `host:port` for a remote engine, internal/collector/health.go, 241, and
  internal/collector/probe.go, 105),
  which is an infrastructure name the operator typed or the run discovered,
  and no control in the table removes it.

## Entry points

Every externally reachable input, with its code location:

1. **Ingest HTTP server** (on by default): `POST /v1/events` (single JSON or
   NDJSON stream), `GET`/`HEAD /healthz`
   (internal/ingest/server.go; routes registered from the endpoint table at
   `ingestEndpoints`, endpoints.go, the 405 `Allow` header names `GET, HEAD` at
   the method guard). The health route is a
   load report, not a bare liveness probe: it answers 503 with a
   `degraded: N/64 event streams in flight` line and the same
   `Retry-After` a refused POST carries while every decode slot is held
   (`handleHealth`, health.go), so it cannot report `ok` for an endpoint that
   is accepting nothing. Binds `127.0.0.1:8420`
   unless `--ingest` says otherwise (cmd/toktop/flags.go); any address is
   accepted, including routable interfaces. An empty `--ingest` is rejected
   (validateIngestAddr, validate.go) because `net.Listen` would treat it as
   `:0` (every interface, ephemeral port). A routable bind prints a startup
   warning naming the unauthenticated exposure (endpoints.go, routableBind).
   Runs
   in demo mode too. `--no-ingest` turns it off (main.go).
2. **CLI arguments**: top-level flags including `--bearer` (secret),
   `--ssh-key`, `--add URL` (repeatable), `--ingest ADDR`, `--agents`,
   `--opencode-db`, and the demo-path `--demo`, `--probe`, `--interval`,
   `--frames`, `--seed`, `--origin` (cmd/toktop/flags.go, 50-69); positional
   `ssh://[user@]host[:port]` targets (interpretArgs / ParseTarget); the
   `update` subcommand with `--check` and `--repo owner/name`
   (cmd/toktop/update.go, 67-72); and the `help` and `version` subcommands
   (cmd/toktop/main.go, 55-61), which accept arbitrary trailing arguments
   (`runHelp(os.Stdout, os.Args[2:])`) and are reachable in forwarded form as
   `toktop --help update` (main.go, 73-76). A set-but-empty `--ssh-key` is
   rejected with exit 2 rather than falling back to `~/.ssh/config` (M31), and
   an `ssh://` target named more than once attaches once, which also keeps one
   forward per remote port (M33). `--seed` fixes the demo RNG, `--origin`
   pins the instant the simulated timeline starts at so one seed replays byte
   for byte, and both are named as having no effect without `--demo`
   (cmd/toktop/validate.go, `warnIgnoredFlags` and `parseOrigin`,
   which accepts RFC3339 or Unix seconds and refuses a mistyped
   instant rather than silently leaving the run on the wall clock);
   demo mode is the one path where the ingest endpoint runs against a
   synthetic recorder. `--repo` is checked with `ValidateRepo`
   (owner/name only). `--add` rejects non-http(s), missing host, userinfo,
   a query string and a fragment, and refuses the same endpoint twice, since
   two polls of it read as twice the tokens; the query refusal matters twice
   over, because a `?api_key=` value would be a credential in argv readable
   from a process listing and would be echoed whole by the audit log, the
   dashboard and both reports (validateAddURL, parseAdd, endpoints.go, 66 and 92). An `ssh://` URL that
   embeds a password, path, query, or fragment is rejected at startup
   (internal/remote/target.go), as is a host or user carrying a bidi control,
   zero-width or other format character, tag character or variation selector
   (validTargetField, target.go), which the sanitizer of M2 strips: those
   characters render one string as another, so the trust store, the first-use
   prompt and the host label would name a host other than the one ssh dials.
   The live dashboard refuses to start when stdout is not a terminal
   (main.go, 185-189); `--once` and `--once --json` are the two non-TTY
   paths.
   `--once --json` (added in dfa93c3) is a third rendering of the same
   snapshot: one JSON object on stdout
   (internal/ui/json.go, `JSONFrame`, 23, and `jsonReportOf`, 227), read by a
   script rather than by a person. The object publishes its own shape
   revision as `schema` (`jsonReportSchema`, currently 1) beside the `version`
   that names the writing binary, so a consumer can refuse a report whose
   fields it does not know rather than read a removed or renamed one as
   present; the revision moves when a published field changes, and it is a
   shape promise only, not a sanitization claim about the field values
   (gap 11). Every free-form field passes through
   `core.SanitizeText` on the way out, as in the frame and the text report
   (json.go, 164-292, M2), so the report is no less sanitized than the
   display. What differs is the consumer: this is the shape most likely to be
   written to a file, a journal, or a webhook body and then re-rendered by
   something with no sanitizer of its own (gap 11). `--json` without
   `--once`, and `--json` with `--plain`, are named as having no effect
   rather than refused (validate.go, 91), and `TOKTOP_COLUMNS` /
   `TOKTOP_LINES` are named as unread there instead of validated, since the
   report is unsized (main.go, 96; validate.go, 367).
3. **Environment variables**: secrets `OMNIROUTE_API_KEY`,
   `TOKTOP_BEARER`, `TOKTOP_SSH_PASSWORD`, `GITHUB_TOKEN`; plus
   `SSH_AUTH_SOCK`, `TOKTOP_COLUMNS`/`TOKTOP_LINES`, `TOKTOP_LOG_LEVEL`
   (cmd/toktop/main.go; internal/remote/auth.go; internal/selfupdate;
   internal/logcfg: one floor for the ingest, ssh, engine-poll and `--add`
   attach loggers alike, and named as having no effect only in a
   `--demo --no-ingest` run, which builds no logger at all,
   cmd/toktop/validate.go, `warnUnusedEnv`),
   `GAUNTLET_HOME` (agentusage/definitions.go; honored
   only when absolute, so a relative value cannot pull definitions from the
   working directory, and it is named as ignored at startup under `--agents`;
   an absolute value with no `agents.json` under it is named in the same
   place, since a missing file is a no-op for the loader and the dashboard
   cannot tell the two apart, cmd/toktop/validate.go,
   `warnIgnoredGauntletHome`, 125),
   `XDG_CONFIG_HOME` (internal/remote/knownhosts.go, 19-28; honored only
   when absolute, and a relative value is named as ignored at startup when
   an `ssh://` target would have read it),
   `XDG_DATA_HOME` (agentusage/opencode_sqlite.go, only when
   `--opencode-db` is on, which it is by default with `--agents`; a
   relative value is named as ignored at startup, like `GAUNTLET_HOME`),
   `KIMI_CODE_HOME` (agentusage/kimi.go, `kimiStore` 82; relocates the
   kimi session store off `~/.kimi-code/sessions` for every kimi watcher,
   and is honored only when absolute, the rule M44 records for
   `GAUNTLET_HOME` and the XDG base directories, so a relative value
   yields no store rather than one under the working directory), and
   `TOKTOP_SCREENSHOT_FONT` (scripts/screenshot.py
   only; the binary ignores it).
   Three more shape where toktop connects or what it reads, and were missing
   from this list before this pass:
   - `USER` then `USERNAME` supply the ssh login user when an `ssh://` target
     names no user, before falling back to the passwd database
     (`currentUser`, internal/remote/client.go, 81-89). Both are validated
     through `validTargetField` and skipped when they would fail, so a
     malformed value falls through instead of reaching the transport (M32);
     any well-formed value is still a destination input, not a display
     string.
   - `HOME` / `USERPROFILE` via `os.UserHomeDir` outranks every other path
     root this model discusses: `~/.ssh/config`, the default identity set,
     `~/.gauntlet/agents.json`, every built-in transcript root, the opencode
     store, and the `~` redaction in internal/core/redact.go, 23. A hostile
     value relocates all of them.
   - `PROCESSOR_IDENTIFIER` is read on Windows only and rendered into the
     system strip (internal/sysmon/sysmon_windows.go, 35).
   `WINDIR` is a second font-path input to scripts/screenshot.py, 41.
   `NO_COLOR`, `CLICOLOR*`, `TERM`, and `COLORTERM` are read by the
   lipgloss/termenv renderer, not by this repository's code, though
   cmd/toktop/help.go, 60 advertises `NO_COLOR`.
4. **Self-update network fetches** (outbound HTTPS): latest-release lookup on
   api.github.com, then download of the checksums archive and platform asset
   named by that response (internal/selfupdate/selfupdate.go).
   Asset URLs must be GitHub download hosts (`githubAssetURL`, enforced by
   `trustedAssetURL` in `applyTo`, 351);
   redirects off those hosts are refused (`githubRedirect`, 212). The asset
   is verified against the downloaded checksums and size-capped before anything
   is renamed over the running binary (`applyTo`, the `ChecksumFor`
   comparison at 359).
5. **SSH client sessions** (outbound): shell scripts executed on the remote
   for discovery and vitals (internal/remote/discover.go;
   internal/remote/stats.go); all returned text is parsed locally, and
   discovery ports from the `/proc/net/tcp` sweep are parsed as 16-bit with
   zero rejected (`parseNetTCP` in discover.go; the shell-probe fallback
   accepts anything `p > 0` (`strconv.Atoi` in the shell-probe fallback,
   discover.go), see gap 8).
6. **SSH loopback relays**: `Forward` binds one `127.0.0.1:0` listener per
   remote engine port and pipes accepted connections through ssh direct-tcpip
   (internal/remote/client.go). The map of remote-port to local-port
   is used as soon as Forward returns. Any process on the host that can
   connect to loopback can speak to those remote engines for the life of the
   session; listeners are closed on Client.Close and on unattended drop
   (closeListeners :412-421, Close :423). Dial into the tunnel is bounded
   (forwardDialTimeout 8s, client.go).
7. **Engine HTTP polling** (outbound GET/POST): startup probe of well-known
   localhost ports (internal/provider/discover.go), process-derived
   candidates, operator-supplied `--add` URLs, and remote ports discovered
   over ssh (cmd/toktop/main.go). Probes POST small generations to
   engines (internal/probe/probe.go).
8. **Agent transcript and session-store reading** (local disk, `--agents`
   only): `/proc` (Linux) or `ps`/`lsof` (Darwin) finds coding-agent
   processes; their JSONL transcripts are read every second via `agentusage`
   (internal/agentwatch/agentwatch.go; agentusage/watcher.go),
   including dsh's default concatenated-zstd logs (agentusage/dsh.go). On
   Linux, attributing an agent to the same engine walks the descriptors of
   *other* processes: `socketInodes` reads `/proc/<pid>/fd` and matches the
   inodes against `/proc/net/tcp` and `/proc/net/tcp6`
   (agentusage/peers_linux.go, 72-126; the Darwin equivalent spawns one `lsof`
   per matched pid, agentusage/discover_darwin.go, 78-92 and peers_darwin.go,
   23-31). A process belonging to another user yields an empty result, which
   callers must read as "unknown", not as "a different engine".
   With the `sqlite` build tag, crush's `.crush/crush.db` is opened automatically
   for each watched working directory (agentusage/crush_sqlite.go).
   opencode's machine-wide SQLite store is a second gate: the tag plus
   `--opencode-db`, on by default with `--agents` and disabled with
   `--opencode-db=false` (cmd/toktop/main.go; agentusage/source.go).
   Agent definitions, including transcript roots, load once at startup from
   `$GAUNTLET_HOME/agents.json` or `~/.gauntlet/agents.json`, and only on
   the `--agents` path; a missing file
   is a no-op, but a malformed one aborts startup with exit 2 rather than
   silently watching a reduced agent set (cmd/toktop/main.go, 143-152;
   agentusage/definitions.go, 217).
9. **Config files read at startup**: `~/.ssh/config` (HostName/User/Port/
   IdentityFile override target fields, internal/remote/target.go),
   the known_hosts store (knownhosts.go), and
   `$GAUNTLET_HOME/agents.json` or `~/.gauntlet/agents.json`
   (agentusage/definitions.go). `XDG_CONFIG_HOME` relocates the
   known_hosts store (knownhosts.go, 31-34). The definitions file is read
   under `maxDefinitionsBytes` 8 MiB and one byte past it is read so a
   truncated document cannot decode as a complete one
   (`readCapped`, agentusage/definitions.go, 468; the cap at 287); the read
   is capped in the reader rather than by a `Stat` so a file grown between
   the two cannot slip past, and a file past the cap is refused through
   `ErrInvalidDefinitions` with the home folded out of the message (M41).
10. **Self hot-reload**: polls the running executable's stat identity and
    restarts into it when changed (internal/selfreload/selfreload.go,
    exec_unix.go; cmd/toktop/main.go). The watcher is armed on every
    platform whenever the live TUI runs (main.go, 405), default on;
    `--no-hot-reload` disables it. Only the re-exec is Unix: on Windows
    Watch still fires, but Restart only prints and the process then exits
    (exec_windows.go).
11. **Local system introspection**: `/proc` and sysctl reads
    (internal/procs/, internal/sysmon/), vendor CLIs executed from `$PATH`:
    nvidia-smi, rocm-smi, xpu-smi (internal/gpu/gpu.go),
    system_profiler and ioreg (internal/gpu/gpu_darwin.go), ps
    (internal/procs/procs_darwin.go; agentusage/discover_darwin.go),
    lsof (agentusage/discover_darwin.go; peers_darwin.go), a PowerShell
    CIM query (internal/procs/procs_windows.go).
12. **Site deployment** (operator-run, not CI): `make site-deploy` and
    `make site-rollback` take `dist/site.lock` and shell to
    `bunx wrangler@4.126.0 deploy` / `rollback` inside `site/`, then poll
    `https://toktop.ai/health` 6 times, 10s apart, and exit non-zero when
    the site never answers `ok`; a failed deploy names
    `make site-rollback` as the next step (Makefile, the `site-deploy` and
    `site-rollback` targets and the `wait_for_site` shell function they
    share, with the values in the `WRANGLER`, `SITE_LOCK`, `SITE_DEPLOYED`,
    `SITE_ROLLED_BACK`, `SITE_HEALTH_URL`, `SITE_HEALTH_TRIES` and
    `SITE_HEALTH_WAIT` assignments; the target and function names are the
    anchors here, not their line numbers). A rollback undoes the most recent
    deployment whoever shipped it, so a deploy that reported success records
    `dist/site.deployed` and a rollback moves it to `dist/site.rolled-back`:
    a second rollback has nothing of this tree's to undo and exits 0 without
    calling wrangler. The marker a deploy leaves behind holds a
    `manifest` naming the commit, the `wrangler` and `bun` pins, and the
    digests of `worker.js` and of the captures, so the version that reached
    production is on record without reading the Cloudflare dashboard; a
    rollback moves it with the directory. A deploy also refuses to run
    while `site/` or `wrangler.jsonc` holds uncommitted changes
    (`check-deploy-source`, `ALLOW_DIRTY=1` overrides), the rule
    `check-release-source` already applies to a release, because an
    uncommitted Worker or capture reaches the live site held by no commit and
    a rollback undoes the upload rather than the edit. The poll is a
    presence-and-binding check, not an
    identity check: `/health` answers 503 rather than `ok` while the worker's
    asset binding is unbound (site/worker.js, the `/health` branch inside
    `handle`), so a
    deploy that shipped without `/dashboard.png` fails the gate, but an upload
    that never took effect still answers `ok` from the version already live.
    The deploy tool is fetched from the
    npm registry at run time by version tag rather than from a lockfile, and
    it authenticates with whatever Cloudflare credentials the invoking shell
    already holds. Anyone who can run that target with those credentials can
    replace the site carrying the project's download links.

Deployment surface:

- GitHub Actions CI runs gofmt/vet/`go mod tidy -diff`/govulncheck/staticcheck/
  race tests on pushes and PRs, plus biome over the Worker and the jsonc
  configs, `bun test site/` and
  screenshot-script lint (.github/workflows/ci.yml, actions pinned by SHA, bun
  from `.bun-version`, biome at the Makefile `BIOME` pin, uv 0.12.6, the
  interpreter in `.python-version`, Python
  tools from `scripts/requirements-dev.txt`);
  Dependabot updates modules, actions, and `scripts/` pip deps
  (.github/dependabot.yml); tag pushes build release binaries for six
  platforms plus a CycloneDX SBOM (.github/workflows/release.yml, Makefile
  `release` and `sbom`). Release artifacts ship SHA-256 checksums only; no
  signature step exists. Tag names that reach ldflags and dist filenames are
  refused unless they are a safe identifier (release.yml), and a non-dev
  `VERSION` cannot be cut from a dirty or non-git tree at all (M37,
  Makefile `check-release-source`). The
  already-published guard reads `gh release view` with `GH_TOKEN` set from
  the job token and admits only a 404, so an auth failure, a rate limit or an
  outage can no longer read as a free version (release.yml, 44-70; M36).
  The job's last step is the restore drill: `make release-verify` reads the
  published asset list, compares it against what `PLATFORMS` says a version
  holds, and re-verifies every downloaded asset against the release's own
  `checksums.txt`, so an upload that arrived short or altered fails the
  release instead of reaching an installer (release.yml, `release-verify`
  step; Makefile `release-verify`).
  The site deploy credential is documented rather than invented here: the
  operator exports `CLOUDFLARE_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID`, or
  logs in through wrangler's own OAuth store, and no CI job deploys the site
  (CONTRIBUTING.md, 178-191).
- The marketing site is a single Cloudflare Worker serving one static page
  from an embedded string (site/worker.js, just under a thousand lines): the
  worker's exported `fetch` handler is `handle`, and every citation below
  names a symbol or a branch in it rather than a line, because a line in this
  file moves on every page edit. GET/HEAD only (the method guard runs twice,
  once on the image paths and once on the page, and any other method gets 405
  with `allow: GET, HEAD`), a `/health`
  route (a branch of `handle`), a `/favicon.ico` route answered from
  bytes embedded in the Worker itself, needing no asset binding and no
  compression (`FAVICON_PATH`, with `FAVICON_BYTES` and `ICON_ETAG` as its
  constants), weak FNV ETag over
  the page with weak `If-None-Match` matching (`ETAG_HASH`, `ETAG`,
  `ifNoneMatchMatches`, and the 304 answer in `handle`), content negotiation
  (brotli, zstd, gzip, identity) compressed once per isolate, keyed by
  `Vary: Accept-Encoding` on every page response (`VARY`,
  `PAGE_CACHE_CONTROL`, `representationFor`), and hardening headers
  (nosniff, HSTS, referrer-policy, CSP
  `default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:;
  base-uri 'none'; form-action 'none'; frame-ancestors 'none'`) on every
  page, image, health, and favicon response (`SECURITY_HEADERS`; the same set
  adds `x-frame-options: DENY` and an HSTS of `max-age=31536000` with no
  `includeSubDomains`). Every answer also carries
  `server-timing: edge;dur=<ms>`, which names the worker itself, not any
  origin (`serverTiming`). wrangler.jsonc sets
  `assets.run_worker_first` with `html_handling: none` and
  `not_found_handling: none`, so /dashboard.png and related images hit that
  Worker path instead of the asset pipeline; it also publishes the worker on
  workers.dev and binds toktop.ai / www.toktop.ai. `env.ASSETS` is its only
  binding (site/wrangler.jsonc, 12), so the Worker holds no secret;
  `observability.enabled` is true (wrangler.jsonc, 23) and Workers Logs are
  therefore the one place request data is persisted.
  The one forward is on image paths: the Worker re-issues the request to
  `env.ASSETS` carrying the full request URL (so any query string rides
  along) but forwarding only `if-none-match` and `if-modified-since`
  (the header copy in `handle`, immediately before the asset fetch), and an
  asset-store failure
  becomes a one-line text/plain body rather than the store's HTML error
  page (`assetErrorBody`), with `cache-control: no-store`
  on any non-200/304 answer so a 404 cannot stick.
  `style-src 'unsafe-inline'` is idle:
  the HTML is a compile-time string.
  Any path that is neither in `IMAGE_PATHS`, `/health`, nor
  `/favicon.ico` serves
  the marketing page with 200, not a 404 (the catch-all branch of `handle`).
  A client
  that refuses every offered coding gets 406 with `vary: Accept-Encoding`
  (`refusesEveryCoding`, `notAcceptable`). An unbound
  `env.ASSETS` on an image path logs and returns 404
  (the `assets-unbound` branch of `handle`), and any throw becomes a 500 with
  the body `internal error`
  (the `unhandled` catch around `handle`); both answers go out through
  `ERROR_HEADERS`, which is
  otherwise undescribed.
  `/health` is a binding check, not a presence check: with `env.ASSETS`
  unbound it answers 503 with
  `degraded: no asset binding; the dashboard captures are not served`
  rather than `ok` (the `/health` branch of `handle`), because the page still
  serves while every
  capture it shows is a 404. `make site-deploy` polls it with
  `curl -fsS` (Makefile, `wait_for_site`), which fails on that 503, so a
  deploy that
  shipped without its assets cannot report success. What the poll still
  cannot tell is *which* version answered: an upload that never took effect
  leaves an older live version with its bindings answering `ok`.
  Finally, the set of request bytes and strings the Worker inspects is
  larger than a first reading suggests: `logFailure` writes a
  JSON line carrying the caller-controlled `cf-ray` header, plus `method` and
  `path` on the `method-not-allowed`, `assets-unbound`, `asset-missing`,
  `asset-store-error`, and `unhandled` lines (`failRequest`, with the shared
  field set in `requestFields`). The unhandled line adds
  `error: String(err?.message ?? err)`, a thrown message from the asset store
  or the runtime rather than from the caller. The values are JSON-encoded, so
  this is not injection into the log, but a caller can write arbitrary
  strings into the log stream. `REFUSAL_LOG_CAP` and `loggedPerEvent` are
  what keep one caller from writing that many lines: the log is bounded per
  event name.
  Deployment is a local make target, not a CI job: `make site-deploy`
  takes `dist/site.lock`, runs `bunx wrangler@4.126.0 deploy` with ambient
  Cloudflare credentials, records `dist/site.deployed` and the manifest of
  what it uploaded, polls
  `https://toktop.ai/health` 6 times at 10s,
  and points at `make site-rollback` on failure. It depends on `site-lint`,
  `site-check`, `check-wrangler-doc`, and `check-deploy-source` (Makefile,
  the `site-deploy`
  prerequisite list; `check-wrangler-doc` fails the deploy when
  the WRANGLER pin in CONTRIBUTING.md and the one this document names have
  drifted apart; `check-deploy-source` fails it when the Worker or a capture
  it uploads is in no commit), so the same biome lint and
  `bun test` the CI workflow runs gate a local deploy too, and the bytes a
  deploy uploads are ones a checkout can rebuild; `site-rollback`
  has no such dependency, deliberately, so a broken worker can still be
  undone. The lock, the poll and
  the failure exit are shared with `make site-rollback`, so a rollback
  cannot race a deploy and cannot report success over a site that is not
  answering, and the recorded marker makes the rollback itself run once
  (Makefile, the `site-deploy` and `site-rollback` targets and the
  `wait_for_site` function they share).

## Trust boundaries and data flow

- **B1: local processes -> ingest server.** Any process running as any local
  user who can reach the socket can POST events. There is no named
  authentication point; fields are sanitized and clamped
  (internal/ingest/event.go), and two sender classes are refused rather than
  authenticated: POSTs with a nonempty Origin header (M21) and, on a loopback
  bind, requests whose `Host` names anything but this machine (M45), which is
  the class the Origin check cannot see. Request metadata goes to
  stderr subject to `TOKTOP_LOG_LEVEL` (middleware.go).
  Remote address and request id are not authenticated sender identities:
  `X-Request-Id` is caller-controlled, sanitized, and capped at 64 characters
  (middleware.go); non-loopback addresses become `"remote"`
  (middleware.go).
  When `--ingest` binds a non-loopback address this boundary widens to the
  network; the widening is announced at startup (main.go) but not
  prevented.
- **B2: engine HTTP responses -> toktop.** Whatever answers on a scanned
  port, a `--add` URL, or a forwarded remote port is treated as an engine:
  its JSON, Prometheus text, version strings, and error bodies flow into
  parsing and rendering. Validation points: body caps (provider.go),
  non-finite rejection (provider.go), render-time
  sanitization (M2).
- **B3: remote ssh host -> toktop.** A compromised or hostile ssh target
  controls every byte returned by discovery and vitals scripts: `/proc/net/tcp`
  text, cmdline dumps, loadavg/meminfo/CPU/OS/kernel strings, nvidia-smi CSV or
  rocm-smi JSON (stats.go, discover.go). Authentication is the ssh
  handshake itself; after that the data crosses unvalidated except by parsers
  (numbers parse-or-zero, e.g. stats.go and gpu.go; the `/proc/net/tcp`
  sweep parses ports as 16-bit with zero rejected (`parseNetTCP` in
  discover.go), while the shell-probe fallback checks only `p > 0`
  (discover.go, the `strconv.Atoi` in that fallback)
  and the renderer's sanitizer.
  Remote command stderr is sanitized before it is appended to a local error
  (client.go).
- **B3b: local processes -> remote engines (via B3 relays).** Forward's
  loopback listeners (client.go) let any local process send bytes to
  engines on the ssh target. No authentication on the listener; the ssh
  connection is the privilege transition. Closed when the client dies.
- **B4: secrets -> code.** Bearer token: enters via argv or env
  (main.go), lives in a package var (bearer.go), leaves
  in the `Authorization` header of requests bound for origins admitted by
  `Allow`, populated solely from operator-named `--add` URLs
  (cmd/toktop/attach.go, 37, the only `Allow` call site). The token therefore
  rides identification, poll,
  *and* probe requests to those origins (the `Apply` call in
  internal/provider/discover.go, the one in internal/provider/provider.go, and
  the one in internal/probe/probe.go); every other destination is filtered out
  inside `Apply` (bearer.go, 81 and 125), so discovery scans and
  ssh-forwarded remote engines never carry it. `CheckRedirect`
  (bearer.go, 108) strips the header on any hop leaving the original
  origin, and `Set` (bearer.go, 51) discards a token containing CR or
  LF, so neither a redirect nor a hostile env value can move or inject it.
  There is no
  `bearer.Token` accessor; the only outbound path is Apply. It is never
  written to disk or logs.
  SSH password: env or TTY prompt (auth.go), held in memory, used only
  for password and keyboard-interactive mechanisms. Keys: read from disk or
  agent, sign locally. A `~/.ssh` default key that is absent is silent, but
  one that is present and will not load (wrong permissions, a passphrase
  this client cannot ask for) now returns its error instead of degrading
  to a skip, so the chain runs weaker with a record of why
  (`keyFileAuth`, internal/remote/auth.go, 53-66, audited at 233-240); the
  same is true of an agent socket that refuses the connection (auth.go,
  245-254), and both lines fold `$HOME` out of the key path, the socket
  path and the error text (M40). `GITHUB_TOKEN`: env, sent only to
  api.github.com
  (selfupdate.go), with a trailing newline stripped and any other line break
  refused by name (a header value cannot carry one). Rotation points: none;
  all credentials are static
  for the life of the run.
- **B5: build -> runtime.** Two channels replace the executing image:
  hot-reload on Unix trusts that whoever can change the exe file is authorized
  to run code as this user (selfreload.go fires on stat-identity change;
  exec_unix.go), and `toktop update` trusts the named GitHub repo's
  release contents delivered over TLS (selfupdate.go). Both end in execution
  of the replaced binary. Windows hot-reload does not re-exec.
- **B6: user config -> runtime.** `~/.ssh/config` steers where connections go
  and which identity is offered (target.go); `GAUNTLET_HOME` /
  `~/.gauntlet/agents.json` defines which processes count as agents and which
  files are read (cmd/toktop/main.go); `KIMI_CODE_HOME` relocates the kimi
  session store every kimi watcher reads (agentusage/kimi.go, `kimiStore` 82,
  M44 for the absolute-only rule that keeps a relative value from becoming a
  store under the working directory); `PATH` decides which vendor
  CLIs execute (gpu.go, whose stdout is capped at 1 MiB, M33); `USER` /
  `USERNAME` choose the login account and `HOME` / `USERPROFILE` relocates
  every home-relative path, both of which are validated for shape at most
  (M32, gap 10). All are same-user-writable inputs treated as trusted.
- **B7: host filesystem -> agentusage (opt-in).** With `--agents`, process
  discovery plus transcript/SQLite reads cross from other processes' files
  into the dashboard. The operator never names those files; `agents.json`
  roots, crush's walk to `.crush/crush.db` (crush_sqlite.go, capped at
  16 parents), and opencode's well-known path do. SQLite opens are
  read-only (sqlite.go, `_query_only=1`, `_defensive=1`, `_dqs=0`,
  and `trusted_schema=OFF` in the DSN).
  Transcript and crush paths that leave their root via a planted symlink are
  refused (watcher.go; crush_sqlite.go; candidates.go).
  The *content* of a transcript is untrusted too, not just its path: a
  `cwd` read out of the session JSON decides whether that session's tokens
  are credited to the watched directory at all (`owns` at
  agentusage/transcript.go, 263, and `sameDir` at 307; the adapters'
  `sessionCwd` at claude.go, 19 and codex.go, 12-26). A writer of a watched store therefore
  chooses which project's tokens are attributed to which directory, in
  addition to choosing the store's contents.

Privilege transitions: toktop gains no privileges at runtime (no setuid,
no sudo). The ssh connection is the one place code acts with authority beyond
the local process: it executes shell commands on the remote under the target
account (client.go) and opens TCP channels to remote loopback engine
ports that are then exposed on local loopback (client.go).

## Threats per boundary (STRIDE, concrete)

**B1 (and widened B1 via `--ingest`):**
- *Spoofing*: forge agent rows ("claude", "codex") with arbitrary throughput,
  models, and notes by POSTing to /v1/events; no origin auth exists
  (post.go). Renders beside genuine agentwatch data. The one
  sender class refused outright is the browser: a POST carrying an `Origin`
  header gets 403 (post.go), closing cross-origin forgery from web
  pages the operator visits. Origin alone is not the whole browser answer: a
  page that resolves its own name to 127.0.0.1 for one fetch is same-origin
  with a loopback-bound endpoint, so the browser sends no `Origin` and the
  check never fires. The `Host` header survives that rebinding carrying the
  attacker's own name, and on a loopback bind the host guard refuses it with
  403 ahead of the route table (M45, `loopbackHostGuard`), so the rebinding
  sender class is refused on the default bind too. The guard is deliberately
  absent from a `--ingest` bound to a routable address, where peers name the
  box whatever they like, and a rebinding page can only reach that bind
  through the loopback interface in any case. Timestamps more than 2 minutes from arrival in
  either direction are clamped to arrival time (stream.go): ahead, so the
  "live" marker cannot be pinned by a claimed far-future stamp, and behind,
  because a lagging sender's events sort to the front of the retained feed
  and are then refused as outside the window. Mixed Latin+Cyrillic/Greek
  agent names collapse to `anonymous` (event.go).
- *Repudiation*: POST handlers emit remote, request id, status, accepted
  count, and stored count at info on success, warn for rejection/write
  errors, and error for
  5xx (internal/ingest/middleware.go). A warn/error log
  floor suppresses successes; error also suppresses 4xx rejections. Request
  ids are caller-supplied correlation labels, not evidence of identity
  (middleware.go). Event bodies are excluded and non-loopback IPs are
  redacted. The stored count is what the retained feed took, so a replayed
  id is visible as `accepted` above `stored` (post.go;
  internal/collector/collector.go).
- *Information disclosure*: none beyond presence (`/healthz` answers any
  requester, health.go). The route table and the `Allow` header are learned
  only by a request the host guard admits, since it runs ahead of both the
  404 and the 405 (M45). Residual: a routable bind would otherwise have
  put peer IPs on stderr; logRemote drops them.
- *Denial of service*: mitigated (see M3-M5, M30). Bounded three ways per
  connection (byte cap, 1 min body-idle, 10 min absolute lifetime) and a
  fourth across the endpoint: at most `maxInFlightEvents` 64 POST bodies are
  decoded at any instant, process-wide, and a POST arriving when every slot is
  held is refused at once with 503 and `Retry-After` rather than read
  (M30: `eventSlots`, post.go). Residual: the cap is a
  count, not a per-peer
  quota, so 64 stalled local POSTs still hold 64 descriptors and goroutines
  and the 65th legitimate sender is refused with them (gap 5); nothing
  attributes a slot to a peer, so one process can occupy all of them.
  MaxHeaderBytes is 16 KiB (`maxHeaderBytes`, server.go).
- *Elevation of privilege*: none; event content reaches only parsing and
  rendering.

**B2:**
- *Spoofing/tampering*: a hostile listener on a scanned port can pose as an
  engine and feed fake metrics, versions, and model lists (display-only
  effect). Fingerprinting order in identify (discover.go) decides the
  label; mislabeling is possible, impact is a wrong row.
- *Information disclosure*: scoped since commit 21e3feb. Scanned ports and
  forwarded remotes receive no Authorization header because they were never
  Allow-ed (bearer.go). Residual: the token is still exposed to whatever
  listens on an origin the operator explicitly `--add`ed; that is inherent to
  attaching to an endpoint, not a defect, but it means `--add` trust equals
  credential disclosure to that origin.
- *Denial of service*: slow responses bounded by scanTimeout 700ms /
  PollTimeout 1.5s / probe timeout 30s; bodies capped (M8), identification
  decoders included: `decodeScanJSON` puts the same `jsonBodyMax` LimitReader
  in front of `json.NewDecoder` (`decodeScanJSON`, internal/provider/discover.go) that the
  poll path uses, so a listener that answers a scan can no longer stream
  without bound inside the 700ms window. Probe
  generation is 32 tokens with think disabled, a content-byte hang-up, a
  16 KiB line cap, and 429/503 backoff (M10).
- *Elevation*: none; response bytes never reach execution or unsanitized
  output. Probe interpolates a capped model id (probe.go).

**B3:**
- *Spoofing*: TOFU pins nothing on first contact; a MITM present before the
  first connect is pinned as genuine instead (knownhosts.go). Key change
  refusal is loud and dual-fingerprinted (knownhosts.go). Handshake
  algorithms are the library SupportedAlgorithms set, which omits ssh-rsa
  (SHA-1) and DSA (client.go).
- *Tampering/information disclosure*: hostile remote shapes vitals, GPU names,
  and engine lists shown locally; sanitized at render (M2), numbers parse
  defensively. Residual risk is misleading content, not injection.
- *Denial of service*: remote can stall each poll up to runTimeout 15s
  (client.go) and the handshake up to bannerTimeout 15s
  (client.go); keepalive turns silent death into a closed connection
  within ~45s (client.go).
- *Elevation*: remote input never becomes local command text; remote-side
  scripts interpolate only locally generated integers (probeScript,
  discover.go), so no injection path into the remote shell either.
  Discovery output mostly cannot plant impossible tunnels either: listening
  ports from the `/proc/net/tcp` sweep parse as 16-bit with zero rejected
  (`parseNetTCP` in discover.go, fuzz-pinned by FuzzParseDiscoveryOutput), so a
  hostile /proc dump cannot name an out-of-range forward target. The
  shell-probe fallback that runs when that sweep yields nothing is not
  bounded the same way (gap 8).

**B3b:**
- *Spoofing/tampering*: a local process that finds the ephemeral port can
  issue completions, list models, or otherwise drive the remote engine as if
  it were local. Typical targets (ollama, llama.cpp, vLLM on loopback) have
  no auth of their own.
- *Information disclosure*: model lists, generated text, and whatever else
  the remote engine will serve to localhost now leave that host toward any
  local peer of toktop.
- *Denial of service*: the same process can burn remote GPU/queue via the
  relay; probe generation is capped (M10) but a hostile local client of the
  relay is not. Opening the tunneled channel is bounded (forwardDialTimeout).
- *Elevation*: the ssh session's authority to reach remote loopback is
  inherited by every local process that can connect to 127.0.0.1.

**B4 (secrets):**
- *Information disclosure*: token passed via `--bearer` is visible in process
  listings. README documents the env fallback for exactly this reason, and
  warnBearerFlag names it at startup (validate.go).
  `TOKTOP_SSH_PASSWORD` in the environment is readable by same-user processes
  and inherited by children (auth.go); `GITHUB_TOKEN` reaches api.github.com
  on every `toktop update` check when set (selfupdate.go).
- *Tampering*: known_hosts store is plain text in the user config dir, mode
  0600 in a 0700 dir (knownhosts.go); a same-user writer can reset pins.
  Writes are serialized within and across processes (knownHostsMu plus a
  `known_hosts.lock` held across the whole read-modify-write) and go through
  a temp file plus rename, with the directory entry flushed so a crash cannot
  revert to the previous store. A record that does not parse, or a host
  recorded twice with different keys, fails the read: neither is skipped,
  since skipping turns corruption or an appended line into a forced re-TOFU.

**B5 (build/runtime, both replacement channels):**
- *Elevation of privilege*: swapping the exe file converts Unix hot-reload
  into arbitrary code execution as the running user (exec_unix.go);
  substituting `nvidia-smi` et al. via `PATH` runs attacker code with user
  privileges (gpu.go). Both require write access the user already
  controls; they matter when toktop runs with more authority than the
  attacker has (none today, noted for future privilege changes). Windows
  hot-reload cannot take this path (exec_windows.go).
- *Spoofing/tampering* (`toktop update`): the release lookup and downloads
  ride TLS. `ValidateRepo` admits only a GitHub owner/name
  (selfupdate.go, 126); `Check` builds the URL with `url.JoinPath`
  (237). Asset URLs must be GitHub download hosts, and redirects off
  those hosts are refused (selfupdate.go, `githubRedirect` 212 and the
  `trustedAssetURL` gate in `applyTo` 347). The asset
  must match the release's own checksums.txt (`applyTo`, the `ChecksumFor`
comparison at 359), so a
  network attacker cannot
  substitute a binary. The trust anchor is the repo itself: whoever controls
  the repo (or its release pipeline) ships a binary that verifies against its
  own checksums. No external signature (Sigstore/GPG) closes that gap today;
  summary risk 3.
- *Denial of service*: downloads capped at 256 MiB asset (`maxAssetBytes`,
  selfupdate.go, :64) / 1 MiB compressed checksums / 2 MiB decompressed
  checksums;
  a truncated or hostile mirror fails verification and leaves the running
  binary untouched (`applyTo`).

**B6 (user configs):**
- *Elevation of privilege*: `--repo owner/name` redirects the update channel
  to an arbitrary public repo (after ValidateRepo); combined with the
  self-published-checksum trust anchor above, installing from a hostile repo
  is operator-invoked arbitrary code installation. `agents.json` chooses
  which files toktop reads every second; a same-user writer can point it at
  anything readable, and withdrawing an entry stops the read instead
  of leaving the previous adapter on the store it disowned (M29).

**B7 (agent files, `--agents`):**
- *Information disclosure*: toktop reads whatever transcript roots
  `agents.json` names, plus crush project databases found by walking up from
  a watched cwd, plus (with `--opencode-db`, on by default) the operator-wide
  opencode store. Another local user whose files are readable (shared group,
  overly loose home perms, toktop run as root) has those session counters and
  working-directory tails rendered. `--agents` is off by default for this
  reason (main.go).
- *Tampering*: a writable transcript or crush/opencode database can inject
  dashboard rows the same way ingest can; sanitization still applies at
  ingest-equivalent recording and at render (M2). Planted symlinks out of
  the transcript root or crush project are refused (M29). Separately, a
  writable store chooses its own `cwd` header, so it can claim a session
  that belongs to another project or disown one that does
  (transcript.go, 263 and 307); ownership only moves token counts between
  directory rows, it does not cross a trust boundary.
- *Information disclosure*: on Linux the same-engine attribution walks
  `/proc/<pid>/fd` of other processes and reads the kernel TCP tables
  (peers_linux.go, 72-126). Another user's pid returns nothing (the read
  fails and is reported as unknown), but where pids are readable to the
  operator the remote endpoints of those processes are read into the
  attribution logic. The result is used for a same-engine verdict and is not
  rendered as inventory.
- *Denial of service*: a huge transcript or database is opened read-only and
  queried with bounds (maxSaneTokens 1<<40, agentusage/sample.go);
  JSONL is tailed from the attach point rather than replayed in full
  (agentusage/watcher.go). One transcript record is capped at `maxLineBytes`
  8 MiB with a 64 KiB `appendReaderBytes` fill
  (agentusage/transcript.go, 108-171, with the caps declared at 165-171),
  and the ownership scan is capped at
  `ownerScanLines` (agentusage/transcript.go, 250), with a record-free scan refused durably rather than
  re-read every poll. dsh zstd frames are size-capped
  (`zstdMaxFrameBytes`, `WithDecoderMaxMemory` in agentusage/dsh.go).

## Existing mitigations map

Controls verified in code, with the threats they cover:

| Control | Covers | Location |
|---|---|---|
| M1: Origin-scoped bearer application. Token attached only to `Allow`-admitted origins, populated exclusively from operator `--add` URLs; `CheckRedirect` refuses the hop outright on any origin change, off an allowed origin, or to a non-http destination, so the header is never carried to the new target, capped at 10 hops; `Set` refuses a token containing CR or LF and leaves the stored one unchanged, so an argv or env value cannot inject a header | Credential harvesting by scanned ports, forwarded remotes, and probes (B2/B4 disclosure; previous summary risk 1, fixed in commit 21e3feb); credential loss to a redirect target; header injection through a hostile token | internal/bearer/bearer.go, `Set` 51, `CheckRedirect` 108, `admits` 125; cmd/toktop/attach.go, 37; call sites the `bearer.Apply` in provider/discover.go, provider.go and probe.go; tests internal/bearer/bearer_test.go |
| M2: Terminal escape/control-char sanitizer applied both at ingest and at render time (C0/C1 including UTF-8-encoded C1; bidi overrides/isolates; zero-width format; variation selectors), plus `core.SingleLine` for a field that occupies one cell, one report line or one log line: it folds the newlines and tabs `SanitizeText` keeps for block text into single spaces, because every renderer here measures a cell by its widest line and splits a rendered row on newlines into frame rows. Without it, a sender reaching the unauthenticated ingest endpoint or a process squatting a discovered port could print a line the dashboard reads as its own output ("up 9/9 engines", "agent stopped") and spend row budget that pushes the real rows off the pane. Mixed Latin+Cyrillic/Greek agent names collapse to `anonymous`. Every one of the three renderings applies the sanitizer, including the machine-readable one, so the JSON report is no less sanitized than the frame | OSC/CSI clipboard-cursor-title injection from engines, remotes, and events; a forged own-output line and row-budget displacement in the frame; mixed-script impersonation of agent names (asset: terminal integrity, dashboard integrity) | internal/core/sanitize.go, `SingleLine` 106; ingest side internal/ingest/event.go, the field-clamp block in the event sanitizer (`AgentNameField`, `SingleLine`, `RedactHome`); render side internal/ui/ui.go, internal/ui/plain.go, internal/ui/format.go, internal/ui/agents.go, internal/ui/json.go; model ids go through `core.ModelName` (M34); and the reason phrase an engine or the GitHub API returns is folded to one cell where it becomes an error text (`core.HTTPStatus`, internal/core/truncate.go, called from internal/provider/provider.go, internal/probe/probe.go and internal/selfupdate/selfupdate.go), since Go reads that line off the wire and trims only the leading space, so a listener squatting a discovered port could otherwise put a tab or a second line into a message the operator reads |
| M3: Ingest body cap 1 MiB + MaxBytesReader | unbounded upload into decode loop (B1 DoS) | post.go |
| M4: Ingest read deadlines: 10 min absolute lifetime, 1 min idle extension, 5 s header timeout, 2 min idle reap, 30 s response write deadline armed before every error/response write (server.go: `newServer` and the `http.Server` field block; post.go: `progressBody` and the per-response `armWrite` closure, which arms the write deadline before every answer and clears it on keep-alive) | slowloris/drip DoS and stalled-response resource pinning (B1) | server.go; post.go |
| M5: Event field caps (an `id` past 128 characters, or one that is nothing but whitespace or control characters, is refused with a `400` naming the field rather than clamped, since the id is the dedup key and a clamped one folds distinct keys onto one stored id; the display fields clamp: agent 64, model 128, via_engine 128, note 512; a kind outside the four known ones is lowercased, sanitized and clamped to 24 runes rather than replaced, so `kind: "banana"` reaches the feed as `banana` (folded to one line by `core.SingleLine`, so a kind cannot add a row); only a kind that is empty or sanitizes to empty becomes `turn`) + a note that is nothing but a working directory reduced to its last two components and folded through `core.RedactHome`, so a pushed path cannot name the account in a dashboard the operator redirects into a file (`shortNote`/`pathNote`, event.go, 90-130) + token clamp (negative or >1<<40 to zero) + retention caps (512 events, 128 probes per snapshot) + event-id dedup ledger bounded by a 15 min horizon and a 4096-entry cap, so a replay stays recognizable past the display ring and the ledger cannot grow without bound | memory pinning via oversized or numerous events; wrap of agent totals (B1 DoS/tampering); a dropped event from two ids clamped onto one dedup key; a pushed `$HOME` path in a retained note reaching a shared report | event.go:45-71, 90-130, 121-137, 183-189; core.AgentHistoryLen / ProbeHistoryLen internal/core/core.go; internal/collector/agents.go; collector.go |
| M6: Event timestamp skew clamp: stamps >2 min from arrival in either direction reset to arrival time | forged-future stamps pinning the live marker and feed ordering (B1 spoofing); a lagging sender's events refused as outside the retention window | stream.go |
| M7: Negative or absurd token counts clamped to zero by `core.ClampEventTokens` on all three counters; an unrecognized `kind` is kept as sent (ASCII-folded, single-line, clamped to 24 runes) rather than replaced, only an empty or sanitized-to-empty kind becoming `turn`, because a sender that spells a known kind with U+0130 or U+212A must not have it silently rewritten into a different kind (`core.AgentKindError` drives a red row) | junk values entering retained state (B1 tampering); a known kind rewritten by case folding into another | internal/ingest/event.go, the `core.ClampEventTokens` calls and the `switch ev.Kind` block; core.ClampEventTokens internal/core/core.go |
| M8: Engine response caps: 4 MiB JSON, 8 MiB text, 256-rune error snippets. The JSON cap covers discovery as well as polling: `decodeScanJSON` wraps `resp.Body` in `io.LimitReader(resp.Body, jsonBodyMax)` before `json.NewDecoder`, so a listener that answers a well-known-port scan is bounded by bytes, not only by the 700ms `scanTimeout` window | memory blowup and log flooding from hostile engines (B2 DoS/disclosure) | provider/provider.go, `jsonBodyMax` 40, `textCap` 152, and the `getJSON` LimitReader that bounds every poll decode; core.Snippet, `SnippetCap` 256; provider/discover.go, `decodeScanJSON` |
| M9: Non-finite rejection in metrics (per-value and a family-sum overflow guard) and numeric-text coercion of the vendor's Prometheus text exposition (`parseProm`, `strconv.ParseFloat` behind `finite()`) and its JSON replies, both bounded by the body caps of M8; a counter that goes backwards yields a rate clamped to zero rather than a negative one | poisoned counters/rates propagating through history (B2 tampering) | provider/provider.go, `parseProm` 376 and the family-sum guard; internal/gpu/gpu.go, the finite check; internal/collector/rates.go, the counter-reset clamp |
| M10: Probes request 32 tokens, Ollama `think=false`, OpenAI `n=1`; streaming readers stop after 32 observed content/reasoning units or 1 KiB decoded content/reasoning, with 16 KiB scanner and 128 KiB stream limits. Non-stream OpenAI JSON over 16 KiB is rejected. Reported token counts trusted only up to 128; fixed prompt, model-id cap `core.ModelNameMax` 256 (M34, the same constant the provider layer stores ids under), embed/rerank filtering, 429/503 backoff 15s–5m, and a wave cap of 4 backends per wave with a rotating cursor, so one `--probe` tick on a wide fleet cannot fan out one billed generation per backend at once. A wire-reported `eval_duration` is passed through `nanoseconds`, which returns zero for a negative value, one outside the int64 nanosecond range, or one beyond `maxEvalDuration` 24h, so a count that would wrap into a plausible duration cannot reach `fitEvalDuration`'s band rescale and report a throughput the engine never claimed | Limits client work and reduces B2 compute/spend amplification; request parameters and closing a response do not guarantee that a backend stops generation or billing; a hostile engine's reported durations cannot wrap into a plausible rate (B2 tampering) | internal/probe/probe.go |
| M11: Poll/scan/probe timeouts (700ms/1.5s/30s) + context-bounded requests | hung-engine DoS (B2/B3) | discover.go; provider.go; probe.go |
| M12: TOFU host-key store with loud change refusal, 0600 file in 0700 dir, writes serialized within and across processes (mutex plus `known_hosts.lock` over the whole read-modify-write, stale locks broken), temp-file plus rename with the directory entry flushed (core.SyncDir, shared with the self-update install), and unparsable, conflicting-duplicate or record-free stores failing the read. The repair copies beside the store (`known_hosts.bak`, the displaced copy) are consulted and restored only where `interruptedWrite` finds the marks of a write that did not finish, so a store an operator deleted on purpose re-pins on the next connect instead of having the key they just rejected silently pinned back | silent MITM after first contact (B3 spoofing); lost pins under concurrent Connect, in one process or two; corruption or an appended line read as a shorter store, forcing a re-TOFU; a deliberate re-pin undone into the rejected key | knownhosts.go, `readKnownHosts`, `interruptedWrite`, `restoreStore`; core/fs.go |
| M13: Banner deadline lifted only on complete version line; 15s command timeout; keepalive with bounded probe waits; SupportedAlgorithms (no ssh-rsa SHA-1 / DSA); forwardDialTimeout 8s | trickle/silent-peer hangs (B3 DoS); weak host-key algorithms; hung tunnel dial (B3b) | internal/remote/client.go, the `handshakeConn` type and its `Read`, `bannerTimeout`, and the `SupportedAlgorithms` filter; internal/remote/forward.go, `forwardDialTimeout` |
| M14: Local-only defaults: forward listeners on 127.0.0.1, ingest on 127.0.0.1:8420 | accidental network exposure (B1 widening; B3b stays local) | client.go; main.go |
| M15: Routable-bind warning at ingest startup | silent widening of B1 to the network (visibility control; the widening itself remains possible) | endpoints.go |
| M16: Remote shell scripts: static bodies, only locally generated integers interpolated; no secret material sent to remote scripts | command injection into remote shell (B3 elevation) | discover.go; stats.go |
| M17: Password prompt gated on TTY; encrypted keys skipped with guidance. A remote's stderr is quoted into a local error only as a sanitized tail of `stderrTailClusters` 300 grapheme clusters (`stderrTail`, internal/remote/session.go, 207-228, over `core.TailClusters`), and that error string is home-folded before it reaches a frame the operator is told to paste into an issue (internal/remote/stats.go, `poll` 181, the `core.RedactHome(core.Snippet(...))` at 200), so a hostile remote cannot flood the local error or put an operator home path into a diagnostic report through a command that fails | credential handling in headless runs (B4); unbounded remote-chosen text in a displayed error, and a home path in a reported one (B3/B4 disclosure) | internal/remote/auth.go, `answerPasswordPrompt`; internal/remote/session.go, `stderrTail`; core/truncate.go, `TailClusters` 49 |
| M18: Self-update verification: ValidateRepo (owner/name charset, no path/query), url.JoinPath, GitHub-host asset URLs, redirect pin, refuses without checksums asset, SHA-256 match required before rename, 256 MiB size cap, 2 MiB decompressed checksums cap, temp-file-plus-atomic-rename install. `toktop update --check` prints `rel.HTMLURL` for shell capture, and that page URL is held to the same GitHub-host rule as an asset (`TrustedReleaseURL`); a release naming no GitHub page is reported and nothing is printed for the `url=$(toktop update --check)` capture the help screen documents | path traversal / SSRF / tampered/truncated/unbounded/gzip-bomb downloads reaching execution, and a release-supplied URL captured into a shell variable (B5) | selfupdate.go, `ValidateRepo`, `Check`, `applyTo`, `githubRedirect` and `TrustedReleaseURL`; cmd/toktop/update.go, `reportRelease` |
| M19: Flag validation exits 2; `--interval` below 50ms or above 1h rejected (bare numbers are nanoseconds); set-but-invalid `TOKTOP_COLUMNS`/`TOKTOP_LINES` (outside 41-1024 / 21-512) exit 2 under `--once` (and are named as ignored without it); non-TTY stdout aborts the live dashboard; a missing `agents.json` is a no-op but a malformed one exits 2 rather than watching a reduced agent set; unknown `TOKTOP_*` env warned; empty `--ingest` rejected; `--add` userinfo, query and fragment rejected; `--origin` parsed as RFC3339 or Unix seconds and refused when malformed, and both `--seed` and `--origin` named as having no effect without `--demo`; startup config line redacts bearer | misconfiguration acting as silent security-relevant behavior change: empty ingest bind exposing every interface, unitless `--interval 1` hammering engines, oversized `--once` frame OOM, a silently reduced `--agents` watch set, a `?api_key=` credential in argv and in every rendered surface, and a demo replay that differs from the capture it was meant to reproduce | validate.go validateFlags, validateOnceEnv, validateIngestAddr, warnIgnoredFlags, parseOrigin; endpoints.go validateAddURL; main.go logActiveConfig | 
| M20: Supply chain: govulncheck in CI, Dependabot, SHA-pinned workflow actions, SBOM in releases, tag-name identifier check, the release-source gate (M37), and two drift gates that make the CI build matrix and its build tags answerable to the Makefile targets the release job runs (`make check-ci-platforms` at .github/workflows/ci.yml, 89, `make check-ci-tags` at 74), so a platform that stops being vetted in CI cannot silently keep being published. CI runs with `permissions: contents: read` (.github/workflows/ci.yml, 8); the release workflow holds `contents: write` for its whole (single) job rather than for the publish step alone, because Actions accepts permissions on the workflow or the job and not on a step (.github/workflows/release.yml, 10-11 and 141-147), so the scope that can push a tag and its assets is the scope every step of that job runs with, and it is the one token a workflow or runner compromise would use to poison the update channel of summary risk 3 | vulnerable-dependency drift, an unvetted shipped platform, and release-channel write scope (deployment surface) | .github/workflows/ci.yml, .github/dependabot.yml, .github/workflows/release.yml, 11 and 147, Makefile |
| M21: Ingest POSTs carrying an `Origin` header refused with 403 (browsers always send Origin on cross-site writes; scripts and agents never do; the endpoint's Content-Type blindness would otherwise let `text/plain` POSTs sail past CORS preflight). On its own this closes cross-origin forgery and nothing else: a page resolving its own name to 127.0.0.1 for one fetch is same-origin with this endpoint, so the browser sends no `Origin` and M21 never fires. M45 is the control that closes that sender class | browser-driven dashboard forgery from any visited web page (B1 spoofing) | post.go, the `Origin` check in `handlePost`; tests internal/ingest/server_test.go; README "Agent feed API" documents it |
| M22: Remote discovery ports from the `/proc/net/tcp` sweep parsed as 16-bit with port 0 rejected, so hostile `/proc/net/tcp` output cannot plant impossible forward targets; pinned by FuzzParseDiscoveryOutput. Not covered: the shell-probe fallback taken when that sweep returns nothing parses the remote's stdout with `strconv.Atoi` and checks only `p > 0` (`strconv.Atoi` in the shell-probe fallback, discover.go), so a hostile remote answering on that path can put an out-of-range port into `Discovery.Listening` and have it forwarded (gap 8) | tunnel-set manipulation by a hostile ssh remote (B3 elevation/DoS) | remote/discover.go; internal/remote/fuzz_test.go |
| M23: `--agents` opt-in; `--opencode-db` is a second gate on top of the `sqlite` build tag, on by default with `--agents` and turned off with `--opencode-db=false`; crush has no extra flag because the database lives in the watched project | silent process/file scan the operator did not ask for (B7 disclosure) | main.go; agentusage/source.go; crush_sqlite.go |
| M24: SQLite session stores opened `mode=ro` with `_query_only=1`, `_defensive=1`, `_dqs=0`, and `trusted_schema=OFF`; crush walk capped at 16 parents; counters rejected above 1<<40; opencode directory list bound as parameters | accidental writes into agent databases, planted-schema SQL during a read, walk-to-root, overflow, and SQL injection via cwd (B7) | agentusage/sqlite.go; crush_sqlite.go; sample.go; opencode_sqlite.go |
| M25: Structured request metadata to stderr; bodies excluded; caller-controlled X-Request-Id provides correlation only | B1 repudiation: successful POSTs visible at debug/info, suppressed at warn/error; error also suppresses 4xx. No durable storage or authenticated sender attribution | internal/ingest/middleware.go; internal/ingest/server_test.go |
| M26: Ingest response headers (nosniff, DENY framing, CSP `default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'`, CORP same-origin, `Cache-Control: no-store`, `Referrer-Policy: no-referrer`) and MaxHeaderBytes 16 KiB | a fetched JSON body sniffed as HTML or framed when `--ingest` is exposed (B1 disclosure); header-bomb DoS | server.go, `maxHeaderBytes`; middleware.go, `setSecurityHeaders` |
| M27: `logcfg.Remote` rewrites a non-loopback peer address to `"remote"` on the audit line, and `logcfg.RedactAddrs`/`RedactHandler` apply the same rewrite to text in the message, so a peer IP cannot reach stderr even when `http.Server.ErrorLog` interpolates it into a handshake or panic line | peer-IP disclosure when `--ingest` is bound off loopback (B1 information disclosure) | internal/logcfg/logcfg.go, `Remote` 172, `RedactAddrs` 190, `RedactHandler` 203; server.go, the `ErrorLog` field of the `http.Server` |
| M28: Keyboard-interactive answers only a single non-echoing prompt | a hostile sshd harvesting the password across extra or echoing prompts (B4 disclosure) | auth.go |
| M29: Transcript, candidate-walk, and crush paths opened under `os.OpenRoot`; a planted symlink out of the store is refused. The ownership scan is capped at `ownerScanLines` and refused durably when no header is found, so an undecided session is not re-read on every poll. A definition withdrawn from `agents.json` takes its adapter with it: `refreshAdapter` returns without one and `walkCandidates` stops (`adGone`, agentusage/candidates.go, 361), so a reload that drops an agent cannot leave the previous adapter walking the store it disowned for the life of the process | same-user (or writable-store) redirect of `--agents` reads to arbitrary files (B7 disclosure) | agentusage/watcher.go, 420; agentusage/candidates.go, 294 (`walkTranscripts`); agentusage/crush_sqlite.go, 124; agentusage/registry.go, `refreshAdapter` 396 |
| M30: In-flight POST cap. `eventSlots` (post.go) is a process-wide counting channel of `maxInFlightEvents` 64 acquired before the body is read; a POST that cannot take a slot is refused immediately with 503 and one shared `Retry-After: 1` (RFC 9110 whole seconds) rather than queued. `handleHealth` reports the same degraded state with the same delay, so a probe that keeps saying `ok` while every POST is refused cannot be believed. `stallReason` names which of the two 408 bounds broke (`no body bytes for 1m` vs `stream exceeded the 10m lifetime`), since the raw i/o timeout is identical for both and the two have opposite sender-side fixes | a local pile-up of stalled POST bodies holding an fd and goroutine each for a minute (B1 DoS); a sender or health probe inventing a shorter retry interval than the slots free (B1 DoS) | internal/ingest/post.go, `maxInFlightEvents`, `eventSlots`, `stallReason`, the shared `retryAfterSeconds`, and the 408 body in `handlePost`; internal/ingest/health.go, `handleHealth` |
| M31: `--ssh-key` is rejected when set to an empty value (exit 2). An empty value is indistinguishable from the flag never being given: the run silently fell back to `~/.ssh/config` and authenticated with a key the operator did not choose | silent identity substitution from a mistyped or quoted-empty flag (B4 disclosure) | cmd/toktop/validate.go, `validateSSHKeyFlag`; cmd/toktop/main.go, the call from `main` |
| M32: `currentUser` validates `USER` and `USERNAME` through `validTargetField` before handing either to the transport and falls through to the passwd database when either is empty or would fail validation; the passwd name is passed through `basenameLogin` so a Windows `DOMAIN\user` yields `user`. `TOKTOP_SSH_PASSWORD` moved behind the exported `remote.PasswordEnv` constant so the startup warning that names an unusable variable spells it the way the code reads it | a hostile environment redirecting the ssh connection to a different account, or naming an invalid string the transport would reject later as a password failure (B6) | internal/remote/client.go, `currentUser` 118-128; internal/remote/target.go, `validTargetField` 369; internal/remote/auth.go, `PasswordEnv` 99; cmd/toktop/validate.go, the `remote.PasswordEnv` warning at 234-238 |
| M33: A repeated `ssh://` target resolves to one attachment, keeping the first, keyed on ASCII-folded host plus user, port, and key file; `Forward` reuses the listener a port already has and returns the same local port instead of binding a second one that no map entry reaches. Vendor CLI stdout is capped at `maxToolOutput` 1 MiB through a `cappedOutput` writer that fails the write and reports a miss; over the cap the tool's read end closes under it | one host attached twice: a second ssh connection, a second set of loopback relays (widening B3b), and double-counted engine rows, since the UI keys rates by endpoint (B3b availability and dashboard integrity); unbounded memory growth from a wedged or hostile `nvidia-smi` on `$PATH`, sampled several times per tick (B5 DoS) | cmd/toktop/main.go, 149 (`remote.ParseTargets`); internal/remote/target.go, the dedup key at 104; internal/remote/forward.go, the listener reuse at 68-71; internal/gpu/gpu.go, `maxToolOutput` 227 and the `cappedOutput` writer at 230-236 |
| M34: One bound for every engine-supplied model id: `core.ModelNameMax` 256 grapheme clusters, applied through `core.ModelName` (trim, sanitize, cap) at each place a listing or health response becomes a `ModelInfo`, and reused as the probe's own cap so a name that reached a snapshot is one the probe sends unchanged. A `/v1/models` answering with megabyte strings can no longer ride every snapshot, every probe body and the `--json` report at full length | a hostile engine using a single field to inflate memory, log, and report size on every poll (B2 DoS/disclosure) | internal/core/truncate.go, `ModelNameMax` 91, `ModelName` 104; call sites the `core.ModelName` folds in internal/provider/openai.go, internal/provider/ollama.go, and the one in internal/probe/probe.go |
| M35: Redaction in the log handler rather than at each call site. `logcfg.Logger` wraps stderr in `HomeHandler`, which folds `$HOME` to `~` in every record message and every top-level string attribute, so a path written by code that never thought about disclosure (a request path, a rejected header, a library error) is still folded. `logcfg.Field` sanitizes, collapses whitespace so a payload cannot split a line, and caps an attribute before it is logged. All six audit loggers build theirs from that one function (internal/ingest/server.go, 64, the `logcfg.Logger()` call in `New`; internal/remote/client.go, 33; internal/collector/collector.go, 38; internal/gpu/gpu.go, 206; cmd/toktop/attach.go, 28; and the agentusage package's own logger, handed over by `agentusage.SetLogger(logcfg.Logger())` at cmd/toktop/main.go, 118, which is what its walk-failure line goes through), so the redaction reaches the ssh, engine-state, GPU, attach and agent-watch lines too, and the ssh client's own audit lines additionally run `RedactAddrs` over the text through `logcfg.RedactedField` (client.go, 237-455). Documented limits: group attributes are not walked and attributes bound with `WithAttrs` before the wrap are not reached, since the inner handler owns them; a destination the operator typed (an engine `addr`) is not a peer address, so nothing short of the home fold removes it, while an ssh `target` is stripped of its account at the call site before the fold ever sees it (M40); and the one payload that quotes text the process did not author, the recovered panic value and its stack, is folded at the call site instead (M46) rather than by the handler, since `logcfg.Field` alone would not reach the message | the operator pasting a diagnostic line into an issue and publishing the account name inside it, or the names of the hosts and gateways the run polls; a caller-shaped attribute splitting or padding an audit line (B1/B4 disclosure, response readiness) | internal/logcfg/logcfg.go, `Logger` 77, `HomeHandler` 101, `Field` 167, `RedactedField` 201; internal/core/redact.go, `RedactHome` 24 |
| M37: `make release` refuses a non-dev `VERSION` built from a tree that cannot be tied to the bytes shipped. `check-release-source` fails the cut when `git rev-parse HEAD` fails (a source export, where buildinfo has no commit to record and `SOURCE_DATE_EPOCH` falls back to 0, dating every archive member to the epoch) and, unless `ALLOW_DIRTY=1` is passed by name, when `git status --porcelain` is nonempty; a non-numeric `SOURCE_DATE_EPOCH` is refused too. `VERSION=dev` is exempt, since a dev build is a local artifact whose manifest records the tree honestly. This is the reproducibility half of the update channel's trust story: an artifact whose bytes do not match the commit the release page points at cannot be re-derived or audited by anyone who downloads it | a release cut from an uncommitted or non-git tree, shipping binaries that buildinfo names a commit for while the archive members carry something else, against the same trust anchor as summary risk 3 (B5 tampering/repudiation) | Makefile, `check-release-source`, a prerequisite of `release` |
| M38: One watcher per agent store. `discover` stops the trackers of exited processes before it installs a follower onto a store a dead tracker was still tailing (`for _, t := range exited { w.stopOne(t) }` ahead of the install, internal/agentwatch/agentwatch.go, 270-272), and `stopOne` reports the dead watcher's final growth first, so the follower's baseline is taken after the stopped tracker's last sample. Two watchers on one store each report the same growth under their own PID, and the collector's id window cannot merge two sample ids, so every token written in the overlap is counted twice | doubled token counts and a follower whose baseline overlaps the partition it replaced: a dashboard-integrity defect on a path a process exit alone triggers, with no hostile input (B7 tampering) | internal/agentwatch/agentwatch.go, 275-277; test internal/agentwatch/handover_test.go |
| M39: Clocks and callbacks a running goroutine reads are taken under a lock rather than raced. The ingest server's `SetNow` writes `s.now` under `nowMu` and `instant` reads it under `RLock` (internal/ingest/server.go, `nowMu`, `SetNow`, `instant`); the agent watcher guards the two fields `SetNow` and `SetOnError` write with `clockMu`, because every tracker's own reader goroutine stamps events from that clock (internal/agentwatch/agentwatch.go, `clockMu` with `reportError` releasing the lock before calling a caller-supplied sink); `gpu.Sample` collects its per-tool results under a local mutex; the host-vitals sampler reads its clock through `instant`, which takes `clockMu` before calling the caller's function, so a `SetNow` swap cannot tear against a cache window being aged (internal/sysmon/sysmon.go, `clockMu`, `SetNow`, `instant`) | a data race on the demo clock or the error sink between a handler goroutine and a tracker, which is a correctness and availability fault rather than a boundary crossing, recorded here because the ingest clock is set from demo mode and a race there lands in the same audit surface as B1 | internal/ingest/server.go, `nowMu`, `SetNow`, `instant`; internal/agentwatch/agentwatch.go, `clockMu`, `reportError`; internal/gpu/gpu.go, `Sample` 249 (the local mutex at 251-262); internal/sysmon/sysmon.go, `clockMu`, `SetNow`, `instant`; tests internal/ingest/server_test.go, internal/agentwatch/concurrency_test.go |
| M43: A JSONL record's own counters bound what it can put in a display. `maxTurnMS` caps a recorded turn length — grok's `apiDurationMs`/`elapsed_ms`, microagent's `elapsed_ms` — before it is multiplied out to nanoseconds, so a record naming a millisecond count near `MaxInt64` cannot wrap its span negative and report the turn's rate with the wrong sign. A session directory name that percent-decodes to a path carrying a NUL byte is refused rather than read as a working directory. agy's `last_conversations.json` winner is chosen by sorted workspace, not by Go's randomized map order, so one conversation named under two workspaces cannot change directory between polls. Pinned by `FuzzParseAgentLines` (every record parser, its negative-counter, determinism, and restatement-refusal invariants), `FuzzAgyIndex`, `FuzzGrokSessionCwd`, and `FuzzGrokSessionPath` | a hostile or corrupt session store steering attribution between workspaces, or reporting a rate of the wrong sign (B1 tampering) | agentusage/grok.go, `maxTurnMS` and `grokSessionCwd`; agentusage/microagent.go, `parseMicroagent`; agentusage/agy.go, `loadAgyLast`; tests agentusage/fuzz_test.go, agentusage/agy_grok_fuzz_test.go |
| M36: The release guard that refuses a version that is already published now runs `gh` with `GH_TOKEN` from the job token and admits only a 404. Before 9ef37e9 the guard was a bare `if gh release view ... ; then exit 1; fi`: with no token in the environment (the checkout writes no credentials) every call failed on auth, the non-zero status read as "not published", and the guard passed anything. A moved tag then re-runs the job and replaces binaries, checksums and SBOM under a version people have already verified | a repeated or hijacked release replacing artifacts an operator has a recorded checksum for, which is the same trust anchor as summary risk 3 (B5 tampering) | .github/workflows/release.yml, 42-69 |
| M40: The ssh account name is dropped where an audit line is written. `Target.LogHost` returns the host alone and every audit call site uses it (internal/remote/client.go, 237, 239, 243, 370, 452, 455; cmd/toktop/attach.go, 71, 78, 168, 211), and `Target.RedactUser` folds `user@` out of the error text the transport prefixes with the target (target.go, 174, at the connect, forward and connection-lost sites), so a `~/.ssh/config` User or the local login name does not reach the line either. `UserHost` remains what goes into the ssh argv and into the message on the operator's own terminal. The auth-chain lines are folded the same way: a default key path, the `$SSH_AUTH_SOCK` path and both error texts pass through `core.RedactHome`, and the socket through `logcfg.Field` | the account name of the ssh login reaching a line the operator pastes into a public issue, and a key or agent-socket path under `$HOME` naming the account in the same line (B4/response-readiness disclosure) | internal/remote/target.go, `LogHost` 166, `RedactUser` 174; internal/remote/auth.go, 233-240 and 245-254; tests internal/remote/audit_test.go |
| M41: The agent definitions file is read under a cap and every diagnostic it produces is folded. `readCapped` reads through `io.LimitReader(f, maxDefinitionsBytes+1)` and refuses a longer file as `errDefinitionsTooLarge`, joined into `ErrInvalidDefinitions` so the caller contract (a missing file is nil, a file that exists but cannot be used is `ErrInvalidDefinitions`) is unchanged; `defsErr` renders every message through `core.RedactHome` and keeps the cause reachable through `errors.Is`/`errors.As` by wrapping rather than rewriting, because a `*fs.PathError` carries its own copy of the absolute path. The startup line that prints it folds again on the way out (cmd/toktop/main.go, 189) | an unbounded read of a file that is only nominally a few kilobytes (`json.Unmarshal` builds a second copy of everything it decodes), and the account name in the one diagnostic an operator pastes into an issue (B7/response-readiness disclosure, DoS) | agentusage/definitions.go, `maxDefinitionsBytes` 291, `readCapped` 472, `defsErr` 328, `redactedError` 315; cmd/toktop/main.go, the folded startup line at 199-200; tests agentusage/definitions_test.go |
| M42: The release prerequisites are serialized. `.NOTPARALLEL: release` orders check-changelog, check-release-source, sbom and checksums, `checksums` depends on `sbom` rather than racing it, and `buildinfo` takes `dist-clean` as a prerequisite rather than a sibling, so `dist-clean` cannot delete binaries that `test-dist` is writing and the checksums glob cannot miss an SBOM that has not been written yet. `make release-verify` is the outer check that the published asset list matches `PLATFORMS` and re-verifies each download against the release's own `checksums.txt` | a `make -j release` publishing a `checksums.txt` that covers fewer files than the release ships, leaving an asset (the SBOM above all) with nothing to verify it against, against the same trust anchor as summary risk 3 (B5 tampering) | Makefile, `.NOTPARALLEL: release`, `checksums`, `buildinfo`, `release-verify` |
| M44: A definitions file key this build cannot read is reported, and a home that cannot place an absolute store names none. `unknownUsageKeys` reads the file a second time for the keys its usage block does not decode, so a misspelled `roots` is named at startup instead of leaving the agent with no store (`agentusage.UnknownUsageKeys`, `cmd/toktop` `warnUnknownUsageKeys`); the load still succeeds, since the file is gauntlet's and a newer gauntlet can name a key this build does not read. `agentusage.HomeDir` accepts `os.UserHomeDir` only when absolute, so a relative or unset `$HOME` yields no built-in store rather than one under the working directory (`agentusage/registry.go`, `home`, `DefinitionsPath`, `kimiStore`; `internal/remote/knownhosts.go`, `defaultKnownHostsPath`); the same absolute-only rule gates the four variables that relocate a store from the environment (`GAUNTLET_HOME`, `KIMI_CODE_HOME`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME`; `kimiStore` at agentusage/kimi.go, 82, is the last of the four), and `warnIgnoredUserHome` names it when `GAUNTLET_HOME` placed the definitions file and the run continued. An unread usage key and an unusable home both read as agents that used no tokens, which is what an agent nobody defined also looks like | a misspelled key in the one file an operator hand-writes leaving an agent permanently reporting zero tokens, and a relative or unset home silently resolving a built-in store under the working directory (B6/B7 tampering, disclosure) | agentusage/definitions.go, `UnknownUsageKeys` 145, `DefinitionsPath` 498; cmd/toktop/main.go, `warnUnknownUsageKeys` and its call from `main`; cmd/toktop/validate.go, `warnIgnoredUserHome` 157; agentusage/registry.go, `HomeDir` 495; agentusage/kimi.go, `kimiStore` 82; internal/remote/knownhosts.go, `defaultKnownHostsPath` 40 |
| M45: Host guard on the loopback ingest endpoint. `loopbackHostGuard` (internal/ingest/middleware.go, 68) is built in `newServer` from the listener's own address and refuses any `Host` that is not a loopback address, `localhost`, or a name under the reserved `.localhost` suffix, folded ASCII over NFC through `core.FoldASCII` so a U+212A in the name cannot satisfy a compare the operator never wrote. It is nil for a listener bound to a routable or wildcard address, where the host is whatever the operator's peers call the box and no allowlist is right, and a `Server` built as a literal skips it. The check runs in `wrap` ahead of the routing table and ahead of the method guard, so a rebound request learns nothing about what the endpoint serves: the 404's endpoint list and the 405's `Allow` header are both downstream of it, and `/healthz` is covered along with the POST route | DNS rebinding against the default loopback bind: a page that resolves its own name to 127.0.0.1 for one fetch is same-origin with this endpoint, so it sends no `Origin` and M21 never fires, while the `Host` header it does carry names the attacker's own domain; forging rows from any visited page, and learning the served route table from the 404/405 (B1 spoofing, information disclosure) | internal/ingest/middleware.go, `loopbackHostGuard` 68, the guard in `wrap` 142, the refusal line at 143; tests internal/ingest/server_test.go, `TestIngestRejectsReboundHost`, `TestIngestAcceptsLoopbackHostNames`, `TestIngestSkipsHostGuardOffLoopback`; fuzz target internal/ingest/fuzz_test.go |
| M46: The recovered panic value and its stack are the two audit payloads in the ingest package that reach the line untrimmed, so both are folded before they are logged: the panic text through `core.RedactHome` over `core.Snippet` (middleware.go, 130, the `recover` branch of `wrap`) and the stack through `core.RedactHome` inside the `logcfg.Field` cap (the `debug.Stack` argument on the same call). M35's home fold covered the fields this process authors; this is the one place it quotes text it did not | a panic raised while holding a sender-authored event field carrying whatever the sender wrote, and a stack frame naming the build's file paths, both landing in the stderr the operator pastes into a bug report (B1 disclosure, response readiness) | internal/ingest/middleware.go, the `recover` branch of `wrap`, 129-131; covered by M35's handler and the `logcfg.Field` cap |

Documentation claims checked against code on 2026-09-28. What this pass
found is in the header; the list below is what holds as written, so the next
pass has something to re-check rather than a record of what used to be wrong.
Corrections are made in place and the wrong version is deleted, never
appended: a diary of past drift is a fourth thing to keep current, and it
reads as a claim about the code when it is not one.

- The loopback `Host` guard holds as written (M45). `loopbackHostGuard`
  is nil for a wildcard or routable listener, since a `*net.TCPAddr` with a
  nil IP is not `IsLoopback` and that bind is already the state the startup
  warning names; the check runs in `wrap` ahead of both the 404 and the 405,
  so a refused request learns nothing about the route table; and the accepted
  name forms are the loopback address, `localhost`, and any `.localhost`
  suffix, folded ASCII over NFC. Pinned by
  `TestIngestRejectsReboundHost`, `TestIngestAcceptsLoopbackHostNames`,
  `TestIngestSkipsHostGuardOffLoopback`, and the ingest fuzz target.
- The recovered panic value and its stack are folded (M46). Both audit
  payloads on the `recover` branch of `wrap` now pass through
  `core.RedactHome`, the message additionally through `core.Snippet`, which
  is the only place in this package that quotes text the process did not
  author onto the line an operator pastes into an issue.
- The audit stream no longer carries the ssh account. The asset section, M35
  and the response-readiness list were written against `user@host` on the
  `target` attribute; `Target.LogHost` (internal/remote/target.go, 166) is
  what every one of those call sites writes now, and `Target.RedactUser`
  (target.go, 174) takes the account out of the error text beside it, so
  M40 holds and the engine `addr` line is the only operator-named
  destination left in the stream.
- The definitions file is bounded and its diagnostics folded (M41): a
  missing file is still nil, a malformed, colliding or oversize one is
  still `ErrInvalidDefinitions` and still exits 2 with the file named
  (cmd/toktop/main.go, 184-191), and the message now reads `~` where the
  path used to read the account.
- M42's ordering is in the Makefile at this commit: `.NOTPARALLEL: release`,
  `checksums: sbom buildinfo` and `buildinfo: dist-clean test-dist`, with
  `release-verify` unchanged as the outer check.
- The line citations in the Makefile, the ingest server and the audit
  sites were re-anchored this pass; several had moved without any
  behavior changing underneath them, which is the case the symbol rule
  below exists for. `internal/ingest/` has since joined
  `site/worker.js` and the `Makefile` on the symbol rule: three commits
  touched it in one day and every line citation this file held for it
  moved, so its references now name `ingestEndpoints`, `newServer`,
  `loopbackHostGuard`, `eventSlots`, `maxInFlightEvents`, `stallReason`,
  `handleHealth`, `progressBody`, `setSecurityHeaders`, `maxHeaderBytes`,
  `logRequest` and `nowMu` rather than a line.
- The B1 denial-of-service threat and gap 5 are written against the
  process-wide cap (`maxInFlightEvents` 64, M30): a flood of stalled local
  POSTs is refused at the 65th, not merely bounded at the 1-minute reap. The
  residual is the absence of a sender identity on a slot, not the absence of
  a cap. Gap 10 is written against what `currentUser` validation cannot see
  (any well-formed name is still accepted, and `HOME` is not validated at
  all), with M32 recording the control.
- M31-M42 all exist in the code: the empty `--ssh-key` refusal, the
  `currentUser` validation, the repeated-target and per-port forward dedup
  with the vendor-CLI output cap, the `eval_duration` range refusal folded
  into M10, the shared engine model-name bound, the log handler redaction,
  the authenticated release guard, the release-source gate, the single-watcher
  rule, the lock on the injected clock and error sink, the account-name drop
  at every ssh audit site, the capped and folded definitions read, and the
  serialized release prerequisites. None changes the ranking in the summary.
- The shell-probe fallback still checks only `p > 0` on the port it parses
  from a remote's stdout (the `strconv.Atoi` in `Discover`'s fallback,
  internal/remote/discover.go), and `FuzzParseDiscoveryOutput` still covers
  the `/proc/net/tcp` parser alone, so gap 8 stands as written.
- `/health` still answers 503 with the unbound-binding line, and
  `make site-deploy` still polls it with `curl -fsS` inside `wait_for_site`
  (Makefile, `wait_for_site`), so the gate holds. The new `/favicon.ico`
  route is answered from bytes embedded in the Worker (`FAVICON_PATH`,
  `FAVICON_BYTES` and `ICON_ETAG`, site/worker.js) and
  reaches neither `env.ASSETS` nor a compressed body, so it adds no new
  forward to the asset store, and it carries the same `SECURITY_HEADERS` set
  as every other answer (`nosniff` and the CSP among them). Its bytes are a
  compile-time constant in the Worker, so a caller controls the path and
  nothing else about it.
- `make site-deploy` gained a `check-wrangler-doc` prerequisite (Makefile,
  718, the check at 707-716). It fails the deploy when the `WRANGLER` pin in
  CONTRIBUTING.md and the one this document names have drifted apart, so
  gap 7's unpinned deploy tool cannot be fixed in one place and left stale in
  the other.

Other claims checked against code on this pass, all of which hold as written:

- `/health` answers 503 with
  `degraded: no asset binding; the dashboard captures are not served` when
  `env.ASSETS` is unbound, and `make site-deploy` polls it with `curl -fsS`
  (Makefile, `wait_for_site`), so a deploy that shipped without its assets fails the
  gate instead of waiting out a green probe. The poll is still only a
  presence-and-binding check: an older live version with its bindings answers
  `ok` for an upload that never took effect.
- Every answer carries `server-timing: edge;dur=<ms>`. It names the worker,
  not an origin, so it adds no disclosure.
- `failRequest` (:632) adds `error: String(err?.message ?? err)` to the
  unhandled-500 line, so a thrown message from the asset store or the runtime
  reaches Workers Logs alongside the caller-controlled `cf-ray`. The
  caller-controlled set is unchanged in kind: JSON-encoded, so still
  caller-chosen strings in a log stream, not injection.
- The bearer call sites re-verified: `Set` at :51, `Allow` at :68,
  `Apply` at :81, `admits` at :125, and `CheckRedirect` at :108
  (internal/bearer/bearer.go); the three `Apply` call sites are the ones in
  provider/discover.go, provider.go, and probe.go.
  `Apply` sets the header only when the request's origin is in the allow map,
  and the only writer to that map is `bearer.Allow` at cmd/toktop/attach.go,
  37, reached solely from `--add` URLs.
- Every engine JSON body is now decoded under a cap, discovery included:
  `decodeScanJSON` (internal/provider/discover.go) passes
  `io.LimitReader(resp.Body, jsonBodyMax)` to `json.NewDecoder`, the same
  bound the poll path applies (the `getJSON` LimitReader,
  internal/provider/provider.go). The
  identification decoders are no longer the one uncapped reader in the
  package (M8).
- M5's unknown-kind rule holds: internal/ingest/event.go, 63-70 keeps a
  nonempty unknown kind (lowercased, sanitized, folded to one line, 24-rune
  cap) and only an empty or sanitize-to-empty kind becomes `turn`.
- M13's ssh algorithm claim was re-checked against
  golang.org/x/crypto v0.57.0 `ssh/common.go`, 163-176 and upheld:
  `ssh.SupportedAlgorithms()` carries only RSA-SHA2, ECDSA and Ed25519 host
  keys, no `ssh-rsa` and no DSA.
- M29 names three `os.OpenRoot` sites, not two: `walkTranscripts`
  (agentusage/candidates.go, 225) is a third, re-run every second over each
  transcript root.
- M20's supply-chain list holds, including that CI runs
  `permissions: contents: read` while the release workflow holds
  `contents: write` for its whole job and not for the publish step alone,
  because Actions takes permissions on the workflow or the job and has no
  step-level scope (.github/workflows/ci.yml, 8;
  .github/workflows/release.yml, 10-11 and 141-147).
  That job-wide scope is what a workflow or runner
  compromise would use against the update channel of summary risk 3. The two
  CI drift gates (`make check-ci-platforms`, `make check-ci-tags`,
  .github/workflows/ci.yml, 66-74) are what keeps a shipped platform and a
  vetted one the same set.
- The agent-store asset names dsh, crush, and opencode, and the default
  `--agents` path also tails claude, codex, qwen, copilot, kimi, gemini, grok,
  agy, clanker, and the built-in pi, prime-agent, and feynman definitions
  (agentusage/registry.go, 93-121; agentusage/claude.go, 15; codex.go, 31).
- The ingest route registers GET and HEAD on `/healthz`
  (internal/ingest/endpoints.go, the `ingestEndpoints` table and the 405 `Allow`
  header written by the shared method guard); the `help` and `version` subcommands take arbitrary
  trailing arguments (cmd/toktop/main.go, 55-76).
- `XDG_CONFIG_HOME` and `XDG_DATA_HOME` are honored only when absolute. A
  relative `XDG_CONFIG_HOME` would write the known_hosts store under the
  working directory, where a later run from another directory would not find
  the pins and would re-TOFU; the same rule for `XDG_DATA_HOME`
  (agentusage/opencode_sqlite.go) costs an empty agent panel rather than a
  lost pin. Both are named as ignored at startup.
- README states a malformed `agents.json` "is reported at startup rather than
  silently shrinking the watch" (README.md, the `agents.json` paragraph), which
  matches the loader
  exiting 2 (cmd/toktop/main.go, the `ErrInvalidDefinitions` startup).
  README "Zero vendor libraries",
  "Engines: how discovery works", the SSH auth chain, the loopback relay
  listeners, the Origin-header 403, the stream-resume semantics, and the
  ingest POST audit log all match the code paths cited above.
- README documents the origin-scoped bearer correctly: "--bearer TOKEN ...
  sent to --add endpoints only", and the environment table lists
  `GITHUB_TOKEN` for `toktop update`. The token rides probes as well as
  identification and polls, but only to `--add` origins.
- SECURITY.md states there is no dedicated disclosure contact and no
  supported-version matrix, and that `toktop update` fetches the latest
  release. That matches selfupdate.go: `Check` always hits
  `/releases/latest` (237), the named repo defaults to `maci0/toktop`
  (`DefaultRepo`, 44), and `NewerThan` is exact-tag inequality (282), so there is
  no channel of old majors to support. It invents no reporting SLA, and its
  link to this file resolves.

Single points of failure: the render-time sanitizer (core/sanitize.go) is the
only control standing between all untrusted text and the terminal, and it now
has to be called from three renderers (frame, text report, JSON report), so a
fourth output path that forgets it is the concrete way this file goes stale
rather than a hypothetical. The TOFU callback is the only ssh
authentication-of-host control. The release checksums.txt is the only
integrity anchor for the entire update channel. Each carries several
high-impact threats alone.

## Abuse cases (documented, not demonstrated)

1. **Dashboard poisoning.** A hostile local process (or LAN peer after an
   `--ingest 0.0.0.0` start) POSTs NDJSON naming agent "claude" with huge
   token rates and a plausible note; a browser cannot do this on the default
   bind (M21 refuses the `Origin` it must send cross-origin, M45 refuses the
   attacker's own name in the `Host` a rebinding fetch still carries) but any
   script or agent on the host can, without authenticating, by addressing the
   endpoint under its loopback `Host` and sending no `Origin` at all. Enabling path:
   ingest handlePost -> collector.RecordAgent (collector.go) -> UI
   agents feed. The operator's view of "which agent is burning tokens" is now
   attacker-chosen; nothing distinguishes forged rows from agentwatch-sourced
   ones.
2. **Bearer token capture via operator-named endpoint**: whatever
   listens on an origin the operator `--add`ed receives
   `Authorization: Bearer <token>` on identification, poll, and probe
   requests (bearer.go; cmd/toktop/attach.go, 37). A typo'd or stale `--add`
   URL pointing at attacker-controlled space discloses the gateway key.
   Scanned ports and ssh-forwarded engines never receive the header (M1).
3. **First-connect MITM.** An attacker positioned between operator and target
   before the first ssh connect is pinned as the genuine host; subsequent
   sessions expose vitals polling and port forwarding to them. Enabling path:
   knownhosts.go stores first-presented key without out-of-band
   verification.
4. **Malicious-repo install.** `toktop update --repo attacker/toktop` fetches
   that repo's latest release, whose checksums.txt vouches for the attacker's
   binary; after install, Unix hot-reload executes it (update.go,
   selfupdate.go, exec_unix.go). Operator-invoked, but the only
   gate between a valid owner/name and code execution is the repo's own word.
   Path traversal, query strings, and off-GitHub asset URLs no longer reach
   Check (ValidateRepo + trustedAssetURL).
5. **Local client of an ssh relay.** While `toktop ssh://user@host` is
   running, a local process connects to `127.0.0.1:<ephemeral>` (the port
   Forward bound) and talks to the remote engine as localhost. Enabling path:
   client.go. The remote engine's own lack of auth is inherited.
   Scanning loopback for newly bound ports finds the listener; it is not
   secret, only ephemeral.
6. **agents.json file-read redirect.** A same-user writer points
   `~/.gauntlet/agents.json` (or `$GAUNTLET_HOME/agents.json`) at any
   readable JSONL; `--agents` tails it every second. Enabling path:
   definitions.go, main.go. Crush needs no such file: a
   watched cwd whose parents contain `.crush/crush.db` is queried
   automatically in sqlite builds (crush_sqlite.go). A symlink out of
   that project is refused (M29); a same-user writer who can place a regular
   file still wins.
7. **Client-side-trust inversion (noted for completeness):** the ingest feed
   trusts the sender entirely; there is no server-side notion of an authorized
   harness. Anything claiming to be an agent is rendered as one.

## Gaps needing sec-review attention (ranked)

Recorded as threats with locations; fixes do not happen in this document:

1. **No origin authentication on ingest** (risk 1, Medium): post.go, middleware.go.
   The routable-bind case is at least announced now (main.go) and the
   browser sender classes are refused rather than trusted: the cross-origin
   one by M21's 403 on `Origin`, and the rebinding one by M45's `Host` guard,
   which is the one M21 structurally cannot see. Every non-browser local
   process still forges rows unchallenged, and a hostile script can simply
   omit `Origin` and address the endpoint by its loopback `Host`, which is
   the accepted form.
2. **TOFU first-contact acceptance** (risk 2, Medium): inherent design
   tradeoff, documented in README; candidate improvements (verification
   prompt, SSHFP/known_hosts import) belong to sec-review.
3. **Unsigned release artifacts** (risk 3, Medium-Low): checksums.txt is
   self-published within the same release; no Sigstore/GPG signature ties
   binaries to a key independent of the release pipeline
   (.github/workflows/release.yml, selfupdate.go). Repo path
   traversal and off-GitHub asset URLs are closed (M18); the remaining gap
   is the repo as trust anchor.
4. **Unauthenticated ssh loopback relays** (risk 4, Medium-Low):
   client.go. Binding 127.0.0.1 limits the peer set to the host, not
   to the toktop process. Restricting the listener (unix socket with
   mode 0600, or in-process HTTP instead of a TCP port) belongs to
   sec-review.
5. **Ingest slots are a count, not a per-peer quota** (Low): the endpoint
   refuses more than `maxInFlightEvents` 64 concurrent POST
   bodies and advertises `Retry-After` (M30, post.go, `maxInFlightEvents` and `eventSlots`),
   so a flood is refused rather than absorbed. What the cap does not carry is
   a sender identity: one local process can hold every slot, and the 65th honest sender
   is refused alongside it. Per-peer attribution needs a peer notion this
   endpoint does not have (it redacts non-loopback addresses to `"remote"` on
   the audit line, M27, so even the log could not separate them). Gap 1's
   exposure question still governs whether this matters.
6. **Hot-reload and PATH-based tool execution** (risk 5, Low): acceptable for
   a same-user dev tool; revisit if toktop ever runs privileged. Unix-only
   for the re-exec half.
7. **Site deploy runs from a developer shell** (Low): `make site-deploy`
   fetches wrangler from the npm registry at run time and uses ambient
   Cloudflare credentials (Makefile, the `site-deploy` target). The credential's blast radius
   is a Cloudflare account, and the deploy tool is not lockfile-pinned; a
   registry compromise or a hijacked developer machine reaches the published
   site. The `/health` poll and `site-rollback` are the only recovery
   controls: the poll proves the site answers with its asset binding, not
   that this tree is what answers, and `dist/site.lock` serializes the two
   make targets against each other but not against a deploy run outside
   them. A rollback run
   twice is the one repeat that does damage here, which is why it needs
   `dist/site.deployed`: it is a no-op rather than a second undo.
8. **Discovery port bound missing on the shell-probe fallback** (Low): when the `/proc/net/tcp` sweep returns nothing, `Discover`
   falls back to probing well-known ports through the remote shell and
   parses the remote's stdout with `strconv.Atoi`, accepting anything
   `p > 0` (the `strconv.Atoi` in the shell-probe fallback, internal/remote/discover.go).
   `FuzzParseDiscoveryOutput` covers the `/proc/net/tcp` parser only, so the
   fallback is the one path
   where a hostile remote can name an out-of-range forward port
   (`Discovery.Listening` -> `ForwardSet`, the same fallback in discover.go). The forward
   simply fails to dial such a port, so the impact is a poisoned tunnel set
   and failed probes, not code execution. Bounding the fallback the way
   `parseNetTCP` is bounded, and extending the fuzz target to it, belongs
   to sec-review.
9. **A watched transcript's own `cwd` decides session ownership** (Low): `owns` reads a working directory out of the session JSON
    and compares it to the watched directory
    (agentusage/transcript.go, 263 and 307; `sessionCwd` at claude.go, 19,
    codex.go, 16). A writer of a watched store can therefore claim a
    session belonging to another project, or disown its own, moving token
    counts between directory rows. It is an attribution-integrity issue
    within the operator's own stores, not a boundary crossing, and the
    `ownerScanLines` cap means the scan is bounded. Belongs to sec-review if
    the attribution is meant to be authoritative.
10. **A hostile environment chooses the ssh login account and the
    home path roots** (Low): `currentUser` validates
    `USER` and `USERNAME` before use, so a value that could not be a login
    name is skipped rather than passed to the transport (M32,
    internal/remote/client.go, 81-104). What remains is the part validation
    cannot see: any well-formed name is accepted, so a hostile environment
    can still authenticate as a different account on the target, with
    whatever the offered key unlocks there. The blast radius stays bounded
    by the keys the operator already offered. `HOME` / `USERPROFILE` is
    worse and is not validated at all: it relocates `~/.ssh/config`, the
    default identity set, every built-in transcript root, the opencode
    store, and the `~` redaction at once, and a name that passes every
    check still moves all of them. Treating `USER` and `HOME` as trusted
    same-user input belongs to sec-review only if toktop is ever run with a
    less trusted environment.
11. **The JSON report hands sanitized-for-a-terminal text to an arbitrary
    consumer** (Low): `--once --json` (internal/ui/json.go) sanitizes
    every free-form field with `core.SanitizeText`, which is the right
    control for a person reading a terminal and the wrong one for a program
    reading JSON: it strips control characters, not the structure a
    consumer must not splice. A hostile engine, a hostile ssh remote, or any
    local process allowed to POST to the ingest endpoint chooses the content
    of `label`, `model`, `error`, `addr`, and `note` (M2), and the documented
    consumers are shell pipelines (`README.md, 521-533`). Nothing here is a
    defect in toktop's own rendering, and the fields stay inside a JSON
    string, so a reader using `jq` is fine; the exposure is a consumer that
    evals, re-renders into HTML, or rebuilds a shell command from the values
    without treating them as untrusted. Recording it so a future consumer of
    the report does not mistake sanitized-for-TTY for safe-for-markup.

## Response readiness (notes only)

- **Audit trail:** six subsystems write to stderr through the same logger
  and the same floor, `TOKTOP_LOG_LEVEL` (internal/logcfg), so the floor
  moves for all of them at once and none of them is silent because its own
  endpoint is off:
  - the ingest server (`logcfg.Logger()` in `New`, internal/ingest/server.go,
    74): POST /v1/events, 404/405, and recovered handler panics, all through
    `logRequest` at middleware.go:197, which names the request id, method, path, redacted
    peer, status, accepted and stored counts and the elapsed time on every
    line. Default info
    includes successful POSTs; warn/error suppress them, and error also
    suppresses 4xx rejections. Successful health checks are not logged
    (`handleHealth`, health.go:16). Request ids can be supplied by callers
    (`incomingRequestID`, middleware.go:175), so they
    provide correlation, not sender identity. A body that trips a deadline
    names which bound it broke, in the 408 body rather than a log line
    (M30, `stallReason` in post.go), so an operator can
    tell a peer that stopped sending from one whose stream outlived the
    10-minute bound without reading the source for which branch fired.
  - the ssh client (`var audit = logcfg.Logger`, internal/remote/client.go,
    33): connect failure and success (`:236` warn, `:242` info), a peer that
    stopped answering keepalives (`:369`), and a lost connection (`:451`
    error, carrying the uptime). An unanswered channel open is the fourth
    (internal/remote/session.go, 159). Every one names the target by host
    alone, with the account folded out of the error text (M40); a forward
    failure is the sixth site (internal/remote/forward.go, 237). The two
    auth-chain lines written here
    rather than in the client, a default ssh key that is present and will not
    load and an agent socket that refuses the connection
    (internal/remote/auth.go, 247 and 262), fold `$HOME` out of the key and
    socket paths and both error texts for the same reason.
  - the engine collector (`auditFn = logcfg.Logger`,
    internal/collector/collector.go, 38, one line per
    engine that crossed a boundary: `logChanges` at health.go:233, called with
    warn for not answering and info for answering again at collector.go:413-414,
    carrying label, addr, reason and down-for, and warn for a poll that
    answered past `slowPollThreshold` (health.go:221, half of
    provider.PollTimeout) with info when the next one answers in time
    (collector.go:415-416), carrying label, addr, duration and
    slow-for. Both latches are per endpoint, so a run of them writes one line,
    and a failed poll clears the slow latch, because the outage line is the
    loudest signal and the next answer is measured fresh. Events the retained
    window turned away get their own pair of lines from `logWindowRefusals`
    (agents.go:142), so a stored-below-accepted POST reads as a refusal rather
    than a replay.
  - the GPU vendor tools (`var audit = logcfg.Logger`, internal/gpu/gpu.go,
    206): a vendor CLI that failed to answer (`:242`) and one that answered
    again (`:264`), carrying the tool name and the reason, so a `$PATH` tool
    that stops working is a recorded event rather than a blank panel.
  - the agent watch (`aw.SetOnError` in cmd/toktop/main.go, 337): a condition
    the
    watcher cannot return, warn, with the error text. The dedup latch that
    keeps it to one line per distinct condition lives in the watcher
    (agentwatch.go, `engineError`), not here. The agentusage package writes
    its own lines through the logger the host installs
    (`agentusage.SetLogger`, cmd/toktop/main.go, 118, over
    `agentusage.SetLogger` at agentusage/candidates.go, 101): a transcript
    walk that could not finish (`auditWalkFailure`, 125), which folds
    `$HOME` out of both the root and the error text before logging, since a
    walk failure is exactly the line an operator pastes into an issue.
  - the `--add` attach path (`var attachLog = logcfg.Logger`,
    cmd/toktop/attach.go, 28): a refused bearer and an unrecognized endpoint
    (`:39`, `:45`), an ssh target that was not attached and one that was (`:70`,
    `:77`), a remote port that could not be forwarded (`:167`), and a remote
    port skipped for speaking no recognized engine API (`:210`).
  Event bodies, notes, and token counts are not audit attributes; the
  attributes that are written go through `logcfg.Field`
  (sanitize, collapse whitespace, cap) and the whole record through the
  home-folding handler (M35), so a line is postable into an issue, but there
  is no credential-redaction guarantee for arbitrary caller-supplied log
  fields (middleware.go). The subsystems outside ingest carry
  operator-named infrastructure into the same stream: engine `addr` values
  (a `--add` URL, a discovered port, `host:port` for a remote engine,
  collector.go, `logHealth` 538 and `logSlow` 559) and ssh `target` labels,
  written as `tgt.LogHost()`, the host alone with the account dropped
  (`Target.LogHost`, internal/remote/target.go, 166; the sites at
  cmd/toktop/attach.go, 71, 78, 168, 211 and internal/remote/client.go, 226,
  232, 362, 543, 757, 884, internal/remote/stats.go, 186, 193), with the
  account folded out of the error text beside it (`Target.RedactUser`,
  target.go, 174). Those are destinations the operator typed or the run
  discovered, not peer addresses, so `logcfg.Remote` does not apply to them
  and M27 does not cover them; what bounds them is `logcfg.Field` plus the
  home fold, and for an ssh target the account is already gone before either
  runs (M40). An operator pasting a report therefore publishes the names of
  the hosts and gateways that run, which the frame's own
  redaction-into-`~` work (asset: diagnostic log lines) does not reach; what
  it no longer publishes is the login the account name spelled.
  The
  feed is bounded in-memory state
  (internal/collector/collector.go). After exit, only captured stderr
  remains, without enough payload data to reconstruct poisoning.
- **Reported-vulnerability-to-fix path:** undocumented. SECURITY.md exists
  and states that no dedicated disclosure contact or supported-version
  matrix is published; it invents no SLA. CONTRIBUTING.md covers CI gates
  only. Creating a reporting process is an organizational decision, not
  something this document invents.

## Maintenance rules for this file

- Every entry point, boundary, and mitigation line keeps its file reference so
  the next pass can diff claims against code mechanically.
- A reference into a file that changes often names the symbol, the branch, or
  the make target, not a line. That rule covers `Makefile`, `site/worker.js`,
  `internal/ingest/`, `internal/provider/`, `internal/probe/`,
  `internal/selfupdate/`, `internal/ui/`, `cmd/toktop/validate.go` and
  `agentusage/definitions.go` as well as the Go files it was written for: each
  carries hundreds of lines that no review touches, and a line number there
  reads as a claim about the code when it is only a claim about where the code
  was. A line number is kept only where the reference is to a single short
  declaration in a file that is not churning (an `M` row naming one constant),
  and a symbol is named alongside it there too. A symbol can stop existing
  too, which is the other half of the rule: `site/worker.js`'s `/health`
  answer stopped being a named function in the process, so that citation names
  the branch that replaced it.
- New entry point in code => add it here in the same change. Same for a new
  control: a bound, a cap, a validation, or a redaction that did not exist
  when this file was written gets its `M` row in the change that lands it,
  which is what keeps a later pass from finding it instead.
- A correction replaces the wrong line; it does not get a note about the
  correction.
- Fixed vulnerabilities move from "Gaps" into the mitigations table with the
  commit that closed them.
