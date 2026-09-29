# Architecture

How toktop's packages relate, and the rule that keeps them relating that way.

The rule is one line: **a package imports only packages in a strictly lower
tier.** `internal/repogate/deps_test.go` holds the tiers in a Go slice and
fails the build when an import points at or above its own tier, so a new
package is placed deliberately rather than inheriting a direction by
omission. This document is the map; the test is the enforcement.

## Tiers

| Tier | Packages | What lives here |
|------|----------|-----------------|
| 0 | `internal/core` | the data model every other package speaks in, plus the string helpers (sanitize, truncate, fold, redact, clamp) that have no dependency of their own |
| 1 | `internal/logcfg` | how the audit log is written: level from the environment, `$HOME` and address redaction, capped fields |
| 2 | `internal/bearer`, `internal/lockfile`, `internal/procs`, `internal/selfreload`, `agentusage` | one job each, no peers: the bearer token, the cross-process lock file, local engine discovery from process tables, executable watching, agent token usage |
| 3 | `internal/gpu`, `internal/probe`, `internal/provider`, `internal/demo`, `internal/selfupdate`, `internal/ui` | the work that has a shape specific to its subject: accelerator telemetry, streaming probe requests, engine scraping, the simulated fleet, release install, the dashboard |
| 4 | `internal/sysmon`, `internal/ingest`, `internal/agentwatch` | collection and event intake: host vitals, the localhost events endpoint, the bridge from `agentusage` watchers to the dashboard |
| 5 | `internal/remote`, `internal/collector` | the two fan-ins: the ssh client and host-relayed stats, and the poller every engine-side package reports into |
| 6 | `cmd/toktop` | the only package allowed to wire the rest together |
| 7 | `internal/repogate` | no shipped code: the tests over the repository's own metadata (the tier table above, the dependency table, the tool pins, the changelog, the CI action refs, the `--json` report's schema revision) |

`logcfg` sits below its consumers rather than beside them. `procs`, `gpu` and
`ingest` all reach for the redaction helpers, so a tier that held `logcfg`
alongside them would be a layer importing sideways into itself.

## Package map

- `agentusage` (top level, not `internal/`): the public Go package other
  programs embed. It reports tokens for AI coding agents read from the
  transcripts those agents already write. It is tier 2 and imports
  `internal/core` for the string helpers and the cadence types; a consumer
  outside the module never writes an `internal/` import path, since
  `agentusage` re-exports the core types it needs (`Pacer`, `Ticker`,
  `VirtualPacer`) as aliases under its own names. An alias is the only spelling
  that reaches the surface: `internal/repogate/public_test.go` fails the build
  when an exported declaration names an `internal/` type in a signature, a
  struct field or a variable's type, and holds the list of published packages
  to the one the Makefile's `PUBLIC_PKGS` names.
- `cmd/toktop`: flag parsing, validation and warnings (`flags.go`,
  `validate.go`), the startup config record and its two renderings (`config.go`),
  endpoint and target wiring (`attach.go`, `endpoints.go`), the
  subcommands (`help.go`, `update.go`, `version.go`, `completion.go`), and
  `main.go`, whose `runMain` is the whole startup sequence in one function.
- `internal/agentwatch`: the bridge from `agentusage` watchers to the
  dashboard: it follows the agent processes on this machine and records what
  their transcripts grew by. One concern per file: `agentwatch.go` the
  watcher's own state, injected clock and pacer, and the start and stop of a
  tracker, `discover.go` the pass that classifies this pass's processes
  against the trackers and attaches, follows and hands over stores,
  `engines.go` the monitored engines a tracker is matched against, and
  `report.go` the events read off a tracker.
- `internal/bearer`: one process-wide optional `Bearer` token for gateways
  that require an API key.
- `internal/collector`: polls providers on an interval, derives rates, and is
  the `core.AgentRecorder` that posted events land on. One concern per file:
  `collector.go` is the poll loop and the snapshot it builds, `host.go` the
  vitals and process-table pollers, `rates.go` the counter baselines and
  history rings, `health.go` one engine's entry and its outage latches,
  `agents.go` the agent event feed, `probe.go` the probe wave.
- `internal/core`: `Snapshot` and everything in it, plus generic sorted-ring
  helpers (`AppendSorted`, `AppendRetained`) and the `Tick`
  cadence every poller uses.
- `internal/demo`: a seeded simulated fleet, so `--demo` and the tests have
  something to render.
- `internal/gpu`: accelerator telemetry across vendors. One concern per file:
  `gpu.go` the per-tick sampling fan-out, `run.go` the vendor-CLI execution and
  the caches around it (the resolved path, the injected clock, the outage
  latch), `parse.go` the decoders for what those tools printed.
- `internal/ingest`: a tiny localhost HTTP endpoint (`POST /v1/events`) that
  harnesses and agents post usage into, plus the `GET`/`HEAD` `/healthz`
  liveness probe. One concern per file: `server.go` is the listener, its
  timeouts and the event clock, `endpoints.go` the route table behind the
  404 and 405 answers, `middleware.go` the chain every request passes
  (security headers, request id, panic recovery, the audit line), `post.go`
  the POST handler and the bounds on its body, `stream.go` the decode loop,
  and `health.go` the probe. `event.go` is the wire format those decode into
  `core.AgentEvent`.
- `internal/logcfg`: the audit-log vocabulary every other package that logs
  builds its logger from.
- `internal/lockfile`: the exclusive-create lock a process takes before it
  rewrites a file another process may be reading, with the stale break and the
  wait it applies. The wait runs on the `Policy` clock and sleep, which a
  driver replaces with a virtual pair so the polls, the break and the give-up
  land on steps it took. `internal/remote` takes it over the host-key pin store
  and `internal/selfupdate` over the installed binary.
- `internal/probe`: small streaming generations at backends, to measure
  throughput rather than read a counter. One concern per file: `probe.go` the
  request, the budgets that bound a generation and the HTTP handling both
  dialects share, `ollama.go` the Ollama dialect, `openai.go` the
  OpenAI-compatible one, `model.go` the choice of which model to ask.
- `internal/procs`: finds local inference engines by inspecting running
  processes, over procfs on linux and the OS tooling elsewhere.
- `internal/provider`: engine discovery and metric scraping for local
  OpenAI-compatible backends.
- `internal/repogate`: no shipped code. It holds the tests over the
  repository's own metadata: the tier table enforced above, the dependency
  table, the tool pins, the changelog, the CI action refs, and the `--json`
  report's `schema` revision, which `json_report_test.go` holds to the keys the
  report published at the last release. Those tests are
  about files rather than about a Go package, so they sit in their own package
  above every tier instead of inside `cmd/toktop`, where a reader looking for
  how a run starts would have found the supply-chain gate first.
- `internal/remote`: attaches to engines on other hosts over one ssh
  connection, with known-hosts checking and a relayed host-stats sampler. One
  concern per file: `client.go` is the connection and its keepalive, `session.go`
  the command sessions, `forward.go` the local listeners piping remote ports,
  `knownhosts.go` the host-key store, `target.go` the target spelling,
  `auth.go` the key, agent and password order with the platform half beside it,
  `restore_unix.go` and `restore_windows.go` the recovery command an operator
  runs by hand, and `discover.go` and `stats.go` the two samplers.
- `internal/selfreload`: watches the running executable for a rebuild and
  signals the process to restart.
- `internal/selfupdate`: replaces the running binary with a newer release. One
  concern per file: `release.go` is the release API and everything read off the
  network (repo validation, the GitHub-host URL and redirect rules, the release
  lookup, the download), `checksum.go` the verification of what was fetched
  against the release's own `checksums.txt`, `install.go` the filesystem half
  (the staged download, the cross-process install lock, the rename that puts a
  verified binary in place and the recovery of a displaced one).
- `internal/sysmon`: host vitals (RAM, swap, load, temperatures, CPU) and
  GPU readings, which is why it sits above `gpu`.
- `internal/ui`: the bubbletea dashboard, and the plain and JSON reports that
  `--once` renders through the same `core.Snapshot`.

## Platform files

A package's platform split is by build tag, and the file name repeats the tag
so the split is readable without opening the file. `_darwin.go`, `_linux.go`
and `_windows.go` name one platform. A file with no platform suffix holds
what every build shares, and a test file with none is a test that runs
everywhere.

`_unix.go` is not one meaning, and the build tag is the authority in every
case. Five files use it for `!windows` (`cmd/toktop/pipe_unix.go`,
`internal/remote/auth_unix.go`, `internal/remote/restore_unix.go`,
`internal/selfreload/exec_unix.go`, `internal/selfreload/stat_unix.go`),
which is the opposite of the POSIX platforms. `internal/core/procattr_unix.go` uses it for the stricter `unix`
constraint Go provides, pairing with `procattr_other.go` on `!unix`. A file
whose tag names a subset of the POSIX platforms takes no suffix, because a
suffix would claim more than the tag delivers.

A file built for a tag that is not one platform carries the build-tag name
last and a subject before it: `internal/procs/procs_linux_root.go` is the
root-only half of the linux walk, `internal/procs/procs_tooling.go` the half
darwin and windows share, and `internal/sysmon/sysmon_utsfield.go` the
`linux || darwin` field reader. The same shape covers build tags that are not
platforms at all: the `sqlite` tag splits the two source hooks `agentusage`
decides an agent is readable through.
`agentusage/source_off.go` (`!sqlite`) stubs both of them, `builtinSource` and
`setOpenCodeDB`; `agentusage/crush_sqlite.go` (`sqlite`) is the first reading
crush's database; and `agentusage/opencode_sqlite.go` (`sqlite`) is the second,
the half that reads opencode's machine-wide store.
`agentusage/sqlite.go` carries the `sqlite` half of the runtime source registry
those two register into.

`internal/sysmon` is the one place that goes further: `sysmon_darwin_host.go`
and `sysmon_windows_host.go` pair a platform with its host-reading half,
because the darwin and windows samplers are built from different primitives
and are not variants of one another.

## How a run starts

`runMain()` in `cmd/toktop/main.go` is the whole sequence, in order, and reads
top to bottom (`main()` is the `os.Exit` around it, split out so the exit
skips the defers):

1. Ignore `SIGPIPE`, take a subcommand before flag parsing, parse and validate
   the flags, parse `--origin`, hand `logcfg.Logger()` to `agentusage`.
2. Parse the ssh targets, validate the `--ssh-key` flag, open opencode's
   database if it was asked for, warn about flags and environment variables
   this run will not read, then resolve `--ssh-key` to a real path.
3. Pick a data source: `demo.NewSource` under `--demo`, otherwise
   `attachEngines` followed by `collector.New`. Either way a `chan
   core.Snapshot` and a `core.AgentRecorder` come out, and every later step is
   the same in both modes.
4. Start the agent watcher, then the ingest endpoint, both guarded on the
   recorder existing.
5. Hand a `ui.Config` to either `runOnce` or `runTUI`.

The two modes differ only at step 3. That is the payoff of the tier rule:
`demo` and `collector` are interchangeable because neither knows about the
other, and neither knows that a UI exists.

## Where new code goes

- New behaviour on an existing subject goes in that subject's package, beside
  the code it sits next to. A change that makes you look for a second home is
  a change that wants a new package.
- A new package needs a tier in `internal/repogate/deps_test.go` and a row in the
  table above. It imports only lower tiers; if it cannot be placed, the
  boundary it straddles is not where the code is.
- A package that does one job and has no peers belongs in tier 2. Promoted out
  of it only when it grows a second concern.
- Nothing in `internal/` may be imported from outside the module, and only
  `agentusage` is a published surface. If code reaches for an `internal`
  package from outside, the shared part belongs in `agentusage` or in `core`,
  not in a widened `internal/`.

## Related

- [DEPENDENCIES.md](DEPENDENCIES.md): every external package and the reason it
  is here.
- [THREAT_MODEL.md](THREAT_MODEL.md): what toktop trusts, per package.
- [PRIVACY.md](PRIVACY.md): what it reads, sends and stores.
