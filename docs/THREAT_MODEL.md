# toktop threat model

Living document for the security owner: the whole attack surface on one screen,
with file references so each claim can be re-verified against code. Individual
vulnerabilities and their fixes belong to sec-review; this file records where
they live and what already stands in their way.

- **Last reviewed:** 2026-09-18 (ingest audit coverage only; other sections retain
  their earlier verification dates and may contain stale line references)
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
| 1 | Ingest endpoint accepts unauthenticated events from any local process (any network peer if bound non-loopback via `--ingest`); forged telemetry renders as real agents | local processes -> ingest, network -> ingest | internal/ingest/server.go, cmd/toktop/main.go | No authentication; browser-driven forgery, DoS, injection, mixed-script names, and timestamp forgery mitigated (M3-M7, M21, M26) |
| 2 | Trust-on-first-use accepts a first-contact MITM by design; only key *changes* are refused | operator -> ssh target | internal/remote/knownhosts.go | Documented residual risk (README "Auth" section) |
| 3 | Self-update installs whatever binary the named GitHub repo published: integrity rests on the release's own checksums.txt over TLS; no external signature exists | runtime -> update channel | internal/selfupdate/selfupdate.go, .github/workflows/release.yml | Checksum + size + GitHub-host URL verification present; owner/name validated (M18); signing absent |
| 4 | SSH engine relays bind loopback listeners (`127.0.0.1:0`); any local process can reach the remote engines those listeners front | local processes -> remote engines | internal/remote/client.go | Bound loopback-only; no listener auth |
| 5 | Hot-reload re-execs whatever binary occupies the exe path when its identity changes (Unix); PATH-based vendor CLI lookup executes tools from `$PATH` | build -> runtime, host -> process | internal/selfreload/exec_unix.go; internal/gpu/gpu.go | Windows Restart does not exec (exec_windows.go); `--no-hot-reload` exists |
| 6 | Low: ingest poisoning cannot be reconstructed from retained payloads; raising the log floor also hides successful submissions | B1, response readiness | internal/ingest/server.go; internal/collector/collector.go | Request metadata logs at info by default; warn/error suppress successes. No authenticated sender identity or durable event store |

Resolved since 2026-08-25: the previous ranking's "bearer token sent to every
probed endpoint" is closed by origin-scoped token application
(commit 21e3feb); see mitigations M1. The previous claim that `--repo` is
interpolated into the GitHub API path with no owner/name check is closed by
`ValidateRepo` (selfupdate.go, called from Check at :171 and from
`toktop update` at cmd/toktop/update.go). Nothing in this table is
demonstrated by attacking anything; every claim cites code read at review time.

## Assets

What is worth stealing, corrupting, or denying:

- **Engine credentials**: one process-wide bearer token
  (internal/bearer/bearer.go), sourced from `--bearer`,
  `OMNIROUTE_API_KEY`, or `TOKTOP_BEARER` (cmd/toktop/main.go,
  821-831). Grants API access to gateways like OmniRoute.
- **GitHub credential**: optional `GITHUB_TOKEN`, read from the environment
  during `toktop update` and sent only to api.github.com
  (internal/selfupdate/selfupdate.go). Rate-limit relief, not a secret
  with broad scope, but it leaves the host on every update check that sets it.
- **SSH credentials**: private keys read from `--ssh-key`, `~/.ssh/config`
  IdentityFile, or `~/.ssh/id_*` defaults (internal/remote/auth.go,
  44-60), the ssh-agent socket (auth.go, 183-189), and the password
  from `TOKTOP_SSH_PASSWORD` or the terminal prompt (auth.go).
- **Host-key pin store**: `os.UserConfigDir()/toktop/known_hosts`
  (internal/remote/knownhosts.go). Corrupting it enables a forced
  re-TOFU.
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
  agent home dirs, dsh's default `session.v<N>.jsonl.zstd` (concatenated zstd
  frames, agentusage/dsh.go), crush's project database `.crush/crush.db`
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
  components in event notes, internal/agentwatch/agentwatch.go) appear
  on screen and in scrollback; screen shares and captures leak them. Home
  prefixes are rewritten to `~` so a username in those components does not
  become the note (shortDir/stripHome, :439-463).

## Entry points

Every externally reachable input, with its code location:

1. **Ingest HTTP server** (on by default): `POST /v1/events` (single JSON or
   NDJSON stream), `GET /healthz`
   (internal/ingest/server.go). Binds `127.0.0.1:8420` unless `--ingest`
   says otherwise (cmd/toktop/main.go); any address is accepted, including
   routable interfaces. An empty `--ingest` is rejected (validateIngestAddr,
   main.go) because `net.Listen` would treat it as `:0` (every
   interface, ephemeral port). A routable bind prints a startup warning naming
   the unauthenticated exposure (main.go, routableBind :413-423). Runs
   in demo mode too. `--no-ingest` turns it off (main.go).
2. **CLI arguments**: top-level flags including `--bearer` (secret),
   `--ssh-key`, `--add URL` (repeatable), `--ingest ADDR`, `--agents`,
   `--opencode-db` (cmd/toktop/main.go); positional
   `ssh://[user@]host[:port]` targets (interpretArgs / ParseTarget); and the
   `update` subcommand with `--check` and `--repo owner/name`
   (cmd/toktop/update.go). `--repo` is checked with `ValidateRepo`
   (owner/name only). `--add` rejects non-http(s), missing host, and userinfo
   (validateAddURL, main.go). An `ssh://` URL that embeds a password,
   path, query, or fragment is rejected at startup (internal/remote/target.go).
   The live dashboard refuses to start when stdout is not a terminal
   (main.go); `--once` is the non-TTY path.
3. **Environment variables**: secrets `OMNIROUTE_API_KEY`,
   `TOKTOP_BEARER`, `TOKTOP_SSH_PASSWORD`, `GITHUB_TOKEN`; plus
   `SSH_AUTH_SOCK`, `TOKTOP_COLUMNS`/`TOKTOP_LINES`, `TOKTOP_LOG_LEVEL`
   (cmd/toktop/main.go; internal/remote/auth.go; internal/selfupdate;
   internal/ingest), `GAUNTLET_HOME` (agentusage/definitions.go),
   `XDG_DATA_HOME` (agentusage/opencode_sqlite.go, only when
   `--opencode-db` is on, which it is by default with `--agents`), and
   `TOKTOP_SCREENSHOT_FONT` (scripts/screenshot.py
   only; the binary ignores it).
4. **Self-update network fetches** (outbound HTTPS): latest-release lookup on
   api.github.com, then download of the checksums archive and platform asset
   named by that response (internal/selfupdate/selfupdate.go).
   Asset URLs must be GitHub download hosts (trustedAssetURL, :144-150);
   redirects off those hosts are refused (githubRedirect, :155-163). The asset
   is verified against the downloaded checksums and size-capped before anything
   is renamed over the running binary (:258-301).
5. **SSH client sessions** (outbound): shell scripts executed on the remote
   for discovery and vitals (internal/remote/discover.go;
   internal/remote/stats.go); all returned text is parsed locally, and
   discovery ports are parsed as 16-bit with zero rejected
   (discover.go).
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
   (internal/agentwatch/agentwatch.go; agentusage/watch.go),
   including dsh's default concatenated-zstd logs (agentusage/dsh.go). With
   the `sqlite` build tag, crush's `.crush/crush.db` is opened automatically
   for each watched working directory (agentusage/crush_sqlite.go).
   opencode's machine-wide SQLite store is a second gate: the tag plus
   `--opencode-db`, on by default with `--agents` and disabled with
   `--opencode-db=false` (cmd/toktop/main.go; agentusage/source.go).
   Agent definitions, including transcript roots, load once at startup from
   `$GAUNTLET_HOME/agents.json` or `~/.gauntlet/agents.json`; a malformed
   file warns on stderr and leaves only the built-in set
   (cmd/toktop/main.go; agentusage/definitions.go).
9. **Config files read at startup**: `~/.ssh/config` (HostName/User/Port/
   IdentityFile override target fields, internal/remote/target.go)
   and the known_hosts store (knownhosts.go).
10. **Self hot-reload**: polls the running executable's stat identity and
    restarts into it when changed (internal/selfreload/selfreload.go,
    exec_unix.go; cmd/toktop/main.go). Default-on in interactive
    Unix runs; `--no-hot-reload` disables. Windows Watch still fires, but
    Restart only prints and the process then exits (exec_windows.go).
11. **Local system introspection**: `/proc` and sysctl reads
    (internal/procs/, internal/sysmon/), vendor CLIs executed from `$PATH`:
    nvidia-smi, rocm-smi, xpu-smi (internal/gpu/gpu.go),
    system_profiler and ioreg (internal/gpu/gpu_darwin.go), ps
    (internal/procs/procs_darwin.go; agentusage/discover_darwin.go),
    lsof (agentusage/discover_darwin.go; peers_darwin.go), a PowerShell
    CIM query (internal/procs/procs_windows.go).

Deployment surface:

- GitHub Actions CI runs gofmt/vet/`go mod tidy -diff`/govulncheck/staticcheck/
  race tests on pushes and PRs, plus `bun test site/` and screenshot-script
  lint (.github/workflows/ci.yml, actions pinned by SHA, bun from
  `.bun-version`, uv 0.12.6, Python tools from `scripts/requirements-dev.txt`);
  Dependabot updates modules, actions, and `scripts/` pip deps
  (.github/dependabot.yml); tag pushes build release binaries for six
  platforms plus a CycloneDX SBOM (.github/workflows/release.yml, Makefile
  `release`/`sbom`). Release artifacts ship SHA-256 checksums only; no
  signature step exists. Tag names that reach ldflags and dist filenames are
  refused unless they are a safe identifier (release.yml).
- The marketing site is a single Cloudflare Worker serving one static page
  from an embedded string (site/worker.js): GET/HEAD only (:383-388), a
  `/health` route (:389-404), ETag revalidation with a weak validator
  (:407-417), content negotiation (brotli, zstd, gzip, identity)
  compressed once per isolate, keyed by `Vary: Accept-Encoding` on every
  page response (VARY / PAGE_HEADERS, :308-330), and hardening headers
  (nosniff, HSTS, referrer-policy, CSP
  `default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:;
  base-uri 'none'; form-action 'none'; frame-ancestors 'none'`) on every
  page and image response (SECURITY_HEADERS, :313-320). wrangler.jsonc sets
  `assets.run_worker_first` so /dashboard.png and related images hit that
  Worker path instead of the asset pipeline; it also publishes the worker on
  workers.dev and binds toktop.ai / www.toktop.ai. The only request bytes
  inspected are the method, path, If-None-Match, If-Modified-Since, and
  Accept-Encoding headers, compared as strings; nothing is stored, echoed
  into the page, or forwarded anywhere. `style-src 'unsafe-inline'` is idle:
  the HTML is a compile-time string.

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
  (numbers parse-or-zero, e.g. stats.go and gpu.go; ports parse as
  16-bit with zero rejected, discover.go) and the renderer's sanitizer.
  Remote command stderr is sanitized before it is appended to a local error
  (client.go).
- **B3b: local processes -> remote engines (via B3 relays).** Forward's
  loopback listeners (client.go) let any local process send bytes to
  engines on the ssh target. No authentication on the listener; the ssh
  connection is the privilege transition. Closed when the client dies.
- **B4: secrets -> code.** Bearer token: enters via argv or env
  (main.go), lives in a package var (bearer.go), leaves
  in the `Authorization` header of requests bound for origins admitted by
  `Allow`, populated solely from operator-named `--add` URLs (main.go);
  discovery scans, forwarded remote engines, and probes elsewhere never carry
  it (bearer.go gates Apply at every call site: discover.go;
  provider.go; probe.go). There is no `bearer.Token` accessor; the
  only outbound path is Apply. It is never written to disk or logs.
  SSH password: env or TTY prompt (auth.go), held in memory, used only
  for password and keyboard-interactive mechanisms. Keys: read from disk or
  agent, sign locally. `GITHUB_TOKEN`: env, sent only to api.github.com
  (selfupdate.go). Rotation points: none; all credentials are static
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
  CLIs execute (gpu.go). All three are same-user-writable inputs treated
  as trusted.
- **B7: host filesystem -> agentusage (opt-in).** With `--agents`, process
  discovery plus transcript/SQLite reads cross from other processes' files
  into the dashboard. The operator never names those files; `agents.json`
  roots, crush's walk to `.crush/crush.db` (crush_sqlite.go, capped at
  16 parents), and opencode's well-known path do. SQLite opens are
  read-only (sqlite.go, `_query_only=1`, `_defensive=1`, `_dqs=0`,
  and `trusted_schema=OFF` in the DSN).
  Transcript and crush paths that leave their root via a planted symlink are
  refused (watch.go; crush_sqlite.go).

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
- *Repudiation*: POST handlers emit remote, request id, status, and accepted
  count at info on success, warn for rejection/write errors, and error for
  5xx (internal/ingest/server.go). A warn/error log
  floor suppresses successes; error also suppresses 4xx rejections. Request
  ids are caller-supplied correlation labels, not evidence of identity
  (server.go). Event bodies are excluded, non-loopback IPs are
  redacted, and the accepted count describes decoded submissions rather
  than unique retained events (server.go;
  internal/collector/collector.go).
- *Information disclosure*: none beyond presence (`/healthz` answers any
  requester, server.go). Residual: a routable bind would otherwise have
  put peer IPs on stderr; logRemote drops them.
- *Denial of service*: mitigated (see M3-M5). Residual: no per-peer
  connection quota; each open POST holds an fd plus goroutine until the byte
  cap, body-idle timeout (1m), or absolute lifetime (10m) reaps it
  (server.go) (a local flood of stalled POSTs is bounded but
  nonzero). MaxHeaderBytes is 16 KiB (server.go).
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
  PollTimeout 1.5s / probe timeout 30s; bodies capped (M8). Probe generation
  is 32 tokens with think disabled, a content-byte hang-up, a 16 KiB line
  cap, and 429/503 backoff (M10).
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
  Discovery output cannot plant impossible tunnels either: listening ports
  parse as 16-bit with zero rejected (discover.go, fuzz-pinned by
  FuzzParseDiscoveryOutput), so a hostile /proc dump cannot name an out-of-
  range forward target.

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
  warnBearerFlag names it at startup (main.go).
  `TOKTOP_SSH_PASSWORD` in the environment is readable by same-user processes
  and inherited by children (auth.go); `GITHUB_TOKEN` reaches api.github.com
  on every `toktop update` check when set (selfupdate.go).
- *Tampering*: known_hosts store is plain text in the user config dir, mode
  0600 in a 0700 dir (knownhosts.go); a same-user writer can reset pins.
  Writes are serialized (knownHostsMu) and go through a temp file plus rename.

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
  (:174-175). Asset URLs must be GitHub download hosts, and redirects off
  those hosts are refused (:144-163,254-256). The asset must match the
  release's own checksums.txt (:266-291), so a network attacker cannot
  substitute a binary. The trust anchor is the repo itself: whoever controls
  the repo (or its release pipeline) ships a binary that verifies against its
  own checksums. No external signature (Sigstore/GPG) closes that gap today;
  summary risk 3.
- *Denial of service*: downloads capped at 256 MiB asset / 1 MiB compressed
  checksums / 2 MiB decompressed checksums (selfupdate.go);
  a truncated or hostile mirror fails verification and leaves the running
  binary untouched (:289-291).

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
  the transcript root or crush project are refused (M29).
- *Denial of service*: a huge transcript or database is opened read-only and
  queried with bounds (maxSaneTokens 1<<40, agentusage/watch.go);
  JSONL is tailed from the attach point rather than replayed in full
  (agentusage/watch.go). dsh zstd frames are size-capped
  (`zstdMaxFrameBytes`, `WithDecoderMaxMemory` in agentusage/dsh.go).

## Existing mitigations map

Controls verified in code, with the threats they cover:

| Control | Covers | Location |
|---|---|---|
| M1: Origin-scoped bearer application. Token attached only to `Allow`-admitted origins, populated exclusively from operator `--add` URLs | Credential harvesting by scanned ports, forwarded remotes, and probes (B2/B4 disclosure; previous summary risk 1, fixed in commit 21e3feb) | internal/bearer/bearer.go; cmd/toktop/main.go; call sites discover.go; provider.go; probe.go; tests internal/bearer/bearer_test.go |
| M2: Terminal escape/control-char sanitizer applied both at ingest and at render time (C0/C1 including UTF-8-encoded C1; bidi overrides/isolates; zero-width format; variation selectors). Mixed Latin+Cyrillic/Greek agent names collapse to `anonymous` | OSC/CSI clipboard-cursor-title injection from engines, remotes, and events; mixed-script impersonation of agent names (asset: terminal integrity, dashboard integrity) | internal/core/sanitize.go; ingest side server.go; render side internal/ui/ui.go, internal/ui/plain.go, internal/ui/format.go, internal/ui/agents.go |
| M3: Ingest body cap 1 MiB + MaxBytesReader | unbounded upload into decode loop (B1 DoS) | server.go |
| M4: Ingest read deadlines: 10 min absolute lifetime, 1 min idle extension, 5 s header timeout, 2 min idle reap | slowloris/drip DoS (B1) | server.go |
| M5: Event field clamps (agent 64, model 128, note 512, kind 24 runes, id 128) + token clamp (negative or >1<<40 to zero) + retention caps (512 events, 128 probes per snapshot) | memory pinning via oversized or numerous events; wrap of agent totals (B1 DoS/tampering) | server.go; core.AgentHistoryLen / ProbeHistoryLen internal/core/core.go; collector.go |
| M6: Event timestamp skew clamp: stamps >2 min in the future reset to arrival time | forged-future stamps pinning the live marker and feed ordering (B1 spoofing) | server.go |
| M7: Negative/absurd token counts clamped to zero; unknown kinds defaulted | junk values entering retained state (B1 tampering) | server.go |
| M8: Engine response caps: 4 MiB JSON, 8 MiB text, 256-rune error snippets | memory blowup and log flooding from hostile engines (B2 DoS/disclosure) | provider/provider.go httpStatus; core.Snippet |
| M9: Non-finite rejection in metrics (per-value and family-sum overflow guard) and vendor CSV/JSON coercion | poisoned counters/rates propagating through history (B2 tampering) | provider.go; gpu.go; collector counter-reset clamp collector.go |
| M10: Probes request 32 tokens, Ollama `think=false`, OpenAI `n=1`; streaming readers stop after 32 observed content/reasoning units or 1 KiB decoded content/reasoning, with 16 KiB scanner and 128 KiB stream limits. Non-stream OpenAI JSON over 16 KiB is rejected. Reported token counts trusted only up to 128; fixed prompt, model-id cap 256, embed/rerank filtering, and 429/503 backoff 15s–5m | Limits client work and reduces B2 compute/spend amplification; request parameters and closing a response do not guarantee that a backend stops generation or billing | internal/probe/probe.go |
| M11: Poll/scan/probe timeouts (700ms/1.5s/30s) + context-bounded requests | hung-engine DoS (B2/B3) | discover.go; provider.go; probe.go |
| M12: TOFU host-key store with loud change refusal, 0600 file in 0700 dir, serialized writes, temp-file plus rename | silent MITM after first contact (B3 spoofing); lost pins under concurrent Connect | knownhosts.go |
| M13: Banner deadline lifted only on complete version line; 15s command timeout; keepalive with bounded probe waits; SupportedAlgorithms (no ssh-rsa SHA-1 / DSA); forwardDialTimeout 8s | trickle/silent-peer hangs (B3 DoS); weak host-key algorithms; hung tunnel dial (B3b) | client.go |
| M14: Local-only defaults: forward listeners on 127.0.0.1, ingest on 127.0.0.1:8420 | accidental network exposure (B1 widening; B3b stays local) | client.go; main.go |
| M15: Routable-bind warning at ingest startup | silent widening of B1 to the network (visibility control; the widening itself remains possible) | main.go |
| M16: Remote shell scripts: static bodies, only locally generated integers interpolated; no secret material sent to remote scripts | command injection into remote shell (B3 elevation) | discover.go; stats.go |
| M17: Password prompt gated on TTY; encrypted keys skipped with guidance | credential handling in headless runs (B4) | auth.go |
| M18: Self-update verification: ValidateRepo (owner/name charset, no path/query), url.JoinPath, GitHub-host asset URLs, redirect pin, refuses without checksums asset, SHA-256 match required before rename, 256 MiB size cap, 2 MiB decompressed checksums cap, temp-file-plus-atomic-rename install | path traversal / SSRF / tampered/truncated/unbounded/gzip-bomb downloads reaching execution (B5) | selfupdate.go |
| M19: Flag validation exits 2; `--interval` below 50ms or above 1h rejected (bare numbers are nanoseconds); set-but-invalid `TOKTOP_COLUMNS`/`TOKTOP_LINES` (outside 41-1024 / 21-512) exit 2 under `--once` (and are named as ignored without it); non-TTY stdout aborts the live dashboard; malformed `~/.gauntlet/agents.json` warned instead of swallowed; unknown `TOKTOP_*` env warned; empty `--ingest` rejected; `--add` userinfo rejected; startup config line redacts bearer | misconfiguration acting as silent security-relevant behavior change; empty ingest bind exposing every interface; unitless `--interval 1` hammering engines; oversized `--once` frame OOM | main.go validateFlags, validateOnceEnv, logActiveConfig, validateAddURL, validateIngestAddr |
| M20: Supply chain: govulncheck in CI, Dependabot, SHA-pinned workflow actions, SBOM in releases, tag-name identifier check | vulnerable-dependency drift (deployment surface) | .github/workflows/ci.yml, .github/dependabot.yml, .github/workflows/release.yml, Makefile |
| M21: Ingest POSTs carrying an `Origin` header refused with 403 (browsers always send Origin on cross-site writes; scripts and agents never do; the endpoint's Content-Type blindness would otherwise let `text/plain` POSTs sail past CORS preflight) | browser-driven dashboard forgery from any visited web page (B1 spoofing) | server.go; tests internal/ingest/server_test.go; README "Agent feed API" documents it |
| M22: Remote discovery ports parsed as 16-bit with port 0 rejected, so hostile `/proc/net/tcp` output cannot plant impossible forward targets; pinned by FuzzParseDiscoveryOutput | tunnel-set manipulation by a hostile ssh remote (B3 elevation/DoS) | remote/discover.go; internal/remote/fuzz_test.go |
| M23: `--agents` opt-in; `--opencode-db` is a second gate on top of the `sqlite` build tag, on by default with `--agents` and turned off with `--opencode-db=false`; crush has no extra flag because the database lives in the watched project | silent process/file scan the operator did not ask for (B7 disclosure) | main.go; agentusage/source.go; crush_sqlite.go |
| M24: SQLite session stores opened `mode=ro` with `_query_only=1`, `_defensive=1`, `_dqs=0`, and `trusted_schema=OFF`; crush walk capped at 16 parents; counters rejected above 1<<40; opencode directory list bound as parameters | accidental writes into agent databases, planted-schema SQL during a read, walk-to-root, overflow, and SQL injection via cwd (B7) | agentusage/sqlite.go; crush_sqlite.go; watch.go; opencode_sqlite.go |
| M25: Structured request metadata to stderr; bodies excluded; caller-controlled X-Request-Id provides correlation only | B1 repudiation: successful POSTs visible at debug/info, suppressed at warn/error; error also suppresses 4xx. No durable storage or authenticated sender attribution | internal/ingest/server.go; internal/ingest/server_test.go |
| M26: Ingest response headers (nosniff, DENY framing, CSP `default-src 'none'`, CORP same-origin, no-store) and MaxHeaderBytes 16 KiB | a fetched JSON body sniffed as HTML or framed when `--ingest` is exposed (B1 disclosure); header-bomb DoS | server.go |
| M27: logRemote rewrites non-loopback peer addresses to `"remote"` on the audit line and on http.Server.ErrorLog | peer-IP disclosure when `--ingest` is bound off loopback (B1 information disclosure) | server.go |
| M28: Keyboard-interactive answers only a single non-echoing prompt | a hostile sshd harvesting the password across extra or echoing prompts (B4 disclosure) | auth.go |
| M29: Transcript and crush paths opened under `os.OpenRoot`; a planted symlink out of the store is refused | same-user (or writable-store) redirect of `--agents` reads to arbitrary files (B7 disclosure) | agentusage/watch.go; crush_sqlite.go |

Documentation claims checked against code this pass:

- README "Zero vendor libraries", "Engines: how discovery works", and the SSH
  auth chain match the code paths cited above, including ioreg on Darwin.
- README documents the post-21e3feb scoping accurately: "--bearer TOKEN ...
  sent to --add endpoints only" and the environment-variable table listing
  `GITHUB_TOKEN` for `toktop update`; both match bearer.go and
  selfupdate.go.
- README documents the Origin-header 403, the stream-resume semantics, and
  the ingest POST audit log; they match server.go Origin refusal, the fail()
  path, and logRequest.
- README documents loopback listeners for ssh engine relays
  (client.go). The earlier claim that no local listeners exist is
  gone.
- SECURITY.md states there is no dedicated disclosure contact and no
  supported-version matrix, and that `toktop update` fetches the latest
  release. That matches selfupdate.go (Check always hits
  `/releases/latest`; NewerThan is exact-tag inequality, not a channel of
  old majors). It invents no reporting SLA.

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
5. **No per-peer ingest quotas** (Low): bounded per connection
   (server.go) but not per peer; matters only once gap 1's exposure
   question is settled.
6. **Hot-reload and PATH-based tool execution** (risk 5, Low): acceptable for
   a same-user dev tool; revisit if toktop ever runs privileged. Unix-only
   for the re-exec half.

## Response readiness (notes only)

- **Audit trail:** POST /v1/events, 404/405, and recovered handler panics
  emit structured stderr metadata, subject to `TOKTOP_LOG_LEVEL`
  (internal/ingest/server.go).
  Default info includes successful POSTs; warn/error suppress them, and
  error also suppresses 4xx rejections. Successful health checks are not
  logged (server.go). Request ids can be supplied by callers
  (server.go), so they provide correlation, not sender identity.
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
- New entry point in code => add it here in the same change.
- Fixed vulnerabilities move from "Gaps" into the mitigations table with the
  commit that closed them.
