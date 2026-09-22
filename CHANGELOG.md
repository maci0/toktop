# Changelog

Consumer-facing changes to the `toktop` CLI, the ingest `/v1/events` body, and
the `agentusage` Go package. GitHub Releases also attach auto-generated notes
from git; this file is the summary aimed at people upgrading.

The project is 0.x: those surfaces may change without a major bump. `toktop
update` always fetches the latest GitHub release; older tags are not a
support channel (see SECURITY.md).

## [Unreleased]

## [0.13.0] - 2026-09-22

### Changed

- Frame rendering does one agent-rate pass instead of two where both
  directions are needed, filters via-engine events in place instead of
  copying the feed, and pre-sizes the chart series buffer. Rendered output
  is unchanged.
- The `!sqlite` build stubs for the database-backed agent sources live in
  one file instead of two. No behavior change.

## [0.12.0] - 2026-09-19

Latest tagged release. Binaries, checksums, and a CycloneDX SBOM are on
[GitHub Releases](https://github.com/maci0/toktop/releases/tag/v0.12.0).

### Added

- The dashboard opens on the agents view when `--agents` is on and no engines
  were found, instead of the setup card complaining about engines nobody asked
  for. `a` toggles which side gets the panel estate: engines (the default) or
  agents. The side not in focus keeps the header, the shared throughput chart
  and the host strip, so neither half of the machine disappears. The compact
  strip for panes too small for the dashboard drops the missing-engines line
  on an `--agents` run too.

### Breaking

- `--opencode-db` is on by default with `--agents`: a binary built with the
  `sqlite` driver reads opencode's session database without an extra flag.
  Pass `--opencode-db=false` to leave it alone. A build without the driver
  still reports nothing for opencode, and only an explicit `--opencode-db`
  prints the note saying so.

### Changed

- Frame rendering is roughly 45% cheaper and allocates about 63% fewer
  objects: the chart age fade no longer parses a hex color with
  `fmt.Sscanf` for every bisection step of every chart column, reads the
  background's luminance once instead of per comparison, and assembles the
  faded hex without `fmt.Sprintf`. Rendered output is unchanged.

### Fixed

- `agentusage` reads dsh's v3 session logs. The provider's counts are nested
  under `data.usage` on the `assistant/message` record; the reader accepted
  only a top-level `usage` object that no released dsh writes, so every real
  dsh session reported no tokens. Cached input (`cacheReadTokens`,
  `cacheWriteTokens`) now counts as billed prompt tokens, the same fold the
  Claude reader applies, and `totalTokens` supplies the per-call context
  size. `compaction/summary` is still left out, matching what dsh itself
  reports as durable session usage.
- `agentusage` resolves an agent's provider and transcript adapter on every
  poll instead of trusting the binding made at attach. `EnableOpenCodeDB(false)`
  now stops a running watcher from reading opencode's store (it keeps its last
  sample), a definition reloaded with `LoadDefinitions` reaches watchers that
  are already running, and `Watch` no longer writes the adapter registry.

## [0.10.0] - 2026-09-18

### Breaking

- `--agents` exits with status 2 instead of warning and continuing when
  `~/.gauntlet/agents.json` (or `$GAUNTLET_HOME/agents.json`) is malformed
  or unreadable. Before upgrading from 0.9.0, fix the JSON or permissions,
  or move the file aside if custom definitions are not needed. A missing
  file remains valid. `agentusage.LoadDefinitions` already returned errors
  for malformed or unreadable files; this startup change affects the CLI.
- `agentusage.LoadDefinitions` now rejects names with `usage` definitions
  that collide after trimming surrounding whitespace and NFC normalization,
  rather than silently keeping one definition. For example, merge the
  entries `"cafe\u0301"` and `"caf\u00e9"` into one name with the intended
  `usage` settings. Rejection leaves the registry unchanged and matches
  `ErrInvalidDefinitions` via `errors.Is`; `--agents` also refuses the file.
- `agentusage.LoadDefinitions` now rejects a JSON document that is `null`,
  or an entry whose value is `null`, with an `ErrInvalidDefinitions` error
  naming the file; the registry is left unchanged. Before, `null` was
  accepted and a `null` entry was skipped silently. Replace a `null`
  document with `{}`, and replace `{"myagent": null}` with
  `{"myagent": {}}` or remove that entry. `"usage": null` inside an object
  remains valid. With `--agents`, rejected definitions now exit with status 2.
- `agentusage.Rate` and `agentusage.InputRate` return `(0, false)` when
  either sample lacks a timestamp. In 0.9.0, a zero-value previous sample
  followed by a timestamped, growing counter could return a spurious small
  rate with `true`. Wait for two timestamped readings and check the boolean
  result before displaying a rate. For a measured zero-counter baseline,
  set `Sample{At: start}` to the actual start time rather than `Sample{}`.
- Generation probes no longer follow HTTP redirects, including same-origin
  redirects. In 0.9.0, a 307 or 308 could replay a generation POST at the
  redirect target. Probes now fail with the redirect's HTTP status instead.
  Set `--add` to the final engine or gateway URL, or configure that endpoint
  to serve `/api/generate` or `/v1/chat/completions` without redirecting.
- `toktop help extra` and `toktop version --help extra` exit 2 instead of
  printing help. `toktop help update extra` was already a usage error.

### Security

- Engine discovery and polling strip `Authorization` when redirected to a
  different origin (scheme, host, or port), including subdomains and HTTPS
  downgrades. In 0.9.0, some such redirects could receive the bearer token.
  If an authenticated gateway relies on redirects, set `--add` to its final
  trusted URL; same-origin redirects still retain the configured token.
- Ingest error and write-failure logs no longer include the peer address.

### Changed

- Ingest `Idempotency-Key` ids now use the first eight bytes of the key's
  SHA-256 hash as 16 hexadecimal characters, followed by `:N` for the
  1-based line index, instead of `key:N`. Keys received by the handler are
  hashed without truncation or whitespace collapsing, avoiding the previous
  systematic collisions; the truncated hash is not collision-free. Senders
  retrying the same body with the same header need no change. To control
  the event id across versions, supply an explicit body `id`, which still
  takes precedence over the header.
- Recaptured the README and toktop.ai dashboard screenshot from a 0.9.0
  demo frame.

### Fixed

- `agentusage.Sample.Total` now takes the largest context size across
  watched transcript files instead of adding each file's maximum. Values
  can be lower than in 0.9.0 when several transcripts are watched; `Input`
  and `Output` remain accrued token counts, not context sizes.
- `agentusage.Agents` includes names added through `RegisterSpec`, so process
  discovery on Linux and macOS can find those agents. In 0.9.0, only built-in
  names and names loaded from definitions were listed. Results remain sorted,
  deduplicated, and safe for callers to modify.
- `--once`, help, version, and update output paths report stdout write errors
  with status 1 instead of reporting success. Scripts should check the exit
  status before treating redirected output as complete; repair the output
  destination before retrying. An update may already be installed if writing
  its final confirmation fails.
- `--add` rejects URLs without a hostname (such as `http://:8080`) and ports
  above 65535 at startup instead of accepting unusable endpoints. Supply the
  engine's hostname and listening port; an omitted port still uses the scheme
  default.
- OpenCode session directories on macOS and Windows match with a Unicode-aware
  case-insensitive collation instead of ASCII `lower()`. Crush usage in the
  attach second is counted. A trailing JSONL record at a reader buffer
  boundary is counted. Negative sqlite counters no longer subtract from
  totals. Non-TCP and listening sockets are excluded from peer matching.
- `~/.ssh/config` honors tab and `=` separators and `Host !pattern`
  exclusions. Remembered host keys are accepted on reconnect. Forwards do
  not reopen after teardown. Session creation is bound by the command
  deadline. Remote stderr is kept to a 4k tail. Loaded Ollama models
  survive an SSH hop.
- Windows engine discovery matches names such as `OLLAMA.EXE`. macOS process
  listing splits columns on any whitespace. Process CPU sampling treats a
  newly seen PID as a baseline rather than a delta from zero.
- Prometheus samples parse the value before an optional timestamp. Duration
  metrics are not counted as running requests. A zero tok/s gauge is shown
  rather than treated as missing. The first sample can show a direct tok/s
  gauge. Duplicate provider endpoints collapse to one poller. Remote engines
  no longer inherit local process metrics.
- Generation probes reject oversized non-stream bodies, cap total stream
  bytes, bound reasoning output, fail when a stream reports usage with no
  generation, keep backend token limits across model changes, and refuse
  overflowing token counts. A `Retry-After` duration that would overflow
  uses the maximum backoff.
- The empty dashboard reports an ingest failure. Engine state rows remain
  when details overflow the panel. Probe feedback stays visible on a narrow
  pane. `--once` frames do not depend on wall clock.
- Ingest audit logs record body-read errors, name write-failure causes, and
  keep ingestion progress in panic logs. `toktop update` error text omits
  the home directory.
- The toktop.ai worker honors `Accept-Encoding` and does not double-encode
  precompressed pages.

## [0.9.0] - 2026-09-13

Binaries, checksums, and a CycloneDX SBOM are on
[GitHub Releases](https://github.com/maci0/toktop/releases/tag/v0.9.0).

### Breaking

- Ingest `ts` now requires an RFC 3339 string with an offset. Values accepted
  in 0.8.0 without a zone, with colon-less offsets or SQL-style spaces, or as
  Unix epoch numbers (including numeric strings) now return HTTP 400 and stop
  processing the remaining events in that request. Update senders before
  upgrading: `"2026-01-02 03:04:05"` becomes `"2026-01-02T03:04:05Z"` to
  preserve the previous UTC interpretation; `"2026-01-02T03:04:05+0200"`
  becomes `"2026-01-02T03:04:05+02:00"`. Convert epoch values to an RFC 3339
  string representing the same instant. Omitting `ts` still uses arrival
  time when the original event time is not needed.
- Ingest removed the schema hint at `GET /v1/events`; it and
  `HEAD /v1/events` now return HTTP 405 instead of 200. Read the event fields
  in [README.md](README.md#agent-feed-api) instead. Switch liveness probes to
  `GET /healthz`, which answers `ok`; event submission remains
  `POST /v1/events`.

## [0.8.0] - 2026-09-02

### Added

- Startup prints one stderr line of the knobs that apply (`interval`,
  `ingest`, mode flags). Bearer tokens appear only as `bearer=set`.
- `$TOKTOP_LOG_LEVEL` sets the ingest audit log floor (`debug`, `info`,
  `warn`, `error`; default `info`). A set-but-invalid value aborts at
  startup.
- Ingest `POST /v1/events` honors `Idempotency-Key`: when an event omits
  `id`, the key plus the line's index is used, so a retried POST of the same
  stream is ignored instead of double-counted.
- `agentusage.InputRate` reports billed prompt tokens per second between two
  samples, matching `Rate` for output.
- `agentusage.Process.Watch` starts a watcher from a discovered process so the
  agent name and working directory cannot be swapped.
- Ingest logs 404/405 and handler panics on the same structured stderr line
  as POST `/v1/events` (including `method` and `path`). A 202 whose body
  cannot be written is a warning, not a success.
- OpenCode session directories on macOS match case-insensitively, the same
  way transcript adapters already do on default APFS.
- `toktop ssh://host` on Windows uses the OpenSSH named pipe agent when
  `$SSH_AUTH_SOCK` is unset, matching ssh.exe.
- `toktop help version` prints the same usage as `toktop --help`.

### Changed

- `--once` help names piping, `--ingest` help names `host:port`, and the
  usage footer says the live dashboard needs a terminal.
- Ingest `GET /v1/events` names NDJSON in the schema hint, matching what
  `POST /v1/events` accepts. Unknown paths answer 404 naming the three
  endpoints instead of Go's generic 404 page.
- The header session duration says `session`, matching `--once --plain`,
  instead of `up` next to the engine count.
- ENGINE STATE shows the context window as a token count (`ctx 8.2k tok`)
  rather than a byte estimate with a `tok` suffix.
- The compact dashboard's empty hint lists `--demo`, `--add URL`, or
  `--agents` as alternatives, not one command with every flag.
- Help on a pane too small for the full dashboard lists only the keys that
  work there (`q`, space, `?`).
- The agent event ring keeps 512 events so several agents over the 30s rate
  window are not evicted by a shared 64-slot cap. Duplicate `id` values are
  ignored only while that event is still in the ring.
- dsh zstd session reads cap newly-appended compressed bytes per poll so a
  huge append cannot pin an unbounded buffer; leftover frames count on the
  next poll.

### Fixed

- A LiteLLM, GPUStack, TRT-LLM, OmniRoute or LM Studio backend that answers
  neither `/metrics` nor `/v1/models` is reported as down, not as an idle
  success. Lemonade and LM Studio still stay up when their native health or
  v0 feed answers.
- `--interval` below 50ms or above 1h is rejected at startup. A bare
  `--interval 1` is 1 nanosecond in Go and would have hammered engines.
- `TOKTOP_COLUMNS` / `TOKTOP_LINES` reject values outside 41-1024 / 21-512
  so a typo cannot size a `--once` frame large enough to OOM.
- `toktop update --repo ''` (or `--repo=`) is a usage error instead of
  installing from the default repository.
- `toktop help update extra` and `toktop help version extra` are usage
  errors, matching `toktop version extra`.
- `scripts/screenshot.py` rejects extra arguments and unknown options
  (exit 2) instead of treating them as file names; error lines start with
  `screenshot.py:`.
- OpenCode token totals stored as JSON strings are compared as numbers when
  taking the largest context size, so `"9"` does not beat `100`.
- Agent SQLite stores are opened with `trusted_schema` off and double-quoted
  string literals disabled, so a planted schema cannot run extra SQL during
  a read.
- Generation probes cap thinking models (`think: false`), send
  `max_completion_tokens` and `n: 1`, back off 15s–5m on HTTP 429/503, skip
  embedding/rerank ids, and parse non-stream JSON replies, so a probe cannot
  run away on a billed gateway or an engine that ignores `stream`.
- The compact dashboard clips long engine rows to the pane width and keeps
  the key hint on the last line, so a crowded or narrow strip no longer
  wraps or hides `q` / space / `?`.
- Help on a small pane is clipped to the pane instead of overflowing.
- Empty PROBES copy says `q quit, then --probe N`, matching the setup card.
- Host-strip NPU names pass the same terminal sanitizer as CPU and driver
  strings.
- Ingest `ErrorLog` lines redact the peer address the same way the POST
  audit line does: loopback keeps the port, any other IP is dropped.
- `agentusage` does not walk `prompt`, `messages`, `choices`, `system`, or
  `text` trees for counters or a working directory; those keys hold user
  or model text, not usage.
- `--agents` refuses a crush `.crush/crush.db` (or `.crush` directory) that
  is a symlink pointing outside the project, matching the JSONL transcript
  rule, so a writable store cannot pull in another project's sessions.
- Ingest `POST /v1/events` treats mixed Latin+Cyrillic/Greek agent names as
  anonymous, so a lookalike cannot sit next to a real agent in the feed.
- SSH host-key change messages fingerprint RSA and ECDSA keys the same way
  as ed25519, instead of hashing the raw stored line.
- SSH keyboard-interactive auth answers only a single non-echoing prompt,
  so a server cannot harvest the password by asking extra questions.
- SSH connections use the library's supported algorithms, so ssh-rsa
  (SHA-1) and DSA host keys are not accepted.
- Ingest responses include `Cross-Origin-Resource-Policy: same-origin`.
- `ssh://user:pass@host` is rejected at startup (use `$TOKTOP_SSH_PASSWORD`
  or `--ssh-key`); a path, query, or fragment on the URL is rejected rather
  than ignored. The parse error does not echo the password.
- `--probe` above 86400 seconds is rejected so the auto-probe ticker cannot
  overflow. `--frames` above 180 with `--once` is rejected (chart history
  length).
- `$TOKTOP_BEARER` / `$OMNIROUTE_API_KEY` without `--add`,
  `$TOKTOP_SSH_PASSWORD` without an `ssh://` target, `--bearer` without
  `--add`, and `$TOKTOP_LOG_LEVEL` with `--no-ingest` are named as unused.
  `$TOKTOP_SCREENSHOT_FONT` is no longer reported as an unknown variable.
- `agentusage` resumes after a JSONL record larger than 8MiB instead of
  dropping every later record in that transcript.
- Ingest sanitization strips Unicode tag characters and other format
  characters (except ZWJ), so they cannot hide inside agent names.
- `agentusage` composes agent names to NFC, so a definition of `"café"`
  spelled with a combining accent is the same agent as the precomposed form.
- Ingest `ts` accepts a Unix epoch number (seconds, milliseconds,
  microseconds, or nanoseconds by magnitude), so a Python `time.time()` or
  JS `Date.now()` does not 400 the rest of an NDJSON stream. Small integers
  such as `123` stay type errors.
- Linux `--agents` process start time comes from `/proc/PID/stat` starttime
  plus boot time, not the `/proc/PID` directory mtime, so an NTP step or a
  recycled proc inode cannot look like PID reuse and drop the attach baseline.
- The live dashboard scores agent rates against the snapshot stamp, matching
  `--once` / `--plain`, so a demo or injected clock does not empty the 30s
  window.
- `agentusage` counts a thinking-only reading (opencode SQLite, and Claude /
  Qwen / Codex transcript lines that carry reasoning with no billed output)
  instead of reporting nothing until the first completion token.
- `toktop help` and `toktop version --help` list the same flags as
  `toktop --help`.
- `toktop --help update` matches `toktop help update`; extra arguments to
  `--version` are a usage error, like `toktop version`.
- `--agents` readings carry a stable event id, so a retried report of the
  same sample is not added twice.
- `LoadDefinitions` skips specs with only blank roots, matching `RegisterSpec`,
  so `Supported` is not true for an agent `Watch` would reject.
- `LoadDefinitions` names the file in malformed-JSON and unreadable-file errors.
- Agent names passed to `RegisterSpec`, `LoadDefinitions`, `Watch`, and
  `Supported` are trimmed, so a definition of `"claude "` is the same agent
  Discover reports.
- `agentusage` compiles on GOOS values other than linux, darwin, and windows:
  `Discover` reports nothing there, matching `Peers`.
- `POST /v1/events` rejects a non-object root (`null`, a bool, a number) with
  400 instead of recording it as an empty anonymous turn.
- `--demo --agents` stamps transcript-derived events on the simulated clock
  so rates stay on the seeded timeline.
- A probe with no model id reports `no model` instead of POSTing an empty id
  (some engines treat that as load-default).
- LM Studio listings omit unloaded catalog entries, so a probe cannot
  JIT-load a cold model.
- `--agents` on macOS no longer hangs for the rest of the run if `ps` or
  `lsof` leaves a grandchild holding stdout past the kill deadline.
- Closing the ingest endpoint releases its listen socket even if `Serve`
  has not started, so a failed startup cannot leave the port bound.
- SSH port forwards time out a hung remote Dial instead of pinning a
  goroutine and the accepted connection until the ssh session itself dies.
- Linux CPU model retries an empty first read instead of staying blank, and
  uses ARM Hardware/Processor fields and the device-tree model when
  `/proc/cpuinfo` has no `model name`.

## [0.7.0] - 2026-08-30

Binaries, checksums, and a CycloneDX SBOM are on
[GitHub Releases](https://github.com/maci0/toktop/releases/tag/v0.7.0).

### Added

- `agentusage` reads dsh's default session log (`session.jsonl.zstd`):
  concatenated independent Zstandard frames, with the provider's
  `inputTokens` / `outputTokens` / `reasoningTokens` on each completed
  `assistant/message`. Uncompressed `.jsonl` still works. The streaming
  usage chunk is not counted; it repeats the message.

## [0.6.1] - 2026-08-28

Binaries, checksums, and a CycloneDX SBOM are on
[GitHub Releases](https://github.com/maci0/toktop/releases/tag/v0.6.1).

### Changed

- Source builds and `go install` require Go 1.27, the version in `go.mod`.
- `--demo` uses `math/rand/v2`. The same `--seed` no longer matches a 1.26 frame.

### Fixed

- `--agents` refuses a symlink that points outside a transcript root, so a
  writable store cannot pull in another project's session as usage.

## [0.6.0] - 2026-08-27

Binaries, checksums, and a CycloneDX SBOM are on
[GitHub Releases](https://github.com/maci0/toktop/releases/tag/v0.6.0).

### Added

- Agent rows whose traffic is already counted by a watched engine are labelled
  `via <engine>` and are not added again to header or chart totals.
- Ingest `POST /v1/events` accepts optional `id` (a repeat of a key still in
  the retained feed is ignored, so retries are safe), `thinking_tokens`, and
  `via_engine` (the same attribution, so a harness POST is not added on top
  of a watched engine's totals).
- Ingest token counts accept whole JSON numbers (`100.0`, `1e2`), matching
  what Python `json.dumps` of a float emits.
- `agentusage.MatchingEndpoints` attributes many agent processes to engines
  in one connection-table pass.
- windows/arm64 release binaries, matching the other ARM64 targets.
- `agentusage.ErrEmptyTool`, `ErrNoRoots`, and `ErrInvalidDefinitions` so
  `RegisterSpec` and `LoadDefinitions` failures are matchable with `errors.Is`.

### Changed

- `agentusage.Sample` now has `Input` (billed prompt tokens since attach).
  Prompt rates use that field, not max-context minus summed output.
- Header and chart totals skip `via_engine` events individually, so an agent
  that switches onto a watched engine still contributes tokens spent before.
- Dashboard colors match toktop.ai (cool dark, green accent, amber pressure).
  The wordmark is a single accent color.
- The engine list panel is titled ENGINES, matching the header, site, and
  `--once --plain` report.
- Per-agent token counts use ▲ for output and ▼ for prompt, the same
  directions as the header rates.

### Fixed

- Empty dashboard names how to quit and re-run, shows when it is paused, and
  does not suggest `--agents` when that watch is already on.
- Failed probes show the error instead of a green zero rate.
- Engine rows label the KV-cache bar and run/wait queues instead of a bare
  bar and `r/w`.
- Help describes `p` as a real generation, not a synthetic one; the empty
  PROBES panel says `--probe N` needs a quit and re-run.
- Agents-only AGENT FEED shows ingest-down and the POST target, matching
  the full dashboard.
- `--add` rejects non-http(s) URLs, values with no host, and URLs that embed
  userinfo (use `--bearer` / `$TOKTOP_BEARER`). `--ingest` rejects an empty
  or non-`host:port` listen address instead of binding every interface on an
  ephemeral port. `--bearer` that is explicitly empty no longer falls through
  to `$OMNIROUTE_API_KEY` / `$TOKTOP_BEARER`. `--ssh-key` expands `~` and
  fails at startup if the file is missing.
- Agent session directories on Windows (and default APFS) match the
  filesystem's case and separator rules, so transcripts are not dropped
  when an agent recorded `C:/Users/Foo` and toktop resolved `c:\users\foo`.
- `toktop ssh://host` on Windows uses `%USERNAME%` when `$USER` is unset, and
  strips a `DOMAIN\` prefix from the account name.
- Crush session stores fill `Sample.Input` from `prompt_tokens`, matching the
  other adapters.
- `Sample.Empty` is false when only thinking tokens were observed.
- Ingest type errors for `POST /v1/events` name the JSON field (for example
  `prompt_tokens must be an integer`) instead of the internal Go type.
- `go install` binaries report the installed module version from `--version`
  and `toktop update`, instead of impersonating 0.1.0.
- `--agents` no longer re-walks every transcript store on each tick, and
  engine attribution reads the kernel connection tables once per pass.
- Crush session databases are summed once per attach; a continued session
  contributes only growth after attach. A store that cannot be read at
  attach is retried rather than treated as empty, so pre-attach tokens are
  not dumped into the review once it becomes readable.
- Transcripts that grew under one mtime stamp are still read.
- Probes hang up after the requested token budget if an engine keeps
  streaming, and ignore engine-reported usage figures far past that budget.
- `--agents` retargets a watcher when the kernel reuses a PID, so tokens
  are not attributed to the previous process.

## [0.5.0] - 2026-08-26

Binaries, checksums, and a CycloneDX SBOM are on
[GitHub Releases](https://github.com/maci0/toktop/releases/tag/v0.5.0).

[Unreleased]: https://github.com/maci0/toktop/compare/v0.12.0...HEAD
[0.12.0]: https://github.com/maci0/toktop/compare/v0.11.0...v0.12.0
[0.11.0]: https://github.com/maci0/toktop/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/maci0/toktop/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/maci0/toktop/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/maci0/toktop/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/maci0/toktop/compare/v0.6.1...v0.7.0
[0.6.1]: https://github.com/maci0/toktop/compare/v0.6.0...v0.6.1
[0.6.0]: https://github.com/maci0/toktop/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/maci0/toktop/compare/v0.4.5...v0.5.0
