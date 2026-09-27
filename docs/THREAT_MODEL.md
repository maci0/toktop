# toktop threat model

Living document for the security owner: the whole attack surface on one screen,
with file references so each claim can be re-verified against code. Individual
vulnerabilities and their fixes belong to sec-review; this file records where
they live and what already stands in their way.

- **Last reviewed:** 2026-09-27 (every claim below re-read against code at
  this commit: ingest, bearer, provider, probe, remote, selfupdate, selfreload,
  gpu, agentusage, workflows, Makefile, site/worker.js). The current pass
  found this file had drifted behind five commits that each changed a
  documented control, and that the site paragraph had been re-anchored three
  times in one day without the underlying problem being addressed. The
  material correction is that the ingest server bounds concurrent POST bodies
  process-wide (`maxInFlightEvents`), which the B1 denial-of-service threat
  and gap 6 both described as unbounded; gap 6 is now written against the
  real bound. Four controls that did not exist when those lines were written
  are recorded (M30-M33), and gap 11's claim that a hostile `$USER` chooses
  the ssh login account no longer holds, since the value is validated before
  it reaches the transport. Site references now name a symbol as well as a
  line. The per-pass correction diary that had accumulated below is gone; the
  documentation-claims section records what holds and what the current pass
  found instead. Nothing left the surface and no risk changed rank.
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
| 1 | Ingest endpoint accepts unauthenticated events from any local process (any network peer if bound non-loopback via `--ingest`); forged telemetry renders as real agents | local processes -> ingest, network -> ingest | internal/ingest/server.go, cmd/toktop/main.go | No authentication; browser-driven forgery, DoS, injection, mixed-script names, and timestamp forgery mitigated (M3-M7, M21, M26, M30) |
| 2 | Trust-on-first-use accepts a first-contact MITM by design; only key *changes* are refused | operator -> ssh target | internal/remote/knownhosts.go | Documented residual risk (README "Auth" section) |
| 3 | Self-update installs whatever binary the named GitHub repo published: integrity rests on the release's own checksums.txt over TLS; no external signature exists | runtime -> update channel | internal/selfupdate/selfupdate.go, .github/workflows/release.yml | Checksum + size + GitHub-host URL verification present; owner/name validated (M18); signing absent |
| 4 | SSH engine relays bind loopback listeners (`127.0.0.1:0`); any local process can reach the remote engines those listeners front | local processes -> remote engines | internal/remote/client.go | Bound loopback-only; no listener auth |
| 5 | Hot-reload re-execs whatever binary occupies the exe path when its identity changes (Unix); PATH-based vendor CLI lookup executes tools from `$PATH` | build -> runtime, host -> process | internal/selfreload/exec_unix.go; internal/gpu/gpu.go | Windows Restart does not exec (exec_windows.go); `--no-hot-reload` exists |
| 6 | Low: ingest poisoning cannot be reconstructed from retained payloads; raising the log floor also hides successful submissions | B1, response readiness | internal/ingest/server.go; internal/collector/collector.go | Request metadata logs at info by default; warn/error suppress successes. No authenticated sender identity or durable event store |

Resolved since 2026-08-25: the previous ranking's "bearer token sent to every
probed endpoint" is closed by origin-scoped token application
(commit 21e3feb); see mitigations M1. The previous claim that `--repo` is
interpolated into the GitHub API path with no owner/name check is closed by
`ValidateRepo` (selfupdate.go, 96-118, called from Check at :180 and from
`toktop update` at cmd/toktop/update.go). Nothing in this table is
demonstrated by attacking anything; every claim cites code read at review time.

## Assets

What is worth stealing, corrupting, or denying:

- **Engine credentials**: one process-wide bearer token
  (internal/bearer/bearer.go), sourced from `--bearer`,
  `OMNIROUTE_API_KEY`, or `TOKTOP_BEARER` (resolveBearer,
  cmd/toktop/validate.go, 206-215, called at cmd/toktop/main.go, 169 and
  565). Grants API access to gateways like OmniRoute.
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
  (internal/remote/knownhosts.go, 31-34). The env var is read first on every
  platform, so on macOS and Windows it widens the store's location past what
  `UserConfigDir` would pick. This is the only state toktop keeps across runs
  and nothing in this repository backs it up: a lost, truncated or emptied
  store is a total loss of pins, and a read that cannot be trusted now fails
  loudly instead of re-trusting every host. The two writes that can end a
  store half-finished (a kill between the two renames `replaceFile` makes on
  Windows) leave the previous pins beside the store under `.displaced`, and
  `readKnownHosts` reads that copy back rather than reading a missing store as
  no pins at all; the same window in the self-update leaves the installed
  binary under `.old`, which the next install restores before replacing it
  (internal/selfupdate/selfupdate.go, restoreDisplaced). A store lost with no
  `.displaced` beside it is a manual repair: restore the file, or delete it to
  pin the hosts again on purpose.
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
  (`~/.qwen/projects`), copilot (`~/.copilot/session-state`), clanker
  (its token log inside the repository it runs in, registry.go, 126-133), and
  the built-in pi, prime-agent, and feynman definitions
  (agentusage/definitions.go, 115-121). Beyond plain JSONL, dsh's default
  `session.v<N>.jsonl.zstd` (concatenated zstd frames, agentusage/dsh.go),
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
  become the note (shortDir, :465, over core.ShortDir).

## Entry points

Every externally reachable input, with its code location:

1. **Ingest HTTP server** (on by default): `POST /v1/events` (single JSON or
   NDJSON stream), `GET`/`HEAD /healthz`
   (internal/ingest/server.go; routes registered from the endpoint table at
   :257, the 405 `Allow` header names `GET, HEAD`). The health route is a
   load report, not a bare liveness probe: it answers 503 with a
   `degraded: N/64 event streams in flight` line and the same
   `Retry-After` a refused POST carries while every decode slot is held
   (M30, server.go, 466-478), so it cannot report `ok` for an endpoint that
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
   `--frames`, `--seed` (cmd/toktop/flags.go, 42-61); positional
   `ssh://[user@]host[:port]` targets (interpretArgs / ParseTarget); the
   `update` subcommand with `--check` and `--repo owner/name`
   (cmd/toktop/update.go, 67-72); and the `help` and `version` subcommands
   (cmd/toktop/main.go, 51-53), which accept arbitrary trailing arguments
   (`runHelp(os.Stdout, os.Args[2:])`) and are reachable in forwarded form as
   `toktop --help update` (main.go, 62-65). A set-but-empty `--ssh-key` is
   rejected with exit 2 rather than falling back to `~/.ssh/config` (M31), and
   an `ssh://` target named more than once attaches once, which also keeps one
   forward per remote port (M33). `--seed` fixes the demo RNG, and
   demo mode is the one path where the ingest endpoint runs against a
   synthetic recorder. `--repo` is checked with `ValidateRepo`
   (owner/name only). `--add` rejects non-http(s), missing host, and userinfo,
   and refuses the same endpoint twice, since two polls of it read as twice
   the tokens (validateAddURL, parseAdd, endpoints.go). An `ssh://` URL that
   embeds a password, path, query, or fragment is rejected at startup
   (internal/remote/target.go).
   The live dashboard refuses to start when stdout is not a terminal
   (main.go); `--once` is the non-TTY path.
3. **Environment variables**: secrets `OMNIROUTE_API_KEY`,
   `TOKTOP_BEARER`, `TOKTOP_SSH_PASSWORD`, `GITHUB_TOKEN`; plus
   `SSH_AUTH_SOCK`, `TOKTOP_COLUMNS`/`TOKTOP_LINES`, `TOKTOP_LOG_LEVEL`
   (cmd/toktop/main.go; internal/remote/auth.go; internal/selfupdate;
   internal/ingest), `GAUNTLET_HOME` (agentusage/definitions.go; honored
   only when absolute, so a relative value cannot pull definitions from the
   working directory, and it is named as ignored at startup under `--agents`),
   `XDG_CONFIG_HOME` (internal/remote/knownhosts.go, 19-28; honored only
   when absolute, and a relative value is named as ignored at startup when
   an `ssh://` target would have read it),
   `XDG_DATA_HOME` (agentusage/opencode_sqlite.go, only when
   `--opencode-db` is on, which it is by default with `--agents`; a
   relative value is named as ignored at startup, like `GAUNTLET_HOME`), and
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
   Asset URLs must be GitHub download hosts (trustedAssetURL, :89-91,
   enforced at :263-265);
   redirects off those hosts are refused (githubRedirect, :161-174). The asset
   is verified against the downloaded checksums and size-capped before anything
   is renamed over the running binary (:255-300).
5. **SSH client sessions** (outbound): shell scripts executed on the remote
   for discovery and vitals (internal/remote/discover.go;
   internal/remote/stats.go); all returned text is parsed locally, and
   discovery ports from the `/proc/net/tcp` sweep are parsed as 16-bit with
   zero rejected (discover.go, 106-109; the shell-probe fallback at
   41-48 accepts anything `p > 0` (Atoi at :44), see gap 9).
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
   known_hosts store (knownhosts.go, 31-34).
10. **Self hot-reload**: polls the running executable's stat identity and
    restarts into it when changed (internal/selfreload/selfreload.go,
    exec_unix.go; cmd/toktop/main.go). The watcher is armed on every
    platform whenever the live TUI runs (main.go, 346), default on;
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
    `make site-rollback` as the next step (Makefile site-deploy /
    site-rollback, SITE_GUARD, WRANGLER, SITE_LOCK, SITE_DEPLOYED,
    SITE_ROLLED_BACK, SITE_HEALTH_URL,
    SITE_HEALTH_TRIES, SITE_HEALTH_WAIT). A rollback undoes the most recent
    deployment whoever shipped it, so a deploy that reported success records
    `dist/site.deployed` and a rollback moves it to `dist/site.rolled-back`:
    a second rollback has nothing of this tree's to undo and exits 0 without
    calling wrangler. The poll is a presence-and-binding check, not an
    identity check: `/health` answers 503 rather than `ok` while the worker's
    asset binding is unbound (site/worker.js, 688-717), so a deploy that
    shipped without `/dashboard.png` fails the gate, but an upload that never
    took effect still answers `ok` from the version already live. The deploy
    tool is fetched from the
    npm registry at run time by version tag rather than from a lockfile, and
    it authenticates with whatever Cloudflare credentials the invoking shell
    already holds. Anyone who can run that target with those credentials can
    replace the site carrying the project's download links.

Deployment surface:

- GitHub Actions CI runs gofmt/vet/`go mod tidy -diff`/govulncheck/staticcheck/
  race tests on pushes and PRs, plus biome over the Worker and the jsonc
  configs, `bun test site/` and
  screenshot-script lint (.github/workflows/ci.yml, actions pinned by SHA, bun
  from `.bun-version`, biome at the Makefile `BIOME` pin, uv 0.12.6, Python
  tools from `scripts/requirements-dev.txt`);
  Dependabot updates modules, actions, and `scripts/` pip deps
  (.github/dependabot.yml); tag pushes build release binaries for six
  platforms plus a CycloneDX SBOM (.github/workflows/release.yml, Makefile
  `release`/`sbom`). Release artifacts ship SHA-256 checksums only; no
  signature step exists. Tag names that reach ldflags and dist filenames are
  refused unless they are a safe identifier (release.yml).
- The marketing site is a single Cloudflare Worker serving one static page
  from an embedded string (site/worker.js, 813 lines): GET/HEAD only (the
  method guard runs twice, at :678-681 for image paths and :736-739 for the
  page, and any other method gets 405 with `allow: GET, HEAD`), a `/health`
  route (:740-770), weak FNV ETag over the page with weak `If-None-Match`
  matching (ETAG_HASH :351-360, ETAG :362, ifNoneMatchMatches :368-377, the
  304 answer :786-797), content negotiation (brotli, zstd, gzip, identity)
  compressed once per isolate, keyed by `Vary: Accept-Encoding` on every
  page response (VARY :518, PAGE_CACHE_CONTROL :513, representationFor
  :491-507), and hardening headers
  (nosniff, HSTS, referrer-policy, CSP
  `default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:;
  base-uri 'none'; form-action 'none'; frame-ancestors 'none'`) on every
  page, image, and health response (SECURITY_HEADERS, :520-527; the same set
  adds `x-frame-options: DENY` and an HSTS of `max-age=31536000` with no
  `includeSubDomains`). Every answer also carries
  `server-timing: edge;dur=<ms>`, which names the worker itself, not any
  origin (serverTiming, :590-592). wrangler.jsonc sets
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
  (worker.js, :688-702, the fetch itself at :696), and an asset-store failure
  becomes a one-line text/plain body rather than the store's HTML error
  page (assetErrorBody, :612-615, :713-717), with `cache-control: no-store`
  on any non-200/304 answer so a 404 cannot stick (:718-725).
  `style-src 'unsafe-inline'` is idle:
  the HTML is a compile-time string.
  Any path that is neither in `IMAGE_PATHS` (:631-636) nor `/health` serves
  the marketing page with 200, not a 404 (the catch-all, :778-813). A client
  that refuses every offered coding gets 406 with `vary: Accept-Encoding`
  (:783-785). An unbound `env.ASSETS` on an image path logs and returns 404
  (:685-687), and any throw becomes a 500 with the body `internal error`
  (:660-672); both answers go out through ERROR_HEADERS (:529-533), which is
  otherwise undescribed.
  `/health` is a binding check, not a presence check: with `env.ASSETS`
  unbound it answers 503 with
  `degraded: no asset binding; the dashboard captures are not served`
  rather than `ok` (:740-770), because the page still serves while every
  capture it shows is a 404. `make site-deploy` polls it with
  `curl -fsS` (Makefile, :384), which fails on that 503, so a deploy that
  shipped without its assets cannot report success. What the poll still
  cannot tell is *which* version answered: an upload that never took effect
  leaves an older live version with its bindings answering `ok`.
  Finally, the set of request bytes and strings the Worker inspects is
  larger than this file previously claimed: `logFailure` (:599-608) writes a
  JSON line carrying the caller-controlled `cf-ray` header, plus `method` and
  `path` on the `assets-unbound`, `asset-missing`, `asset-store-error`, and
  `unhandled` lines (failRequest, :573-582). The unhandled line adds
  `error: String(err?.message ?? err)`, a thrown message from the asset store
  or the runtime rather than from the caller. The values are JSON-encoded, so
  this is not injection into the log, but a caller can write arbitrary
  strings into the log stream.
  Deployment is a local make target, not a CI job: `make site-deploy`
  takes `dist/site.lock`, runs `bunx wrangler@4.126.0 deploy` with ambient
  Cloudflare credentials, records `dist/site.deployed`, polls
  `https://toktop.ai/health` 6 times at 10s,
  and points at `make site-rollback` on failure. It depends on `site-lint`
  and `site-check` (Makefile, 397, 347-352), so the same biome lint and
  `bun test` the CI workflow runs gate a local deploy too; `site-rollback`
  has no such dependency, deliberately, so a broken worker can still be
  undone. The lock, the poll and
  the failure exit are shared with `make site-rollback`, so a rollback
  cannot race a deploy and cannot report success over a site that is not
  answering, and the recorded marker makes the rollback itself run once
  (Makefile, 375-415).

## Trust boundaries and data flow

- **B1: local processes -> ingest server.** Any process running as any local
  user who can reach the socket can POST events. There is no named
  authentication point; fields are sanitized and clamped
  (internal/ingest/event.go), and POSTs with a nonempty Origin header
  are refused (internal/ingest/server.go). Request metadata goes to
  stderr subject to `TOKTOP_LOG_LEVEL` (server.go).
  Remote address and request id are not authenticated sender identities:
  `X-Request-Id` is caller-controlled, sanitized, and capped at 64 characters
  (server.go); non-loopback addresses become `"remote"`
  (server.go).
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
  sweep parses ports as 16-bit with zero rejected, discover.go, 106-109,
  while the shell-probe fallback checks only `p > 0`, discover.go, 41-48)
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
  `Allow`, populated solely from operator-named `--add` URLs (main.go, the
  only `Allow` call site). The token therefore rides identification, poll,
  *and* probe requests to those origins (`Apply` at
  internal/provider/discover.go, internal/provider/provider.go, and
  internal/probe/probe.go, 448); every other destination is filtered out
  inside `Apply` (bearer.go, 70-77, 114-122), so discovery scans and
  ssh-forwarded remote engines never carry it. `CheckRedirect`
  (bearer.go, 97-113) strips the header on any hop leaving the original
  origin, and `Set` (bearer.go, 40-48) discards a token containing CR or
  LF, so neither a redirect nor a hostile env value can move or inject it.
  There is no
  `bearer.Token` accessor; the only outbound path is Apply. It is never
  written to disk or logs.
  SSH password: env or TTY prompt (auth.go), held in memory, used only
  for password and keyboard-interactive mechanisms. Keys: read from disk or
  agent, sign locally. `GITHUB_TOKEN`: env, sent only to api.github.com
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
  files are read (cmd/toktop/main.go); `PATH` decides which vendor
  CLIs execute (gpu.go, whose stdout is capped at 1 MiB, M33); `USER` /
  `USERNAME` choose the login account and `HOME` / `USERPROFILE` relocates
  every home-relative path, both of which are validated for shape at most
  (M32, gap 11). All are same-user-writable inputs treated as trusted.
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
  (server.go). Renders beside genuine agentwatch data. The one
  sender class refused outright is the browser: a POST carrying an `Origin`
  header gets 403 (server.go), closing cross-site forgery from web
  pages the operator visits. Forged future timestamps are clamped to arrival
  time beyond a 2-minute skew (server.go), so the "live" marker
  cannot be pinned by a claimed far-future stamp. Mixed Latin+Cyrillic/Greek
  agent names collapse to `anonymous` (server.go).
- *Repudiation*: POST handlers emit remote, request id, status, accepted
  count, and stored count at info on success, warn for rejection/write
  errors, and error for
  5xx (internal/ingest/server.go). A warn/error log
  floor suppresses successes; error also suppresses 4xx rejections. Request
  ids are caller-supplied correlation labels, not evidence of identity
  (server.go). Event bodies are excluded and non-loopback IPs are
  redacted. The stored count is what the retained feed took, so a replayed
  id is visible as `accepted` above `stored` (server.go;
  internal/collector/collector.go).
- *Information disclosure*: none beyond presence (`/healthz` answers any
  requester, server.go). Residual: a routable bind would otherwise have
  put peer IPs on stderr; logRemote drops them.
- *Denial of service*: mitigated (see M3-M5, M30). Bounded three ways per
  connection (byte cap, 1 min body-idle, 10 min absolute lifetime) and a
  fourth across the endpoint: at most `maxInFlightEvents` 64 POST bodies are
  decoded at any instant, process-wide, and a POST arriving when every slot is
  held is refused at once with 503 and `Retry-After` rather than read
  (server.go, 372-387, 529). Residual: the cap is a count, not a per-peer
  quota, so 64 stalled local POSTs still hold 64 descriptors and goroutines
  and the 65th legitimate sender is refused with them (gap 6); nothing
  attributes a slot to a peer, so one process can occupy all of them.
  MaxHeaderBytes is 16 KiB (server.go, 81).
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
  PollTimeout 1.5s / probe timeout 30s; bodies capped (M8). Identification
  decoders are the exception: they read `resp.Body` with no limit
  (provider/discover.go, 254, 308, 324), so a listener that answers a
  scan can stream without bound inside the 700ms window (gap 5). Probe
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
  (discover.go, 106-109, fuzz-pinned by FuzzParseDiscoveryOutput), so a
  hostile /proc dump cannot name an out-of-range forward target. The
  shell-probe fallback that runs when that sweep yields nothing is not
  bounded the same way (gap 9).

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
  (selfupdate.go); Check builds the URL with `url.JoinPath`
  (:184-186). Asset URLs must be GitHub download hosts, and redirects off
  those hosts are refused (:161-174, 263-265). The asset must match the
  release's own checksums.txt (:267-291), so a network attacker cannot
  substitute a binary. The trust anchor is the repo itself: whoever controls
  the repo (or its release pipeline) ships a binary that verifies against its
  own checksums. No external signature (Sigstore/GPG) closes that gap today;
  summary risk 3.
- *Denial of service*: downloads capped at 256 MiB asset / 1 MiB compressed
  checksums / 2 MiB decompressed checksums (selfupdate.go);
  a truncated or hostile mirror fails verification and leaves the running
  binary untouched (:295-297).

**B6 (user configs):**
- *Elevation of privilege*: `--repo owner/name` redirects the update channel
  to an arbitrary public repo (after ValidateRepo); combined with the
  self-published-checksum trust anchor above, installing from a hostile repo
  is operator-invoked arbitrary code installation. `agents.json` chooses
  which files toktop reads every second; a same-user writer can point it at
  anything readable.

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
| M1: Origin-scoped bearer application. Token attached only to `Allow`-admitted origins, populated exclusively from operator `--add` URLs; `CheckRedirect` deletes the header on any hop off the original origin or outside the allow set, capped at 10 hops; `Set` blanks a token containing CR or LF, so an argv or env value cannot inject a header | Credential harvesting by scanned ports, forwarded remotes, and probes (B2/B4 disclosure; previous summary risk 1, fixed in commit 21e3feb); credential loss to a redirect target; header injection through a hostile token | internal/bearer/bearer.go, 40-48, 97-113, 114-122; cmd/toktop/main.go, 207; call sites provider/discover.go, 72 and 287; provider.go, 79; probe.go, 448; tests internal/bearer/bearer_test.go |
| M2: Terminal escape/control-char sanitizer applied both at ingest and at render time (C0/C1 including UTF-8-encoded C1; bidi overrides/isolates; zero-width format; variation selectors). Mixed Latin+Cyrillic/Greek agent names collapse to `anonymous` | OSC/CSI clipboard-cursor-title injection from engines, remotes, and events; mixed-script impersonation of agent names (asset: terminal integrity, dashboard integrity) | internal/core/sanitize.go; ingest side server.go; render side internal/ui/ui.go, internal/ui/plain.go, internal/ui/format.go, internal/ui/agents.go |
| M3: Ingest body cap 1 MiB + MaxBytesReader | unbounded upload into decode loop (B1 DoS) | server.go |
| M4: Ingest read deadlines: 10 min absolute lifetime, 1 min idle extension, 5 s header timeout, 2 min idle reap, 30 s response write deadline armed before every error/response write (server.go, 438-444, 495-497) | slowloris/drip DoS and stalled-response resource pinning (B1) | server.go |
| M5: Event field clamps (id 128, agent 64, model 128, via_engine 128, note 512; a kind outside the four known ones is lowercased, sanitized and clamped to 24 runes rather than replaced, so `kind: "banana"` reaches the feed as `banana`; only a kind that is empty or sanitizes to empty becomes `turn`) + token clamp (negative or >1<<40 to zero) + retention caps (512 events, 128 probes per snapshot) + event-id dedup ledger bounded by a 15 min horizon and a 4096-entry cap, so a replay stays recognizable past the display ring and the ledger cannot grow without bound | memory pinning via oversized or numerous events; wrap of agent totals (B1 DoS/tampering) | server.go; event.go:44-70, 177-183; core.AgentHistoryLen / ProbeHistoryLen internal/core/core.go; collector.go |
| M6: Event timestamp skew clamp: stamps >2 min in the future reset to arrival time | forged-future stamps pinning the live marker and feed ordering (B1 spoofing) | server.go |
| M7: Negative/absurd token counts clamped to zero; unknown kinds defaulted | junk values entering retained state (B1 tampering) | server.go |
| M8: Engine response caps: 4 MiB JSON, 8 MiB text, 256-rune error snippets | memory blowup and log flooding from hostile engines (B2 DoS/disclosure) | provider/provider.go httpStatus; core.Snippet |
| M9: Non-finite rejection in metrics (per-value and family-sum overflow guard) and vendor CSV/JSON coercion | poisoned counters/rates propagating through history (B2 tampering) | provider.go; gpu.go; collector counter-reset clamp collector.go |
| M10: Probes request 32 tokens, Ollama `think=false`, OpenAI `n=1`; streaming readers stop after 32 observed content/reasoning units or 1 KiB decoded content/reasoning, with 16 KiB scanner and 128 KiB stream limits. Non-stream OpenAI JSON over 16 KiB is rejected. Reported token counts trusted only up to 128; fixed prompt, model-id cap 256, embed/rerank filtering, and 429/503 backoff 15s–5m. A wire-reported `eval_duration` is passed through `nanoseconds`, which returns zero for a negative value, one outside the int64 nanosecond range, or one beyond `maxEvalDuration` 24h, so a count that would wrap into a plausible duration cannot reach `fitEvalDuration`'s band rescale and report a throughput the engine never claimed | Limits client work and reduces B2 compute/spend amplification; request parameters and closing a response do not guarantee that a backend stops generation or billing; a hostile engine's reported durations cannot wrap into a plausible rate (B2 tampering) | internal/probe/probe.go |
| M11: Poll/scan/probe timeouts (700ms/1.5s/30s) + context-bounded requests | hung-engine DoS (B2/B3) | discover.go; provider.go; probe.go |
| M12: TOFU host-key store with loud change refusal, 0600 file in 0700 dir, writes serialized within and across processes (mutex plus `known_hosts.lock` over the whole read-modify-write, stale locks broken), temp-file plus rename with the directory entry flushed (core.SyncDir, shared with the self-update install), and unparsable, conflicting-duplicate or record-free stores failing the read | silent MITM after first contact (B3 spoofing); lost pins under concurrent Connect, in one process or two; corruption or an appended line read as a shorter store, forcing a re-TOFU | knownhosts.go; core/fs.go |
| M13: Banner deadline lifted only on complete version line; 15s command timeout; keepalive with bounded probe waits; SupportedAlgorithms (no ssh-rsa SHA-1 / DSA); forwardDialTimeout 8s | trickle/silent-peer hangs (B3 DoS); weak host-key algorithms; hung tunnel dial (B3b) | client.go |
| M14: Local-only defaults: forward listeners on 127.0.0.1, ingest on 127.0.0.1:8420 | accidental network exposure (B1 widening; B3b stays local) | client.go; main.go |
| M15: Routable-bind warning at ingest startup | silent widening of B1 to the network (visibility control; the widening itself remains possible) | endpoints.go |
| M16: Remote shell scripts: static bodies, only locally generated integers interpolated; no secret material sent to remote scripts | command injection into remote shell (B3 elevation) | discover.go; stats.go |
| M17: Password prompt gated on TTY; encrypted keys skipped with guidance | credential handling in headless runs (B4) | auth.go |
| M18: Self-update verification: ValidateRepo (owner/name charset, no path/query), url.JoinPath, GitHub-host asset URLs, redirect pin, refuses without checksums asset, SHA-256 match required before rename, 256 MiB size cap, 2 MiB decompressed checksums cap, temp-file-plus-atomic-rename install | path traversal / SSRF / tampered/truncated/unbounded/gzip-bomb downloads reaching execution (B5) | selfupdate.go |
| M19: Flag validation exits 2; `--interval` below 50ms or above 1h rejected (bare numbers are nanoseconds); set-but-invalid `TOKTOP_COLUMNS`/`TOKTOP_LINES` (outside 41-1024 / 21-512) exit 2 under `--once` (and are named as ignored without it); non-TTY stdout aborts the live dashboard; a missing `agents.json` is a no-op but a malformed one exits 2 rather than watching a reduced agent set; unknown `TOKTOP_*` env warned; empty `--ingest` rejected; `--add` userinfo rejected; startup config line redacts bearer | misconfiguration acting as silent security-relevant behavior change: empty ingest bind exposing every interface, unitless `--interval 1` hammering engines, oversized `--once` frame OOM, and a silently reduced `--agents` watch set | validate.go validateFlags, validateOnceEnv, validateIngestAddr; endpoints.go validateAddURL; main.go logActiveConfig; | 
| M20: Supply chain: govulncheck in CI, Dependabot, SHA-pinned workflow actions, SBOM in releases, tag-name identifier check. CI runs with `permissions: contents: read` (.github/workflows/ci.yml, 8); the release workflow holds `contents: write` (.github/workflows/release.yml, 7), which is the scope that can push a tag and its assets, so it is the one token a workflow or runner compromise would use to poison the update channel of summary risk 3 | vulnerable-dependency drift (deployment surface); release-channel write scope | .github/workflows/ci.yml, .github/dependabot.yml, .github/workflows/release.yml, 7, Makefile |
| M21: Ingest POSTs carrying an `Origin` header refused with 403 (browsers always send Origin on cross-site writes; scripts and agents never do; the endpoint's Content-Type blindness would otherwise let `text/plain` POSTs sail past CORS preflight) | browser-driven dashboard forgery from any visited web page (B1 spoofing) | server.go; tests internal/ingest/server_test.go; README "Agent feed API" documents it |
| M22: Remote discovery ports from the `/proc/net/tcp` sweep parsed as 16-bit with port 0 rejected, so hostile `/proc/net/tcp` output cannot plant impossible forward targets; pinned by FuzzParseDiscoveryOutput. Not covered: the shell-probe fallback taken when that sweep returns nothing parses the remote's stdout with `strconv.Atoi` and checks only `p > 0` (discover.go, 41-48, the parse at :45), so a hostile remote answering on that path can put an out-of-range port into `Discovery.Listening` and have it forwarded (gap 9) | tunnel-set manipulation by a hostile ssh remote (B3 elevation/DoS) | remote/discover.go; internal/remote/fuzz_test.go |
| M23: `--agents` opt-in; `--opencode-db` is a second gate on top of the `sqlite` build tag, on by default with `--agents` and turned off with `--opencode-db=false`; crush has no extra flag because the database lives in the watched project | silent process/file scan the operator did not ask for (B7 disclosure) | main.go; agentusage/source.go; crush_sqlite.go |
| M24: SQLite session stores opened `mode=ro` with `_query_only=1`, `_defensive=1`, `_dqs=0`, and `trusted_schema=OFF`; crush walk capped at 16 parents; counters rejected above 1<<40; opencode directory list bound as parameters | accidental writes into agent databases, planted-schema SQL during a read, walk-to-root, overflow, and SQL injection via cwd (B7) | agentusage/sqlite.go; crush_sqlite.go; sample.go; opencode_sqlite.go |
| M25: Structured request metadata to stderr; bodies excluded; caller-controlled X-Request-Id provides correlation only | B1 repudiation: successful POSTs visible at debug/info, suppressed at warn/error; error also suppresses 4xx. No durable storage or authenticated sender attribution | internal/ingest/server.go; internal/ingest/server_test.go |
| M26: Ingest response headers (nosniff, DENY framing, CSP `default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'`, CORP same-origin, `Cache-Control: no-store`, `Referrer-Policy: no-referrer`) and MaxHeaderBytes 16 KiB | a fetched JSON body sniffed as HTML or framed when `--ingest` is exposed (B1 disclosure); header-bomb DoS | server.go, 81, 100-107 |
| M27: logRemote rewrites non-loopback peer addresses to `"remote"` on the audit line and on http.Server.ErrorLog | peer-IP disclosure when `--ingest` is bound off loopback (B1 information disclosure) | server.go |
| M28: Keyboard-interactive answers only a single non-echoing prompt | a hostile sshd harvesting the password across extra or echoing prompts (B4 disclosure) | auth.go |
| M29: Transcript, candidate-walk, and crush paths opened under `os.OpenRoot`; a planted symlink out of the store is refused. The ownership scan is capped at `ownerScanLines` and refused durably when no header is found, so an undecided session is not re-read on every poll | same-user (or writable-store) redirect of `--agents` reads to arbitrary files (B7 disclosure) | agentusage/watcher.go, 296; agentusage/candidates.go, 121 (`walkTranscripts`); agentusage/crush_sqlite.go, 118 |
| M30: In-flight POST cap. `eventSlots` is a process-wide counting channel of `maxInFlightEvents` 64 acquired before the body is read; a POST that cannot take a slot is refused immediately with 503 and one shared `Retry-After: 1` (RFC 9110 whole seconds) rather than queued. `handleHealth` reports the same degraded state with the same delay, so a probe that keeps saying `ok` while every POST is refused cannot be believed. `stallReason` names which of the two 408 bounds broke (`no body bytes for 1m` vs `stream exceeded the 10m lifetime`), since the raw i/o timeout is identical for both and the two have opposite sender-side fixes | a local pile-up of stalled POST bodies holding an fd and goroutine each for a minute (B1 DoS); a sender or health probe inventing a shorter retry interval than the slots free (B1 DoS) | internal/ingest/server.go, 372-387, 442-458, 466-478, 529, 599 |
| M31: `--ssh-key` is rejected when set to an empty value (exit 2). An empty value is indistinguishable from the flag never being given: the run silently fell back to `~/.ssh/config` and authenticated with a key the operator did not choose | silent identity substitution from a mistyped or quoted-empty flag (B4 disclosure) | cmd/toktop/validate.go, 206-220; cmd/toktop/main.go, 130-133 |
| M32: `currentUser` validates `USER` and `USERNAME` through `validTargetField` before handing either to the transport and falls through to the passwd database when either is empty or would fail validation; the passwd name is passed through `basenameLogin` so a Windows `DOMAIN\user` yields `user`. `TOKTOP_SSH_PASSWORD` moved behind the exported `remote.PasswordEnv` constant so the startup warning that names an unusable variable spells it the way the code reads it | a hostile environment redirecting the ssh connection to a different account, or naming an invalid string the transport would reject later as a password failure (B6) | internal/remote/client.go, 81-104; internal/remote/target.go, 258; internal/remote/auth.go, 93-107; cmd/toktop/validate.go, 137-147 |
| M33: A repeated `ssh://` target resolves to one attachment, keeping the first, keyed on ASCII-folded host plus user, port, and key file; `Forward` reuses the listener a port already has and returns the same local port instead of binding a second one that no map entry reaches. Vendor CLI stdout is capped at `maxToolOutput` 1 MiB through a `cappedOutput` writer that fails the write and reports a miss; over the cap the tool's read end closes under it | one host attached twice: a second ssh connection, a second set of loopback relays (widening B3b), and double-counted engine rows, since the UI keys rates by endpoint (B3b availability and dashboard integrity); unbounded memory growth from a wedged or hostile `nvidia-smi` on `$PATH`, sampled several times per tick (B5 DoS) | cmd/toktop/main.go, 548-575; internal/remote/client.go, 532-570; internal/gpu/gpu.go, 90-121 |

Documentation claims checked against code on 2026-09-27. The first four
bullets are what the current pass found: this file had drifted behind five
commits of security-relevant change, and the site paragraph had been
re-anchored three times in one day without the drift that caused it being
addressed. The list after them records claims that hold as written, so the
next pass has something to re-check rather than a narrative of what used to
be wrong:

- The B1 denial-of-service threat and gap 6 both described the ingest
  endpoint as bounded per connection but unbounded across connections, and
  neither mentioned `maxInFlightEvents`. The cap is the oldest of the five
  commits' worth of drift and the only one that made a threat statement
  wrong rather than incomplete: a flood of stalled local POSTs is now refused
  at the 65th, not merely bounded at the 1-minute reap. B1 DoS and gap 6
  rewritten against the real bound; the residual is now the absence of a
  sender identity on a slot, not the absence of a cap. Recorded as M30.
- Gap 11 claimed a hostile `$USER` is handed to the ssh transport. It is not:
  `currentUser` runs both `USER` and `USERNAME` through `validTargetField`
  and skips a value that would fail, so a malformed one now falls through to
  the passwd database and the transport sees nothing. The claim is narrowed
  to what validation cannot see (any well-formed name is still accepted, and
  `HOME` is not validated at all) and M32 records the control. Gap 11's
  `internal/remote/client.go, 78-89` reference had drifted to 81-89 and is
  re-anchored.
- Four controls did not exist when these lines were written and are now
  recorded: `--ssh-key` set-but-empty rejected (M31), the `$USER` and
  `TOKTOP_SSH_PASSWORD` handling (M32), the repeated-ssh-target and
  per-port forward dedup with the vendor-CLI output cap (M33), and the
  eval_duration range refusal folded into M10. None of them changes the
  ranking in the summary; the repeated-target one narrows a real B3b
  consequence, since attaching one host twice used to open a second set of
  loopback relays and double-count its engines in the UI totals.
- The `strconv.Atoi` in the shell-probe fallback was cited at
  internal/remote/discover.go, :44; it is at :45, and gap 9 now says so.
  The `41-48` range was already right, and the claim itself is unchanged
  and re-verified: `p > 0` is still the only check on that path.
- The site paragraph's references had drifted again, for the fourth time in
  one day: the file is 813 lines, not the 751 the paragraph cited, and every
  anchor had moved 30-60 lines. Re-anchored: the method guards at :678-681
  and :736-739, `/health` at :740-770, the `env.ASSETS` forward at
  :688-702, `logFailure` at :599-608, `failRequest` at :573-582,
  `IMAGE_PATHS` at :631-636, the 406 at :783-785, the unbound-`env.ASSETS`
  404 at :685-687, the unhandled-500 path at :660-672, and `ERROR_HEADERS`
  at :529-533. Every site reference now names the symbol it points at as well
  as the line, so the next pass can grep the symbol and see the drift without
  trusting a number that a comment insertion moved. Earlier passes corrected
  the same references three times; those corrections are gone rather than
  appended, because a list of what was wrong last week is a fourth thing to
  keep current and reads as a claim about the code when it is not one.

Other claims checked against code on this pass, all of which hold as written:

- `/health` answers 503 with
  `degraded: no asset binding; the dashboard captures are not served` when
  `env.ASSETS` is unbound, and `make site-deploy` polls it with `curl -fsS`
  (Makefile, :384), so a deploy that shipped without its assets fails the
  gate instead of waiting out a green probe. The poll is still only a
  presence-and-binding check: an older live version with its bindings answers
  `ok` for an upload that never took effect.
- Every answer carries `server-timing: edge;dur=<ms>`. It names the worker,
  not an origin, so it adds no disclosure.
- `failRequest` (:573-582) adds `error: String(err?.message ?? err)` to the
  unhandled-500 line, so a thrown message from the asset store or the runtime
  reaches Workers Logs alongside the caller-controlled `cf-ray`. The
  caller-controlled set is unchanged in kind: JSON-encoded, so still
  caller-chosen strings in a log stream, not injection.
- The bearer call sites re-verified: `Set` at :40-48, `Allow` at :57-66,
  `Apply` at :70-77, `admits` at :114-122, and `CheckRedirect` at :97-113
  (internal/bearer/bearer.go); the four `Apply` call sites are
  provider/discover.go, 72 and 287, provider.go, 79, and probe.go, 448.
  `Apply` sets the header only when the request's origin is in the allow map,
  and the only writer to that map is `bearer.Allow` at cmd/toktop/main.go,
  207, reached solely from `--add` URLs.
- The uncapped identification decoders are at
  internal/provider/discover.go, 254, 308, 324: none passes an
  `io.LimitReader` to `json.NewDecoder`, while the poll path does
  (internal/provider/provider.go, 101, 115). Gap 5 stands.
- M5's unknown-kind rule holds: internal/ingest/event.go, 63-70 keeps a
  nonempty unknown kind (lowercased, sanitized, 24-rune cap) and only an
  empty or sanitize-to-empty kind becomes `turn`.
- M13's ssh algorithm claim was re-checked against
  golang.org/x/crypto v0.57.0 `ssh/common.go`, 163-176 and upheld:
  `ssh.SupportedAlgorithms()` carries only RSA-SHA2, ECDSA and Ed25519 host
  keys, no `ssh-rsa` and no DSA.
- M29 names three `os.OpenRoot` sites, not two: `walkTranscripts`
  (agentusage/candidates.go, 121) is a third, re-run every second over each
  transcript root.
- M20's supply-chain list holds, including that CI runs
  `permissions: contents: read` while the release workflow holds
  `contents: write` (.github/workflows/ci.yml, 8;
  .github/workflows/release.yml, 7). That scope is what a workflow or runner
  compromise would use against the update channel of summary risk 3.
- The agent-store asset names dsh, crush, and opencode, and the default
  `--agents` path also tails claude, codex, qwen, copilot, clanker, and the
  built-in pi, prime-agent, and feynman definitions
  (agentusage/registry.go, 93-121; agentusage/claude.go, 15; codex.go, 31).
- The ingest route registers GET and HEAD on `/healthz`
  (internal/ingest/server.go, 257 and the 405 `Allow` header); the
  `help` and `version` subcommands take arbitrary trailing arguments
  (cmd/toktop/main.go, 51-65).
- `XDG_CONFIG_HOME` and `XDG_DATA_HOME` are honored only when absolute. A
  relative `XDG_CONFIG_HOME` would write the known_hosts store under the
  working directory, where a later run from another directory would not find
  the pins and would re-TOFU; the same rule for `XDG_DATA_HOME`
  (agentusage/opencode_sqlite.go) costs an empty agent panel rather than a
  lost pin. Both are named as ignored at startup.
- README states a malformed `agents.json` "is reported at startup rather than
  silently shrinking the watch" (README.md, 92-95), which matches the loader
  exiting 2 (cmd/toktop/main.go, 143-152). README "Zero vendor libraries",
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
  `/releases/latest` (:184-186), the named repo defaults to `maci0/toktop`
  (:40-41), and `NewerThan` is exact-tag inequality (:217-222), so there is
  no channel of old majors to support. It invents no reporting SLA, and its
  link to this file resolves.

Single points of failure: the render-time sanitizer (core/sanitize.go) is the
only control standing between all untrusted text and the terminal; every new
UI row must remember to call it. The TOFU callback is the only ssh
authentication-of-host control. The release checksums.txt is the only
integrity anchor for the entire update channel. Each carries several
high-impact threats alone.

## Abuse cases (documented, not demonstrated)

1. **Dashboard poisoning.** A hostile local process (or LAN peer after an
   `--ingest 0.0.0.0` start) POSTs NDJSON naming agent "claude" with huge
   token rates and a plausible note; a browser cannot do this (M21) but any
   script or agent on the host can, without authenticating. Enabling path:
   ingest handlePost -> collector.RecordAgent (collector.go) -> UI
   agents feed. The operator's view of "which agent is burning tokens" is now
   attacker-chosen; nothing distinguishes forged rows from agentwatch-sourced
   ones.
2. **Bearer token capture via operator-named endpoint** (downgraded from the
   previous "capture via any scanned port", which M1 closed): whatever
   listens on an origin the operator `--add`ed receives
   `Authorization: Bearer <token>` on identification, poll, and probe
   requests (bearer.go; main.go). A typo'd or stale `--add` URL
   pointing at attacker-controlled space discloses the gateway key. Scanned
   ports and ssh-forwarded engines no longer receive the header.
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

1. **No origin authentication on ingest** (risk 1, Medium): server.go.
   The routable-bind case is at least announced now (main.go) and the
   browser sender class is refused (M21); every non-browser local process
   still forges rows unchallenged, and a hostile script can simply omit
   `Origin`.
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
5. **Identification responses are decoded without a body cap** (Low-Medium,
   re-verified): the engine-identification decoders in
   `json.NewDecoder(resp.Body)` take no `io.LimitReader`, unlike the poll path
   (internal/provider/provider.go, 88, 113;
   internal/provider/discover.go, 254, 308, 324). Enabling path: any process that
   answers on a scanned well-known port, or any engine reached through an ssh
   relay, can stream a JSON body that never ends. The 700 ms `scanTimeout`
   client (internal/provider/discover.go, 20, 62) bounds how long, not how
   many bytes: a loopback peer can push a large volume inside that window, and
   a slow-drip body holds the discovery goroutine until the timeout fires.
   Applying the existing 4 MiB cap to the identification decoders belongs to
   sec-review.
6. **Ingest slots are a count, not a per-peer quota** (Low, re-scoped this
   pass): the endpoint refuses more than `maxInFlightEvents` 64 concurrent POST
   bodies and advertises `Retry-After` (M30, server.go, 372-387), so a flood
   is refused rather than absorbed. What the cap does not carry is a sender
   identity: one local process can hold every slot, and the 65th honest sender
   is refused alongside it. Per-peer attribution needs a peer notion this
   endpoint does not have (it redacts non-loopback addresses to `"remote"` on
   the audit line, M27, so even the log could not separate them). Gap 1's
   exposure question still governs whether this matters.
7. **Hot-reload and PATH-based tool execution** (risk 5, Low): acceptable for
   a same-user dev tool; revisit if toktop ever runs privileged. Unix-only
   for the re-exec half.
8. **Site deploy runs from a developer shell** (Low): `make site-deploy`
   fetches wrangler from the npm registry at run time and uses ambient
   Cloudflare credentials (Makefile, 397-415). The credential's blast radius
   is a Cloudflare account, and the deploy tool is not lockfile-pinned; a
   registry compromise or a hijacked developer machine reaches the published
   site. The `/health` poll and `site-rollback` are the only recovery
   controls: the poll proves the site answers with its asset binding, not
   that this tree is what answers, and `dist/site.lock` serializes the two
   make targets against each other but not against a deploy run outside
   them. A rollback run
   twice is the one repeat that does damage here, which is why it needs
   `dist/site.deployed`: it is a no-op rather than a second undo.
9. **Discovery port bound missing on the shell-probe fallback** (Low, new
   re-verified): when the `/proc/net/tcp` sweep returns nothing, `Discover`
   falls back to probing well-known ports through the remote shell and
   parses the remote's stdout with `strconv.Atoi`, accepting anything
   `p > 0` (internal/remote/discover.go, 41-48, the parse at 45).
   `FuzzParseDiscoveryOutput` covers the `/proc/net/tcp` parser only, so the
   fallback is the one path
   where a hostile remote can name an out-of-range forward port
   (`Discovery.Listening` -> `ForwardSet`, discover.go, 44-48). The forward
   simply fails to dial such a port, so the impact is a poisoned tunnel set
   and failed probes, not code execution. Bounding the fallback the way
   `parseNetTCP` is bounded, and extending the fuzz target to it, belongs
   to sec-review.
10. **A watched transcript's own `cwd` decides session ownership** (Low, new
    re-verified): `owns` reads a working directory out of the session JSON
    and compares it to the watched directory
    (agentusage/transcript.go, 263 and 307; `sessionCwd` at claude.go, 19,
    codex.go, 16). A writer of a watched store can therefore claim a
    session belonging to another project, or disown its own, moving token
    counts between directory rows. It is an attribution-integrity issue
    within the operator's own stores, not a boundary crossing, and the
    `ownerScanLines` cap means the scan is bounded. Belongs to sec-review if
    the attribution is meant to be authoritative.
11. **A hostile environment still chooses the ssh login account and the
    home path roots** (Low, narrowed this pass): `currentUser` validates
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

## Response readiness (notes only)

- **Audit trail:** POST /v1/events, 404/405, and recovered handler panics
  emit structured stderr metadata, subject to `TOKTOP_LOG_LEVEL`
  (internal/ingest/server.go).
  Default info includes successful POSTs; warn/error suppress them, and
  error also suppresses 4xx rejections. Successful health checks are not
  logged (server.go). Request ids can be supplied by callers
  (server.go), so they provide correlation, not sender identity.
  A body that trips a deadline now names which bound it broke, in the 408
  body rather than a log line (M30, server.go, 442-458), so an operator can
  tell a peer that stopped sending from one whose stream outlived the
  10-minute bound without reading the source for which branch fired.
  Event bodies, notes, and token counts are not audit attributes; there is
  no credential-redaction guarantee for arbitrary caller-supplied log
  fields (server.go). The feed is bounded in-memory state
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
- A reference into a file that changes often names the symbol as well as the
  line. `site/worker.js` grew 62 lines in a day and moved every citation in
  this file; a symbol survives a comment insertion, a line number does not.
- New entry point in code => add it here in the same change.
- Fixed vulnerabilities move from "Gaps" into the mitigations table with the
  commit that closed them.
