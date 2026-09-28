# Changelog

Consumer-facing changes to the `toktop` CLI, the ingest `/v1/events` body, and
the `agentusage` Go package. GitHub Releases also attach auto-generated notes
from git; this file is the summary aimed at people upgrading.

The project is 0.x: those surfaces may change without a major bump. `toktop
update` always fetches the latest GitHub release; older tags are not a
support channel (see SECURITY.md).

This file starts at 0.5.0. Releases before that have no notes here; see
[0.1.0 to 0.4.5](#010-to-045) for what to read instead.

## [Unreleased]

### Added

- The THROUGHPUT and PROMPT panel titles name the peak their braille plot is
  scaled against, and `--once --plain` prints a `THROUGHPUT` section with
  both peaks and the window they were measured over. The plots were the one
  thing on the frame that carried a measurement no screen reader could read,
  and the only account of the curve's shape was the drawing itself.

- The spellings a macOS watcher looks a working directory's sessions up under
  now include its case folds, the way the Windows ones already did. A volume
  storing `Users` is reached by an agent started from `users`, and the module
  already called the two spellings one directory: `SameDir` matched them and
  `DirKey` gave them one map entry, while a lookup still tried only the
  normalization forms, so a session recorded under the other spelling went
  uncounted. The added forms are the same fold `DirKey` applies, so every one
  of them is a spelling the volume resolves to the directory looked up.

- The key reference scrolls with the arrow keys, page keys or ctrl+n/ctrl+p
  on a pane too short for the whole list, and says how many rows are off
  screen. The rows it could not show were the flags at the bottom, and no key
  reached them.

### Performance

- The generic transcript reader (copilot, cursor-agent, clanker) no longer
  decodes a record that carries no key it can use. The decode builds a map and
  a boxed value per key for the whole record, tool results and user text
  included, and the walk then dropped all of it: over a mixed six-record poll,
  178 allocations and 7,764 bytes became 104 and 4,527, with the decode skipped
  on the four records of that poll that carry nothing it can use. The keys that
  can match are a fixed set, so a scan of the raw bytes rules a record out
  before it is decoded, and a test fails the build if a key added to those
  tables holds none of the scanned spellings.

- A transcript store holding two extensions is walked once for both. The
  shared listing was keyed per suffix, so a dsh watcher (zstd and plain
  session logs in the same tree) traversed the store and stat'd every file in
  it twice per rescan for a list that is the union either way. The key is the
  whole suffix set now, and one walk fills it.

### Fixed

- `$SSH_AUTH_SOCK` is stripped of surrounding whitespace and a trailing newline
  before the ssh-agent is dialed, the rule `$TOKTOP_SSH_PASSWORD` and
  `$GITHUB_TOKEN` already followed. A wrapper that exported the socket name
  through `$(...)` left the line ending on it; the socket so named does not
  exist, the dial failed, and the run fell back to keys and then to a password
  prompt, which is the error naming nothing about the cause. A value that is
  only whitespace now resolves to the platform default rather than a path made
  of the whitespace.

- The startup line names a `TOKTOP_COLUMNS` / `TOKTOP_LINES` frame override in
  force, as `columns=` and `lines=`. The line is the record a run is
  reconstructed from, and a sized capture leaves it out: the render is a
  bitmap carrying nothing about the dimensions it was asked for, so a
  screenshot whose frame was overridden had no account of the size. The
  override is named only where the sized frame is what renders, since
  `--plain` and `--json` replace it.

- The web site no longer removes the focus outline from the skip link's
  target. `main` takes focus from that key press, so a keyboard user who
  activated the skip link had nothing to show where focus had landed.

- A `kbd` keycap on the web site draws its border in the page's foreground
  color rather than the divider token, which measured 1.3:1 against the
  background. The border is the only thing that identifies the keycap as a
  control, and it was under the 3:1 that boundary needs.

- The shell prompt in the screenshot caption on the web site is hidden from
  assistive technology, so a screen reader announces the command rather than
  a "dollar" in front of it.

- A second `collector.Run` on a live collector is refused instead of run
  alongside the first. Two loops on one collector start a second pair of
  background pollers, write a second snapshot per interval into the same
  channel, and fold their polls into the same rate baselines and health
  latches, so every frame was rendered twice and each rate was measured across
  the other loop's samples. A run started after the previous one returned is
  unaffected.

- The collector calls its injected clock with `clockMu` released, the way
  every other injected clock in the program is read. `drainWindowRefusals`
  reads it from under `c.mu`, and `SetNow` took the stamp under the same lock,
  so a clock that reached back for either one deadlocked the poll loop rather
  than producing a frame. Every goroutine the collector runs reads that clock,
  from the poll loop through the ingest handlers to the UI's probe wave, so
  holding the lock across the call also made one slow clock a process-wide
  stall.

- The endpoint list a `404` from the ingest server answers with reads
  `not found; endpoints: /v1/events (POST); /healthz (GET, HEAD)`. It ran the
  methods in front of each path and separated the entries with a comma, so
  `POST /v1/events, GET, HEAD /healthz` split two ways and named a third
  endpoint, `/events`, that is not served. A sender that reads the list has to
  be able to tell where one path's methods stop and the next path starts.

- The working-directory note a macOS watcher builds strips the home directory
  across Unicode normalization, the way the log redactor already does. A home
  macOS stored decomposed and a working directory an agent recorded composed
  are one directory and differ byte for byte, so the relation came back
  outside home, the `~` rewrite did not happen, and the note kept the whole
  absolute path, account name included.

- opencode's stored session directory is folded with `core.FoldCase`, the
  simple case fold `strings.EqualFold` compares by, rather than the full case
  folding `x/text/cases.Fold` performs. Full folding is a case mapping and
  over-folds: it renders `ß` as `ss` and `U+0130` as `i` plus a combining dot,
  so a stored `/work/straße` and a watched `/work/strasse`, two directories a
  case-insensitive volume keeps apart, matched, and one checkout's tokens were
  billed to the other.

- An ingest endpoint bound to loopback refuses a request whose `Host` names
  anything but the loopback interface, `localhost`, or a name under
  `.localhost`. The `Origin` check already refused a cross-site page, but DNS
  rebinding defeats it: a page that points its own name at `127.0.0.1` for one
  fetch is same-origin with the endpoint, so the browser sends no `Origin` at
  all and the request forged rows into the feed unchecked. The `Host` header
  is what such a request still carries, and it carries the attacker's name.
  The check runs ahead of routing, so a rebound request also cannot read the
  endpoint list a `404` names. An endpoint bound off loopback with `--ingest`
  applies no such rule: its peers reach it under whatever name they use for
  the machine.

- The directory key a macOS or Windows watcher maps recorded working
  directories by is folded with `core.FoldCase`, the case fold
  `strings.EqualFold` compares by, rather than `strings.ToLower`. The two
  spellings of a directory a case-insensitive volume looks up are equal
  exactly when `EqualFold` says so, and `ToLower` is not: it is a full case
  mapping, so U+0130 came back as a bare `i` and the two directories
  `Users/i` and `Users/İ`, which the volume keeps apart, shared one key. The
  session recorded under one was then watched as the other.

- A recorded working directory that has lost bytes names no directory that can
  be verified, and no longer matches one. `encoding/json` replaces every
  ill-formed byte and every unpaired surrogate escape with U+FFFD, and
  Gemini's `.project_root` is read as raw bytes, so a checkout under a name
  that is not valid UTF-8 reached the comparison with those bytes already
  collapsed. On Linux, where paths are compared exactly so that two
  differently-spelled names stay two projects, `/work/a\xff1` and
  `/work/a\xfe1` then compared equal and one checkout's tokens were billed to
  the other. A session whose directory is refused instead goes uncounted
  rather than misattributed.

- The two startup warnings that name externally supplied strings, the unknown
  usage keys in `~/.gauntlet/agents.json` and the unknown `TOKTOP_*`
  environment variables, sanitize them first. Both are text the program did
  not write, and an unknown-key warning is among the first things a run
  prints, so an escape sequence in either reached the operator's terminal
  from a definitions file or a wrapper script. The names are also capped at
  64 characters, cut between grapheme clusters so a trailing emoji sequence
  or a decomposed accent is never sliced in half.

- An agent the snapshot cannot date shows `time unknown` in the agents table
  and the plain report instead of a blank cell. A blank read as a missing
  value, which is not what "no instant to measure against" means.

- `--once --json` reports two fields the dashboard and `--plain` already show
  and it dropped. An agent event carries `span_ms`, how long the model spent on
  its tokens under the name and unit `POST /v1/events` takes, which is the
  denominator `agent_rates[].tok_per_s` prefers: a consumer had the rate and
  the token counts it came from with no way to check one against the other.
  `system.drivers` carries the host's accelerator driver versions, including
  the ones read from a remote target. An event with no span omits the field,
  which is the same meaning an absent `span_ms` has on the wire.

- `agentusage.Definitions` and `agentusage.Definition` model the agent
  definitions file (`agents.json`) as Go values, so a program that writes or
  edits one marshals the same shape `LoadDefinitions` reads instead of
  hand-building the `usage` wrapper. An entry carrying fields beside `usage`
  survives a round trip, since the same file also describes how to launch an
  agent and this package leaves those fields alone.

- The audit log names a run of agent events the retained feed window refused,
  once when it starts and once when an event lands again. A sender whose clock
  lags has every event sorted behind the window, and the only record was
  `stored` under `accepted` on its own POST, which is the answer a replay also
  gets: the agent list went empty with nothing on it saying why.

- A `POST /v1/events` carrying an `Idempotency-Key` audits `event_key`, the
  hashed prefix its derived event ids start with, so the request and the feed
  rows it minted can be matched on one field.

### Fixed

- A long `--agents` run held per-file bookkeeping for every transcript a
  per-file-owner agent's store had ever shown it, judged ours and then aged out
  of the recency window. Nothing released it: the verdict is kept so a session
  that comes back is read from where it stopped rather than from byte zero, and
  that keep had no cap, so a machine-wide watch grew by every session on the
  host. The verdicts are now bounded like the counted files, oldest first, and
  a transcript past the cap is read from its start if it is ever appended to
  again.

- A vendor GPU CLI that kept failing was tracked by the path it was run from,
  and lookup re-resolves that path after ten minutes, so a driver reinstall or a
  flipped symlink left a new entry per location the binary had ever had and made
  one tool read as a new outage each time. The outage latch is keyed by the tool
  name, and the audit line now carries the resolved path beside it.

- A transcript store that could not be walked (a partial mount, a root that is
  not a directory) left the shared listing unstamped, but the watcher reading it
  kept the same empty answer as its own listing for a rescan window and aged its
  per-file bookkeeping against it. A transcript it never reached lost its read
  position, so the next append to it was read from byte zero and the session
  counted twice. An incomplete walk now leaves the watcher's listing unstamped
  and its bookkeeping alone, and the next poll re-walks.

- A transcript the watcher could not attribute to a working directory was
  retried on every poll and reported nowhere, so the whole session's usage
  went uncounted with the agent simply absent from the dashboard. The failing
  file is now named once on the audit log, with the cause the read gave.

- A hot-reload watcher that could not stat the running executable said
  nothing and kept polling, so a `go build` landing in that window was never
  noticed and the session ran the old image. The audit log now reports the
  reload going off and coming back.

- A failed write to the host key store could leave its staging file, holding
  pinned host keys, behind with nothing but the write's own error; the
  leftover is now named with the path to delete. A stale store lock that could
  not be removed was reported as held by another toktop, which is not true;
  the message now carries the removal failure instead.

- A probe response body that stopped partway was quoted as the engine's own
  error text, which is indistinguishable from a short but complete one. The
  read failure is now wrapped into the error, as the provider path already
  did.

- A value the flag package could not parse ended in its bare `parse error`,
  which names neither the expectation nor a value that would work, and every
  numeric flag reported it identically. `--frames abc`, `--probe 1.5` and
  `--seed 1.5` now name the value each flag takes, and `--interval` keeps the
  note that a bare number there reads as nanoseconds.

- `--check` and `--repo` written at the top level were answered with a bare
  "flag provided but not defined", naming neither the subcommand they belong
  to nor the command line that would work, while a misplaced subcommand word
  got a hint. Both now answer with the `toktop update` line that would run.

- Two crush tests failed on a machine with a crush database in an ancestor of
  the system temp directory. `crushDBPath` walks up to the project root, so a
  `/tmp/.crush` left by another run is what the walk found and the assertions
  about a tree with no database read that file instead. Both now name the path
  they cannot see past and skip, rather than failing on a store they did not
  create.

- One auto-probe tick fired a generation against every backend at once. The
  wave gate spaced waves but never bounded their width, so a fleet wider than
  four turned `--probe 1` into a burst of concurrent requests a gateway bills
  per token. A wave now probes four backends and the next wave resumes where
  it stopped, so the rest of the fleet is still measured.

- A probe stream whose frames carried no content and no reasoning tripped
  neither the token budget nor the byte budget, and a line the frame parser
  skipped never counted toward anything. An SSE keepalive comment, a role-only
  opening frame, or a gateway replaying empty choices held the generation open,
  and billed, until the 30-second client timeout. Probes now hang up after
  `probeFrameMax` frames, counted ahead of the parser so a skipped line still
  counts.

- A probe the client hung up on reported itself as the engine dying
  mid-generation, because the transport reports a deliberate close as a
  truncation. A tokenless stream bounded on frame count is now the "empty
  stream" it is, not a broken engine.

- A probe that kept answering wrote nothing to the audit log. The transition
  lines say whether an engine is answering, not what it answered like, so a
  throughput regression the next wave recovered from left no record at all. At
  `TOKTOP_LOG_LEVEL=debug` every probe now logs its model, round trip, time to
  first token, tokens per second, and token count; the default floor is
  unchanged, and the transition latch still records one line per outage rather
  than one per tick.

- A probe of a thinking model that answered in one piece reported `empty
  stream`. Engines that ignore `stream: true` return the whole completion in a
  single body, where the trace lands in `message.reasoning_content` and
  `content` can be empty; the count read `content` alone, so a working engine
  read as broken and latched a `probe failed` audit line. The whole-body parse
  counts the reasoning fields the streaming parse already counted.

- A `$HOME` that is not an absolute path, or is unset, put every built-in
  agent store under the directory the run started in: `filepath.Join` drops
  the missing first element, so `~/.claude/projects` became
  `.claude/projects`, where a missing store is an empty one and every agent
  reports no tokens. The XDG, `KIMI_CODE_HOME` and `GAUNTLET_HOME` variables
  are already held to the rule that an unusable value names no path; the home
  every one of them falls back to now is held to it too, and `--agents` names
  it when `GAUNTLET_HOME` placed the definitions file and the run continued.
  The same rule now covers the ssh host-key store's `os.UserConfigDir`
  fallback, which a relative home resolved under the working directory.

- A `usage` key in `~/.gauntlet/agents.json` that this build has no field for
  was ignored in silence, so a misspelled `roots` left the agent with nothing
  to read and it reported no tokens for the whole run, which is what an agent
  nobody defined also looks like. `agentusage.UnknownUsageKeys` reports them and
  `toktop --agents` names them at startup, with the keys it does read. The load
  still succeeds: the file is gauntlet's, and a newer gauntlet can name a key
  this build does not read yet.

- `agentusage.LoadDefinitions` returned a bare `os` error for a definitions
  file that exists but cannot be read, so a program checking
  `errors.Is(err, agentusage.ErrInvalidDefinitions)` for "this file is
  unusable" saw that answer for malformed JSON and no answer for a permission
  failure, a directory, or a path below a regular file. Every refusal of a file
  that exists now carries that error, with the `os` failure joined, so
  `errors.Is` and `errors.As` still reach the cause underneath.

- `HEAD /healthz` answered with a different header set than the `GET` it
  stands in for: no `Content-Length`, because the answer left the length to
  the runtime and `net/http` derives it from the body a `HEAD` never sends.
  A probe that asked for the `HEAD` could not tell a healthy `ok\n` from an
  empty body. The healthy and the degraded answer now state their length, as
  every other answer on that endpoint's surface already did.

- The `404` an unknown path answers names each served endpoint with the
  methods it actually takes, so it reads `GET, HEAD /healthz` where it read
  `GET /healthz`. The two error bodies are built from the same endpoint table
  and now advertise the same method list a `405` puts in its `Allow` header,
  so a sender correcting a typo from the `404` is not told a method the path
  refuses.

- The ENGINES and ENGINE STATE panels named the engines they had no rows for
  (`+3 more`) without saying how to reach them, which reads as three engines
  the tool cannot see rather than three the pane was too short to draw. The
  count now carries `(enlarge window)`, the way the compact strip's overflow
  line already did. A column too narrow for the sentence keeps the bare count.

- The compact strip prints the last probe's outcome, but neither its key line
  nor its help listed `p`, the key that produces it. A pane too small for the
  dashboard hid a control that works there.

- `esc` quits the dashboard from every view but the agents one, and the footer
  advertises `q` alone, so the quit was documented nowhere in the app. The
  in-app key reference now says what `esc` does from the dashboard as well as
  from the help overlay.

- A remote error naming a *different* account that starts with the watched
  user's name no longer loses the drive letter on a Windows path. The
  `C:\Users\me-too\...` a peer reports about itself came back as
  `\Users\me-too\...`, a drive-relative path the peer never mentioned, in the
  dashboard, the `--json` report and the audit log. A home that is folded
  keeps its `C:`.

- `toktop --demo --ingest` deduplicates agent events through a bounded
  15-minute id ledger, the way the live collector does. It answered a replay
  from the retained feed, which holds a couple of minutes of generated
  events: a POST carrying an `Idempotency-Key` and retried after its first
  copy had rolled out of the window was counted a second time. The ledger
  ages out and is capped, so a replay inside the horizon stores nothing and
  the ledger cannot grow without bound.

- The startup config line and the audit record named a bearer token as
  `bearer=set` when it was not. The line is written before `bearer.Set` runs,
  and a token carrying a line break is turned down there, leaving the `--add`
  endpoints queried unauthenticated: every poll answered 401 while both
  records claimed a credential was in force. A refused token is now named
  `bearer=refused`, and the cleartext `--add` warning is reserved for a token
  that is actually sent, so it keeps meaning what it says.

- `$GITHUB_TOKEN` set to a blank value is named at the start of
  `toktop update`. The anonymous rate-limit error it produced advised setting
  the very variable the operator had already set, and read as a first run that
  had never authenticated.

- A transcript whose zstd frame the reader rejected was audited as read
  every poll, and the latch that keeps a failing path out of the read loop was
  cleared each time. The agent that owned it read as one being polled with
  nothing to show, and the same frame was decoded again on every tick. A
  rejected decode now carries the same error the first-baseline read reports
  it under, so a path that never advances is latched and named once.

- The two copies written beside the host-key pin store are read freshest
  first, not in a fixed order. A store lost or emptied is recovered from a
  copy, and a fixed order recovered it from whichever copy predated the
  other, so every host pinned after that write was dropped and the next
  connection to it was refused as unpinned. mtime picks the copy instead of an
  assumption about which one a write leaves behind.

- A directory name carrying the note separator is no longer read as a
  breakdown the note never wrote. The working directory a process reports is
  whatever its checkout was named, and a checkout called `proj · counted by
  engine ollama` attributed the agent's output to an engine that never saw
  it, in the dashboard, the `--json` report and the audit log. The separator
  character itself becomes a hyphen, so every separator left in a note is one
  the note wrote.

- A remote error naming a home under the peer's own account is folded out of
  the diagnostics toktop prints, not only out of the audit lines it kept to
  `HOST` and port. The message reached the dashboard, the `--json` report and
  the reload path with the account's home in it, and those are the surfaces
  that get pasted into a ticket.

- A UTC audit stamp no longer sorts before the whole second it belongs to.
  RFC3339Nano drops trailing zeros and drops the point entirely on a whole
  second, so `12:00:00.9Z` sorted ahead of `12:00:00Z` and a whole-second line
  sorted to the end of its own second, which is the one order a UTC stamp is
  chosen for. Stamps carry a fixed nine-digit fraction, so lines from
  several machines interleave the way they are read.

- An agent's clock offset is no longer forgotten when a superseded row ages
  out of the ledger. Agent, offset and instant identify one reading, so
  dropping a row that was read again at the same value took the offset with
  it and the agent's events went back onto the sender's own timeline. A row
  still in force survives the ageing of the one it replaced.

- An interval computed from a clock that stepped backwards is read as the
  zero it is rather than as a negative duration. The per-process sampler and
  the provider health panel divided by a raw subtraction, so a backward step
  charged a frame's tokens against an interval of the wrong sign.

- The THROUGHPUT and PROMPT plots read one timescale. They are drawn stacked
  with no axis on either, and the prompt plot read the uniform cadence while
  the one above it read the mode `t` selects, so a column of the lower plot
  could span a different span of wall clock than the column above it and a
  reader comparing the two traces had nothing on the frame to say the mode had
  split them. Both now take the mode and the block boundaries from the same
  series, and the prompt plot draws the grid lines the throughput one does, so
  a shared column is visibly a shared column.

- The SYS strip says what it left out. A row that ran out of cells ended
  mid-list, so on a host with several accelerators the vitals row stopped after
  the first GPU and said nothing, which reads as one GPU on the machine, and a
  reader could not tell a row that had to drop a reading from a row that fitted
  one. Both rows now end with the number of readings they shed, and the
  identity row counts the temperatures its cap never turned into segments
  together with the ones the pack dropped, so one `+N more` accounts for the
  whole row rather than two counts a reader cannot tell apart.

- The empty agent feed carries one sentence. The dashboard panel and
  `--once --plain` each spelled their own version of the same advice, and the
  two had drifted apart in wording and punctuation. They share the sentence
  now, and differ only in where they point: the dashboard names the endpoint in
  its panel title, the linear report spells the address out.

- The panic audit line folds the home directory out of both the recovered
  value and the stack. A panic is the one payload in the ingest path that
  reached the line untrimmed, and it is the one this process does not author:
  the handlers decode event fields off the wire, so a panic raised while
  holding one carries whatever the sender wrote, and a stack frame names the
  file it unwound through. Every other sink in the package already clipped and
  folded its text, because the audit stream outlives the run and gets pasted
  into issues.

## [0.19.0] - 2026-09-28

Binaries, checksums, and a CycloneDX SBOM are on
[GitHub Releases](https://github.com/maci0/toktop/releases/tag/v0.19.0).

### Added

- `agentusage.DefaultSuffix` is the transcript extension a `Spec` that names
  none matches. A program defining an agent in Go had to write `.jsonl` out by
  hand, where `DefaultPollInterval` already carries its default.

- `POST /v1/events` takes a `span_ms` field: how long the model spent on that
  event's tokens, in milliseconds, capped at 86400000. It is the rate
  denominator, so a sender that reports it beats the gap between events, which
  is what every sender had to fall back to. Absent, `0` and out of bound all
  mean the same thing: the gap between events.

### Breaking

- `agentusage.Sample` and `agentusage.Delta` gained a `Span time.Duration`
  field: how long the model spent on the tokens in the sample, when the
  transcript records it. Zero means the transcript said nothing, and the rate
  comes from the gap between samples, as it did for every sample before. A Go
  caller that builds either as an unkeyed struct literal has to name the new
  field; a caller that reads fields by name is unaffected.

### Changed

- `POST /v1/events` clamps a `ts` that sits more than two minutes behind
  arrival to the arrival instant, not only one that sits that far ahead. The
  bound ran in one direction, so a lagging sender's stamp was stored as sent.
  A stamp behind arrival sorts to the front of the retained feed, so a sender
  whose clock runs hours behind (a host that has not been on the network since
  boot, a restored VM snapshot) filled the retained window with its own events
  and then had every later event refused as outside it: the `202` reported
  `stored` below `accepted`, and the resend it asked for was refused again on
  every attempt. Such a sender now has its stamps read as arrival.

- The audit log names an `ssh://` target by host and port, without the
  account. A login names a person on the host, no redaction folds one out of
  a line, and these lines outlive the run into whatever ran toktop. The
  account still appears in the message toktop prints to your own terminal.

### Fixed

- A transcript whose read failed once, and which then aged out of the watcher's
  recency window, no longer keeps its path for the rest of the run. The
  failure latch was only cleared by a read that committed, and a file that
  never read again is in none of the maps the sweep walks, so a long session
  accumulated one entry per transcript that hit a permissions or I/O error.

- A session database that exists and will not read is named in the audit log
  once per outage instead of once per poll. Stores are read several times a
  second, so a corrupt page or a database under continuous write wrote a line
  every poll for as long as the dashboard ran. A store that reads again clears
  the latch, and its next failure is reported as the new thing it is.

- The signal context and the `--ingest` listener are released on a clean exit.
  `main` exited through `os.Exit`, which runs no deferred calls, so the
  deferred `srv.Close` was never called and both were reclaimed by kernel
  teardown alone.

- A `span_ms` past a day on `POST /v1/events` clamps to `0`, as the field's
  documented bound says. The count was converted to nanoseconds before the
  bound was applied, and a large enough count wrapped int64 onto a small
  positive duration (18446744073710 ms landed on 448µs) that then passed the
  clamp, so the event's tokens were divided by half a millisecond and the
  agent's rate read billions of times too high. A token count too large for a
  float64 (`1e400`) now says it is out of range, like a count past int64
  already did, rather than claiming it is not an integer.

- A probe against an engine that answers with a whole JSON body instead of an
  SSE stream no longer reports tens of millions of tokens per second. A
  non-stream body has no first-token instant, but the reader sampled the time
  as if it did, leaving `total - ttft` as the gap between two clock reads taken
  nanoseconds apart around the same call. About half the time that gap came out
  positive, and the rate was the token count over tens of nanoseconds. A rate
  is now measured over the whole exchange, which is what a body delivered in
  one piece allows.

- A per-message transcript that folds several records into one carries the
  span of all of them, not the first record's. The tokens of every folded
  record were summed while the span stayed at the first, so a read that saw
  two turns divided both turns' tokens by one turn's time and read high by
  roughly the number of turns folded.

- An accumulated model-time span saturates instead of wrapping, matching the
  rule every token counter already followed. A single grok record may carry a
  span near the whole int64 nanosecond range, so summing two of them on one
  transcript wrapped negative and the sample reported a negative span for work
  it had counted. The ceiling is the 24h the event boundary already enforces.

- A bearer variable that is set but blank no longer authenticates nothing
  while the startup line claims `bearer=set`. `export TOKTOP_BEARER=$(cat key)`
  over a missing file sets it to the empty string, and a token read into a
  variable can keep a trailing newline or space; either won the precedence
  chain ahead of a variable that did hold a token, and the `--add` endpoints
  answered 401 from then on with nothing saying why. Values are trimmed, a
  blank one is skipped in favor of the next source, and a variable set to
  nothing is named at startup with `--add` in play.

- A `KIMI_CODE_HOME` naming a directory with no `sessions` under it is named at
  startup, the way a `GAUNTLET_HOME` with no `agents.json` already was. kimi
  creates that directory itself on first run, so a home it does not use left
  every kimi session reading as an agent producing no tokens.

- `--plain --json=false` no longer warns that the JSON report replaces the
  text report. The warning read the flag as given rather than its value, so a
  run that printed the text report was told the opposite.

- A CUDA version in `nvidia-smi` output with no driver version beside it is
  reported. The parse dropped the whole file when the driver line was absent,
  which is the coupling its own comment says it avoids.

- A zero-byte `temp*_label` in a hwmon chip directory no longer blanks the
  chip name. The label file exists to name a sensor that has no better name,
  and an empty one left the gpu/junction needles with nothing to match.

- `ps` rows naming pid 0 on macOS are no longer treated as processes, the
  same guard the `/proc` walk applies.

- The ssh host-key store's backup copy is renamed into place the way the store
  is. It used a plain rename, which on Windows refuses to overwrite, so every
  write after the first left the copy at its first contents while the store
  moved on.

- An agy `history.jsonl` that begins with a UTF-8 BOM indexes every
  conversation again, and a BOM behind leading whitespace is stripped rather
  than left to fail the JSON parse.

- A dsh record, and a grok record whose BOM sits after leading whitespace, are
  parsed instead of dropped. The other parsers in the package strip the BOM;
  these two stripped it in an order that missed it.

- The ssh host-key store keeps a copy of itself. Every write leaves one at
  `known_hosts.bak` beside the store, and a store that goes missing, is
  emptied, or is overwritten by something else is read back from that copy
  and written to its own path again. Losing the file used to re-pin every
  host on the next connect, which accepts whatever key is presented once
  the pins are gone. The copy lives in the store's own directory: back up
  that directory to cover losing it.

- A `~/.ssh` default key that is there and will not load (wrong permissions,
  a key that needs a passphrase) says so in the audit log. The chain was
  quietly one credential shorter, and the rejection that followed named the
  host key instead. A name that is not there stays silent.

- The key reference (`?` or `h`) says what each key does from inside it. `q`,
  `esc` and `?` close the box, and a line says the action keys are muted while
  it is up; before, `esc` was described by whichever view was behind the help,
  so the list named an outcome the open help could not produce.
- The minimal view prints no tok/s for an engine that is down. It showed
  `0.0 tok/s` beside the down marker, naming two states at once; the full view
  already printed an error and no stats row.
- An engine's model name in the provider block is dim, so the engine and the
  model no longer render as one bold run.
- `--help` now says what `--frames`, `--plain` and `--json` do together:
  the two text reports render the last snapshot alone, so a count above one
  only changes the wait. `toktop --help update` prints the update screen,
  which the prose did not list among the ways to reach it.
- `scripts/screenshot.py --help --bogus` exits 2 naming the flag. The help
  flag answered before the options were looked at, so a mistyped flag next
  to it exited 0 having printed help.

- A grok transcript reporting an absurd turn length no longer wraps its span
  negative, which reported the turn's rate with the wrong sign.
- An agy conversation named under two workspaces in
  `cache/last_conversations.json` is attributed to one of them now, not to
  whichever the read happened to reach first. The smallest path wins, so the
  same conversation does not move between polls.
- A session directory name that decodes to a path carrying a NUL byte is no
  longer read as a working directory, for grok and for anything else matching
  that spelling.
- A definition pointed at a log that reports its token total under `total`
  (what the Gemini CLI writes, and what opencode writes in a message) reads
  that total instead of reporting no context size.

- Grok's tok/s is the turn's own tokens over the time the model spent.
  The counts arrive once, when the turn ends. A rate taken from the gap
  since the previous turn never formed, and dividing by the whole turn
  counted tool time as generation. Cached prompt tokens that are already
  inside the reported input are not added again.
- `agentusage.Rate`, `InputRate` and `ThinkingRate` divide by the recorded
  model time (`Sample.Span`) when the transcript reported one, and by the
  gap between the two readings when it did not. A program using this package
  to show a rate had to make that choice itself, and the gap is the wrong
  interval for an agent that reports a turn's length: the counts arrive when
  the turn ends, so the gap also covers the tool calls the turn spent
  waiting. Samples with no recorded span rate as they did before.

## [0.18.2] - 2026-09-28

Binaries, checksums, and a CycloneDX SBOM are on
[GitHub Releases](https://github.com/maci0/toktop/releases/tag/v0.18.2).

### Fixed

- `dsh web` shows up under `--agents`. The server's working directory is the
  harness, and the sessions it writes belong to the projects it was asked to
  work in, so a watch of only that directory saw no tokens. The server's
  watcher reads both session stores. A dsh run inside one project still
  reads only that project.

## [0.18.1] - 2026-09-28

Binaries, checksums, and a CycloneDX SBOM are on
[GitHub Releases](https://github.com/maci0/toktop/releases/tag/v0.18.1).

### Fixed

- A Windows watch of dsh, cursor-agent, or grok no longer counts the same
  transcript once per spelling of the working directory. Those spellings are
  the same folder, so prompt and output rates were doubled.

## [0.18.0] - 2026-09-28

Binaries, checksums, and a CycloneDX SBOM are on
[GitHub Releases](https://github.com/maci0/toktop/releases/tag/v0.18.0).

### Added

- cursor-agent transcripts under `~/.cursor/projects/<project>/agent-transcripts`
  are read when a line carries token counts. A line that names none
  contributes nothing. The project directory is the working directory with
  its separators folded into the name.
- omp (oh-my-pi) sessions under `~/.omp/agent/sessions` are read the same way
  as pi. A session counts for the working directory its header records.
- dsh also reads `~/.dsh-native/sessions`, the uncompressed store beside
  `~/.dsh/sessions`. A log that never writes a cwd still counts for the
  project directory it sits in.

### Fixed

- pi, prime-agent, and feynman session logs count `cacheRead` and
  `cacheWrite` as billed prompt. Those shares were dropped, so a turn whose
  prompt was almost entirely cached reported only the uncached remainder.
  A session is attributed by the cwd on its header, so another project's
  transcript in the same store is not counted.

## [0.17.1] - 2026-09-28

Binaries, checksums, and a CycloneDX SBOM are on
[GitHub Releases](https://github.com/maci0/toktop/releases/tag/v0.17.1).

### Fixed

- A Grok session directory encodes `:` in the working directory. On Windows
  that path is `C:\...`, and the colon was left in the directory name, so
  the folder could not be opened and the session's usage was never read.

## [0.17.0] - 2026-09-28

Binaries, checksums, and a CycloneDX SBOM are on
[GitHub Releases](https://github.com/maci0/toktop/releases/tag/v0.17.0).

### Added

- Gemini CLI chats are read under `--agents`, from
  `~/.gemini/tmp/<project>/chats`. A `gemini` record's `tokens` are that
  turn's own counts, and the copy written again when its tool calls finish
  is not counted twice. The project is the directory `.project_root` names.
  A record that carries `usageMetadata` instead is read the same way.
- Grok sessions are read from `usage.json` under
  `~/.grok/sessions/<project>/<id>`. The file is that session's own totals,
  rewritten in place, and the project directory is the one the CLI named for
  the working directory.
- Antigravity CLI (`agy`) transcripts under
  `~/.gemini/antigravity-cli/brain/<id>` are read when a step carries token
  counts. A step that names none contributes nothing. The workspace is the
  one `history.jsonl` records for that conversation, which the step itself
  does not name. A conversation absent from that log is taken from
  `cache/last_conversations.json` when that file names it.

### Fixed

- A transcript directory that is not there is an empty store, not a failed
  walk. Clanker reads `<project>/state`, which is absent until the agent
  writes it, and a process whose working directory was a deleted zig cache
  temp logged `agent transcript walk failed` once a second for each of those
  paths. A directory that cannot be read for any other reason is still
  reported.
- On Windows, a process's CPU time is converted from the 100-nanosecond units
  Win32 reports into the same jiffies the other platforms count. The divisor
  was 100 of those units per jiffy instead of 100,000, so a process using half
  a core was reported as using five hundred, and anything busier than that
  sat at the panel's ceiling.

## [0.16.0] - 2026-09-28

Binaries, checksums, and a CycloneDX SBOM are on
[GitHub Releases](https://github.com/maci0/toktop/releases/tag/v0.16.0).

### Breaking

- An ssh target whose host or user carries an invisible formatting character is
  refused, with the reason named. A right-to-left override or a zero-width
  character carries no part of a host name or a user name, and the string it
  produces is not the one ssh dials: the bytes on the command line went to one
  host, while the trust-on-first-use store, the first-use prompt, the host
  label and the audit log all showed the host the operator believes it to be.
  The set is the one `core.SanitizeText` names (bidi controls, zero-width and
  other format characters, tag characters, variation selectors), so the check
  and the sanitizer the rest of the tree renders with cannot drift; a zero-width
  joiner stays legal, so an emoji spelled with it is still a target. The
  characters have to be removed from the name for the target to work, which is
  the point: `ssh` itself dials what the operator typed.
- A `POST /v1/events` event `id` past 128 characters, or one that is nothing but
  whitespace or control characters, is refused with a `400` naming the field
  instead of being clamped. The id is the key the retained feed deduplicates
  on, so clamping it folded every longer key sharing that prefix onto one
  stored id and dropped the second event as a duplicate of the first, and an
  id that sanitized to nothing left the event unkeyed, counted again on every
  replay. An id of exactly 128 characters is still stored whole.

### Added

- `remote.DefaultPollEvery` is the 5s an ssh host is sampled at, and what
  `Stats.Run` uses when its period is not positive. A caller that wanted the
  default copied the number, and the two drifted the moment either side
  changed.

- Kimi Code CLI sessions are read under `--agents`, so one shows a live rate
  beside the other agents. It keeps an event log per session and per agent
  under `~/.kimi-code/sessions/<workDirKey>/<session>/agents/<id>/wire.jsonl`,
  where each model call appends a `usage.record` carrying that call's own
  prompt and output counts; `token_counting.measured` records the context size
  instead and is not read as usage. The store holds one directory per project
  and hundreds of thousands of files, so it is never walked whole: a directory
  is named for a slug and the SHA-256 of the working directory it belongs to,
  and only the directories naming this run's working directory are read.
- The local host-vitals sampler audits a `/proc` file it cannot read, once per
  outage and again when it can. Memory, load and uptime are read from files
  every Linux host has, and a container without procfs mounted, a hardened
  kernel or a revoked permission blank the host strip exactly the way an idle
  machine does, for the rest of the run, with nothing written anywhere. The
  line names the file and the reason; a recovery line says how long it lasted.
- The engines a run measures on this host are recorded once, at attach. Every
  other attach line in the audit log describes something that went wrong, so
  an engine that answers for the whole run wrote none of them, and a log that
  stopped at the startup record could not say whether discovery had found one
  engine or none. The record names the count and the endpoints, matching what
  each ssh target already records for itself.
- A hot reload is audited before the process is replaced, so a log read across
  one shows a run that ended and a run that began with the line naming the
  gap between them.
- A `--demo` run names its seed, and the `--origin` it was pinned to, on the
  startup line and in the audit record. The report that carried both was never
  written for a run that ended early, so a demo run that crashed left the log
  behind without the one input that reproduces it: the same `--seed` and
  `--origin`, spelled as they were given, replay the run.
- The run's active configuration is audited as one record at startup, beside
  the line stderr already carried. The dashboard hides stderr under the alt
  screen, so the startup line was the one piece of a run's configuration that
  did not outlive it: the audit log named the engine failures, the ssh losses
  and the agent-walk errors of a run without saying which interval, which
  endpoints and which log floor produced them. The same record names the
  address the ingest endpoint actually bound, which an `--ingest` on port 0
  leaves the requested line unable to say, and a run whose endpoint stayed
  disabled is now audited with the reason rather than with stderr alone.
- A probe that fails is audited, latched per engine: one line when the failures
  start, one when a probe answers again, and nothing for a probe that keeps
  answering. A generation is the most expensive request `toktop` issues, and a
  failing one was visible only on the PROBES pane, for the frame it happened to
  be drawn on: an unattended `--probe` tick on an engine that will not generate
  left no trace once the dashboard was closed, and the engine kept answering
  its polls, so the collector's engine lines never named it. The failure line
  carries the model, the wall time the generation took and the engine's reason.
- `make release-verify VERSION=x.y.z` fetches every asset a published version
  holds back from GitHub and re-verifies each digest against that release's
  own `checksums.txt`, so a short, empty, or unlisted asset fails here rather
  than on an operator's machine. The release job runs it as its last step:
  the publish step's exit status says the upload was accepted, not that
  every artifact arrived, and a job killed after the release exists leaves
  the already-published guard refusing every retry of that tag.

- `--origin` pins the instant a `--demo` timeline starts at, as an RFC3339
  instant or Unix seconds. The seed decided every simulated value but not the
  instant they are stamped with, so two runs of one seed agreed on every number
  and differed in every timestamp, and `--once --json` of the same seed could
  not be diffed. With an origin, the seed plus the origin replay a run byte for
  byte; the JSON report carries the pinned instant as `demo_origin`, and
  without `--origin` it is omitted and the timeline still starts at the wall
  clock. An `--origin` that parses as neither form aborts the run.

- `agentusage.ThinkingRate` reports reasoning tokens per second between two
  samples, under the same rules as `Rate` and `InputRate`. `Sample.Thinking`
  and `Delta.Thinking` were already public, so a consumer showing a thinking
  rate had to write the division itself.
- `agentusage.SetLogger` sends the lines the package audits (a transcript
  store that could not be walked) to a logger the embedding program chooses.
  Until now a program that only imported `agentusage` read them through
  toktop's own audit configuration: `TOKTOP_LOG_LEVEL`, a text handler on
  stderr, and a message that began `toktop:`. The default is the process
  logger from `log/slog`, which is where a Go program already sends its own.
- `agentusage.DefaultPollInterval` is the 250ms a transcript is re-read at,
  and what `Watcher.Run` polls at when its interval is not positive. It was
  an unexported constant documented only on `Watcher.Run`, so a caller naming
  an interval copied the number out of that doc and drifted the moment either
  side changed.
- kimi session usage logs are read under `--agents`, as a machine-wide store
  under `~/.kimi-code/sessions/<workDirKey>/<session>/agents/<id>/wire.jsonl`
  carrying a `usage.record` event per model call. The log names no working
  directory, so ownership follows the cwd the session's `state.json` records,
  and a subagent's log under the same session counts on its own tokens. No
  build tag and no flag gates it: a `toktop` upgrade starts reporting these
  agents for anyone who had kimi sessions on the machine. `KIMI_CODE_HOME`
  moves the store when it is absolute, and a relative value is named at
  startup under `--agents` rather than ignored in silence, the rule
  `XDG_DATA_HOME` and `XDG_CONFIG_HOME` already followed.

### Changed

- A transcript walk that could not finish audits `agent transcript walk
  failed` without a leading `toktop:`, like every other line in that stream
  now carries the program from its own log handler. toktop installs its audit
  logger into `agentusage` at startup, so the line still reaches the same
  stream at the same level, folded and redacted as before.
- `agentusage.Spec.Suffix` and `agentusage.Spec.Suffixes` entries are trimmed
  before they are matched, and one left blank falls back to the `.jsonl`
  default. A definition written by hand with a padded suffix searched for
  files whose names ended in the padding and found nothing, so the agent
  reported no rate at all; the trim is the same rule `Spec.Suffixes` already
  applied to its blank entries, and both now go through one named default.

- A dashboard pane too narrow for a full row now shows the measurement and
  shortens the decoration, where before the decoration was drawn at its own
  fixed size and the measurement was cut off the right edge. An engine row
  sizes the kv gauge from the space its rates and queue counts did not take,
  and drops the gauge below three cells rather than clip `run`/`wait` behind
  it, so a queue backing up no longer reads as a missing value. A probe row
  measures first and names the model in what is left, dropping the `tok/s`
  and `ttft` unit labels before it drops a number, and a failed probe says
  `failed` where it used to carry a `✗` mark. The agent table sizes its rate
  and token columns to the rows instead of to 22 and 18 cells, so the `● live`
  recency cell at the right is no longer the one a narrow pane cuts.
  Nothing is measured differently: `--once --plain` is unchanged, and a pane
  with room for a full row still draws one.
- An agent row whose tokens are counted by an engine names that engine where
  the rate goes, in the rate cell, and the recency cell no longer prints it a
  second time beside `live`. Before this the row showed a vague `via engine`
  in the cell a rate belongs to and the address over by the recency cell, so
  the one number a reader looks for first was the one cell the row could not
  use. Every other surface (the panel title, the compact strip, the feed line
  and the plain report) already named the engine once.
- A single agent is `1 agent` in the pane header, the compact strip and the
  linear report, where the header and the strip read `1 agents` before. The
  count is spelled in one place now, so the three surfaces cannot disagree.

### Performance

- The site's dashboard capture is 22% smaller at the width a phone asks for
  (10,577 bytes at 768w against 13,563), at 30.0 dB PSNR against the resized
  source.
- The site Worker builds one content coding per request, the one that request
  will send, instead of building all three inside the first request a cold
  isolate answers. Measured on this page brotli takes 23 ms, gzip 0.7 ms and
  zstd 4 ms, so a client that accepts all three paid for the sum. A client
  that accepts only a coding the runtime cannot build now falls through to
  the next one it named, and gets its `406` only when none of them can be
  built.
- Every engine in a frame is matched against the process list through one
  parse of its address rather than two (`urlPort` and `isLoopbackURL` each
  parsed the same string on every frame of every engine), and a provider key
  is built once per probe sweep rather than per lookup.

### Fixed

- A forwarded port pipes at most 64 connections at once, and tearing the ssh
  connection down closes the ones it is already piping. A client that opened
  connections and stayed silent held a file descriptor, two goroutines and an
  ssh channel apiece for as long as the dashboard ran.
- A long `--agents` run releases the bookkeeping of the least recently written
  counted transcripts once it is following more than 512 of them. The read
  position is kept, so a later append still counts as growth, and the tokens
  already reported stay reported. Without the cap a store that keeps every
  session inside the recency window grew one entry per file in each of the
  watcher's maps for the life of the run.
- Shutting down no longer waits forever on an agent read stuck in a transcript
  on a mount that stopped answering. The wait is three seconds, and the tail
  read of an agent that has exited is dropped rather than holding process exit.
- A defined agent's transcript that reports cached prompt shares is counted
  for all of them. `cache_read_input_tokens`, `cache_creation_input_tokens`
  and Kimi's `inputCacheRead` and `inputCacheCreation` were either ignored or
  folded by maximum, so a line carrying only a cached share read as no usage
  and a line carrying all three read as the largest share. They add to the
  uncached share, the same fold the claude and dsh parsers already apply.
- A wall clock stepped backwards reseeds an engine's throughput instead of
  holding the previous rate for as long as the step lasts. A sample at the
  same instant as the one before it still holds the prior rate, so two
  readings with no elapsed time do not produce a NaN.
- A local process listing copies the arguments it keeps. The listing splits
  one `/proc` buffer into windows, and returning those windows pinned the tail
  past the prefix (an inline prompt, a path, a credential) for the life of the
  sampler, which is the retention the prefix exists to prevent.
- A host-key pin with a trailing comment is the same pin as the line without
  one. A store another tool annotated was read as a changed key on the next
  connect.
- An ssh config value may carry a trailing `#` comment. `HostName`, `Port`,
  `User` and `IdentityFile` kept the comment in the value, which then failed
  the host check or failed to parse and was dropped. A `#` with no whitespace
  before it stays part of the value.
- A `--json` demo report includes `demo_seed` when the seed is 0. The field
  was an integer with `omitempty`, so that one seed was omitted and a replay
  could not tell it from a run that named no seed. A run that is not a demo
  still omits the field.
- A CPU temperature label is trimmed of its trailing comma before it is
  shortened, so the dashboard's seven-cell budget is spent on the label
  rather than on the separator the split needed.
- `toktop update` restores a binary left beside the install path by a killed
  update before it tries the network, and a run that is already that release
  removes the leftover. A download that then fails used to leave the install
  missing, and a release that was already installed left the displaced copy
  in place until a later install needed the name.
- `$TOKTOP_LOG_LEVEL` with `--demo --no-ingest` is described as setting the
  startup config record. The warning said no audit log was written, which the
  startup record contradicted.

- A Kimi Code CLI session's tokens are counted again. The working directory a
  session ran in is recorded in its `state.json`, which the CLI writes beside
  the session's `agents/` directory, one level above the `wire.jsonl` the
  transcript is read from. The lookup went up two levels, landed in `agents/`,
  and found no `state.json` there, so every kimi session was left undecided and
  every poll retried it, reporting nothing for the life of the run: an agent
  that spent tokens read as one that spent none. The walk now looks for the
  file up the tree rather than counting levels, so a change in how deep the CLI
  nests an agent id does not read the wrong directory again.
- The `demo_origin` a `--json` report names keeps the sub-second precision of
  the `--origin` it was pinned to. `--origin` takes a fractional RFC 3339
  instant, and the report printed it to whole seconds, so the value a replay is
  fed back named an instant up to a second earlier and the replayed run
  stamped every frame ahead of the capture it was read from.
- Two watchers never tail one agent's transcripts at once. A discovery pass
  stopped the trackers whose process had gone at its end, after it had already
  handed the store one of them held to a surviving process on the same store.
  The dead watcher's poll loop kept running through that handover, so both
  reported the same growth under their own PID, and two different sample ids
  are exactly what the collector's id window cannot merge: every token written
  between the two reads was counted twice. Discovery now stops the exited
  trackers before it installs any watcher, so the survivor's baseline is taken
  only once the dead watcher's final growth has been reported, and the two
  readings partition the same transcripts instead of overlapping them.
- The engine kind badge is padded and cut in terminal cells rather than
  characters. `%-9.9s` counted runes, so a kind spelled in a wide script
  padded to 9 runes while rendering 18 cells, pushing that engine's label 9
  cells right of every other row and overflowing a narrow pane; the precision
  also cut between runes, leaving a combining mark or a multi-byte sequence
  split. It is the one fixed-width cell in the frame, and it is now measured
  the way every other cell is.
- Text matched against ASCII literals is folded with `core.FoldASCII` rather
  than `strings.ToLower`, at every remaining site: process names and command
  lines, hwmon labels and `/proc/cpuinfo` keys, the probe's model-name and
  Content-Type checks, the engine-discovery body sniff, the ingest event
  `kind`, the log level, the ssh target's localhost test, the Windows named-pipe
  prefix, and the GitHub host allowlist `toktop update` trusts.
  `strings.ToLower` also folds runes whose lowercase form is ASCII (U+0130 to
  `i`, U+212A to `k`), so a name spelled with one satisfied a match its
  producer never wrote: a chip named `nvdİa` counted as an `nvidia` GPU, an
  argument naming `gpustacK.start` was claimed as a GPUStack engine, and a
  release asset on a Kelvin-signed host passed the download allowlist. The
  fold also leaves an invalid byte alone, where `ToLower` rewrote it to
  U+FFFD, a name no other process on the machine spells it as. The host folds
  compose to NFC first, so a host typed in the NFD form a macOS terminal
  supplies is the host already trusted.
- `esc` is described by what it does from the view you are in. It closed help,
  returned to the engines dashboard when the agents dashboard had focus, and
  quit otherwise, and the help screen named only the last two, so a reader who
  had pressed `a` had no reference telling them `esc` was the way back.
- `--frames` is named as a wait-only knob under `--once --json`, the way it
  already was under `--once --plain`. Both reports render the last snapshot
  alone, so the count buys the wait before the render and nothing else, and a
  flag that warns in one of the two and stays silent in the other read as a
  difference the reports do not have.
- A GitHub rate limit refused by `toktop update` says so. The API answers a
  spent anonymous quota with 403 or 429 and an otherwise bare
  `github returned 403 Forbidden for ...`, which names a URL and nothing the
  reader can change; the message now names the rate limit and
  `$GITHUB_TOKEN`, the variable the update help screen already documents for
  exactly this.
- `toktop help version` and `toktop version --help` are documented as printing
  the top-level screen. `toktop help update` has its own screen and version
  takes no flags, so the usage block claiming a subcommand screen for both
  sent readers looking for one that does not exist.
- An agent's clock offset follows a corrected sender instead of being decided
  by the first event the name was seen on. The ledger held one reading per
  agent for fifteen minutes, and an agent name is not a sender: a host whose
  clock was 90s fast and has since been corrected kept every later event 90s
  stale, and two machines both running `claude` post under one name, so the
  fast one dragged the other's events with it. Past the 30s rate window those
  events count toward no total and the agent reads as idle while it works. The
  ledger now holds the smallest lead seen, and a smaller one takes over.
- An agent row names its engine only when every event in the window went
  through that one. The label was read off the last event the walk reached,
  which the feed's lack of ordering made arbitrary, and it claimed "the engine
  counts these tokens" for an agent whose direct tokens the header total
  counts anyway. A row that also spent tokens direct, or that named two
  engines, now shows its measured rate; the per-event feed lines still name
  the engine on the events that went through one.
- An agent's rate spans its first to its last event in time, not to the last
  event the walk happened to reach. The feed is not time-ordered: the ingest
  endpoint accepts any `ts`, and a producer's clock can step. One out-of-order
  stamp used to shorten the span, or turn it negative and drop the rate
  entirely, so an agent that reported 80 tok/s a moment ago could read as
  having no rate at all.
- A `SIGTERM` stops the live dashboard instead of leaving it on screen. The
  run context carries the signal, and the context's default dispositions were
  replaced, so a process that watched only for keyboard input had nothing left
  to stop it: the dashboard kept painting the last snapshot it was given while
  every collector behind it had already stopped. It now exits 130, the code
  `--once` and `toktop update` already return for the same signal, and the
  terminal is restored on the way out.

- Failures that reported "nothing happened" now reach the audit log. A crush
  or opencode store that exists and cannot be read, a dsh transcript frame
  that will not decode, a process listing with no last good snapshot, a
  tunneled port that cannot be dialed, and a known_hosts backup that could
  not be deleted each wrote one line naming the resource and the cause
  instead of leaving a blank panel or a quiet agent behind. A store that is
  simply not installed stays silent: that is an answer, not a fault.

- `--agents` under a stripped environment (no home directory, no absolute
  `GAUNTLET_HOME`) now names the cause and exits 2 instead of starting with
  every in-house agent missing from the watch.

- An engine address whose port is 0 is refused, with the address named. The
  port parsed, so the address joined the sweep the agent watcher compares
  connections against, and no connection ever holds that port: the entry
  labelled no agent and left nothing in the output to say why. A misspelled
  address is now reported like any other malformed one.

- A host-key store that would not parse quoted the bytes it choked on whole.
  The reason a record was refused carried the malformed key blob, and a host
  recorded twice with different keys named the host field unbounded, so
  arbitrary store bytes, including escape sequences and invisible formatting
  characters, reached the terminal through an error meant to name a file and
  a line. Both go through the same snippet cap as the line itself now.

- An agent whose host clock runs ahead of the dashboard's never left the agent
  list. An ingested event carries its sender's timestamp, and the endpoint
  accepts one up to its skew bound ahead of arrival, so those stamps sit in
  the dashboard's future: nothing is ever older than the 30s rate window's
  cutoff, and the agent stayed listed with its tokens in the header totals
  however long the host had been quiet, with a `last` in the future beside an
  `idle` recency cell. A sender's offset is one clock reading, not elapsed
  time, so it is recorded once per agent and subtracted from that agent's
  stamps: the feed keeps the spacing the sender measured, and with it the
  rate derived from that spacing, and the agent ages out one window after the
  events that named it arrived. A sender on the dashboard's own timeline, and
  every locally watched agent, is stored exactly as before.

- `--probe` under `--demo` ran off the wall clock, so a run carrying a seed
  and an origin stopped replaying. Auto-probe fired from a real ticker while
  every other value came off the simulated timeline, so how many probe waves
  ran, and the instant each one was stamped, followed how long the process
  happened to take: two runs of `--demo --seed 7 --origin ... --probe 1`
  printed the same numbers under different probe timestamps. The cadence is
  simulated too now, armed on the demo source and fired by the frame that
  crosses the boundary, so the wave count and every wave's stamp are
  functions of the run and the two replays are identical again. A live run
  still probes off the clock, which is what a real engine measures.

- Diagnostics on Windows left the account name in any home path spelled with
  `/`. Redaction matched the home against the platform separator only, and
  Windows names one directory with either separator, so a path reaching a
  message from a user-supplied argument, an ssh target, or a tool built for
  another platform was copied into a log line, an issue, or a bug report whole.
  The other spelling is folded now, and a message that is exactly the home
  directory collapses to `~` whichever way it is written.
- The audit line `agentusage` writes for a transcript walk that could not
  finish folded the store root to `~` and left the error beside it whole, so
  the home directory the error names (the path inside that store the walk
  failed on) reached the line that gets pasted into an issue. A host that
  installs its own logger through `SetLogger` had no fold of its own to catch
  it. Both values are folded now, from one place, so the two call sites
  cannot drift.
- The ingest examples in the README and on the site taught the two shapes a
  harness must not copy: a key built from the clock
  (`Idempotency-Key: coder-$(date +%s)-1`), which the shell re-evaluates on
  every attempt so a retry presents a key the feed has never seen, and, on
  the site, no key at all. Both are counted a second time when the answer is
  lost and the request is resent. Each example now carries a key that names
  one turn and says why a clock does not.
- `toktop update --repo` named a malformed repository as `repo "x" must be
  owner/name` and exited without the usage screen, while every other usage
  error in that subcommand names its flag in the long form the help screen
  documents and prints the screen underneath. The flag is now named and the
  screen shown, as an unknown flag or a missing value already did.
- The startup configuration line named `--plain` in a `--once --plain --json`
  run, which renders the JSON object and ignores the text report. Only the
  report that is actually printed is named now.
- `cmd/toktop/main_test.go` carried a table literal that `gofmt -s` rewrites,
  so `make check` and the CI formatting gate failed on a clean tree. The file
  is formatted as the formatter wants it.
- `toktop --opencode-db` stated its default twice in `--help`, once as
  "default on" inside the description and once as the "(default true)" every
  non-zero default carries.
- `scripts/screenshot.py --bogus` answered a mistyped option with the whole
  usage screen instead of naming it: a lone option is one argument, so the
  positional count was judged first. Options are now named before the count.
- `GET /healthz` ends its body with the newline every other answer on the
  ingest endpoint already carried (`http.Error` appends one, the `202` ack
  writes one, and the site's own `/health` answers `ok\n`). A probe reading a
  whole line no longer has to special-case the healthy one.
- A local process listing keeps only the leading 4096 bytes of a command line,
  the same prefix the engine matchers read and the same one an `ssh://` sweep
  ships. A browser, an Electron app or an agent started with a long inline
  script no longer leaves its whole command line (flags, prompts, paths past
  the cut) in a structure the dashboard holds for as long as it runs. Nothing
  past the cut decided a match or a port before either.
- Per-process CPU is reported on Windows. The CIM query read no CPU time, so
  every engine process showed 0% there while Linux and macOS showed the real
  figure; `Win32_Process` kernel and user times are now part of the one query
  and folded into the same jiffy delta the other platforms use.
- A home directory macOS stored with a decomposed character (`rène` as `e`
  plus U+0301) is folded out of audit lines and diagnostics in the composed
  spelling a process carries. The redaction compared bytes, so the same account
  survived whenever the two spellings disagreed.
- An engine-supplied model name, an agent name, a note, an engine or remote
  error, and every other field the dashboard renders into one cell of a row
  are folded to a single line. A newline in one of them used to become a row
  of its own: the renderers measure a cell as its widest segment and split a
  rendered row on newlines, so a sender at the unauthenticated ingest
  endpoint, or a process squatting a discovered engine port, could print a
  line the dashboard reads as its own output and spend row budget that pushed
  the real rows off the pane.
- `--add` refuses a URL carrying a query string or a fragment. Every request
  is built by appending a path to the base, so such a URL never worked, and
  the query is where an api key tends to be written: the value reached the
  audit log, the dashboard and both reports whole, which is where the help
  screen says a credential does not go.
- `toktop update --check` prints the release page URL only when it is an
  https GitHub URL, the rule the release assets already pass. The value is
  documented for `url=$(toktop update --check)`, so it lands in a shell
  expansion.
- A dsh session window of nothing but newlines is walked rather than
  materialized. A few kilobytes of compressed newlines decompress to 8 MiB,
  and splitting that into a slice cost about 1.2 GB of allocations on the
  poll that read it; any process able to write the session file could make
  the dashboard do that every 250 ms.
- A poll over a dsh window, an engine scan and an engine error text is
  bounded: the identification decoders read what answers on a well-known
  engine port, which is untrusted by construction, and now cap the body the
  way every other engine read in this program does. An engine error string is
  sanitized and folded like every other engine-supplied field, and the engine
  and remote error text the dashboard shows has the home directory folded out
  of it, as the audit log already did.
- `TOKTOP_LOG_LEVEL` is no longer reported as unused under `--no-ingest`. The
  variable sets the floor for every audit log the process writes, and the
  engine collector, the ssh client and the `--add` attach path all write one;
  only a `--demo --no-ingest` run, which measures nothing real and has no ssh
  target, builds no logger at all, and that is the case now named.
- An absolute `$GAUNTLET_HOME` holding no `agents.json` is named at startup
  under `--agents`, the way a relative one already was. A missing definitions
  file is not an error to load, so the directory the operator pointed at being
  empty read as in-house agents producing no tokens.
- `--agents` follows a transcript store once, so the first process found on it
  tails it and the rest are tracked without a reader. When that process exited,
  the remaining processes on the same store were never given one, so a live
  agent that shared a session store with a process that quit reported no tokens
  at all until it was restarted. The surviving process now takes the store over.
- On macOS, a display that reports several `vram` fields and no bare `vram` one
  (the `spdisplays_vram*` spellings) had its total VRAM read from whichever
  field Go's map iteration reached first, and that number is cached for the
  life of the process. Two runs of one build on one Mac could report different
  VRAM for the same card. The bare key still wins outright; the remaining
  candidates are now tried in name order.
- A definitions reload that stops naming an agent stopped that agent's watcher
  as well. `ResetDefinitions`, or a definitions file that no longer lists the
  agent, left the watcher holding the adapter the last load gave it, and it
  went on walking and billing a transcript store no spec claimed any more
  for the life of the process. A spec with no usable root is withdrawn the
  same way. The counts already published stay, so an agent whose definition
  went away holds its last sample instead of dropping to zero and reading as
  an agent whose sessions were deleted; a load that names it again re-derives
  over the same watcher.
- An OpenAI-compatible engine answering a well-formed listing with an empty
  `data` array is now read as that kind of engine. The answer was compared
  against a non-empty listing, so an engine serving no model yet was
  indistinguishable from an endpoint that is not a listing at all, and the
  switch that reaches every kind of engine never reached it. An absent `data`
  is still the shape of a non-listing endpoint.
- A malformed message payload in an opencode session store blinded every
  reading of that agent. The token columns were extracted with a bare
  `json_extract`, which raises on input that is not well-formed JSON, and one
  truncated or non-JSON row failed the whole statement for as long as the row
  survived. The extract is guarded, so such a row reads as absent, and a
  negative stored counter is floored at zero instead of subtracting from the
  sessions that read fine. The store is another program's and is opened
  read-only, so nothing here constrains the rows.
- A GPU vendor CLI that fails now says so in the audit log, once at the start
  of an outage and once at its end. A driver that has been unloaded, a wedged
  `nvidia-smi`, or a container whose GPU device vanished all blank the GPU
  row, and a machine with no GPU at all looks the same on screen, so an
  operator debugging a missing readout had nothing to read. The line is not
  repeated per poll, which for a tool uninstalled hours ago would be one line
  every interval for the rest of the run.
- The release binaries carry the IANA zone database. The header clock, the
  feed timestamps and the text report all render through `time.Local`, which a
  released binary resolved against the host's zone files: on a host with none
  (a scratch or distroless container, a downloaded binary on a machine with no
  Go toolchain) the runtime fell back to UTC in silence, so the clock read two
  hours behind and feed events carried instants the sender never sent. The
  zone files on a host that has them still win, so a desktop install is
  unchanged; the binary grows about 440 KiB.
- The peer's stderr tail quoted into an ssh connection error is bounded in
  grapheme clusters rather than in bytes. The cut landed inside a multi-byte
  sequence, and trimming the partial rune still lands inside a grapheme, so an
  "e" whose combining acute fell on the wrong side printed as an unaccented
  letter in the one line of the traceback the operator needed.

## [0.15.0] - 2026-09-27

Binaries, checksums, and a CycloneDX SBOM are on
[GitHub Releases](https://github.com/maci0/toktop/releases/tag/v0.15.0).

### Breaking

- An ingested event's `note` that is nothing but a path is stored reduced to
  its last two components. Before this release a note was kept as the sender
  wrote it, with only a path under `$HOME` folded to `~`, so
  `/home/you/clients/Acme/migrator` reached the feed, the live dashboard and
  the `--once --plain` report as `~/clients/Acme/migrator`; the feed now holds
  `Acme/migrator`. Everything above the checkout is where a client's name and
  a project index sit, and a feed redirected into a file or a journal kept
  them. A note counts as a bare path when it is a single token carrying a `/`
  or `\`, or spelled from `~`; a note with a space, a tab or a `·` in it is
  free text and is still stored as written, past the home fold. A sender
  whose note is a path cannot opt out of the shortening: put any other text in
  the note (`checkout: /home/you/clients/Acme/migrator`) and the whole string
  is kept with `$HOME` folded. The field is a display label, and
  [README.md](README.md#agent-feed-api) says so per field.

### Added

- `toktop --once --json` prints the last snapshot as one JSON object on
  stdout: the aggregate throughput, every engine with its rates, queue
  depths and models, the agent feed and per-agent rates, the probe samples
  and the host vitals. It is the machine-readable counterpart of
  `--once --plain`, for a script that wants the numbers; the chart
  histories stay out of it, since a series is sampled across runs rather
  than read from one frame's buffer.
- `agentusage.Sample.Delta` returns the growth between two samples, and
  whether there was any. A watcher reports the running total, so every
  program emitting events had to difference two samples itself and decide
  what a transcript rewritten under the watcher means; this is that rule in
  one call, and reports no growth rather than a negative count.
- `agentusage.Watcher.Run` documents the 250ms default it uses for a
  non-positive poll interval, and the runnable examples now cover the
  delta, engine-overlap, and polling calls the package had only prose for.
- A `--add` endpoint on plain `http://` whose host is not this machine is
  named at startup, because the bearer token crosses the network in
  cleartext there.
- An agent definition (`usage.suffixes`) can name more than one transcript
  extension, for an agent that writes a compressed file by default and a plain
  one when compression is off.
- A key that has nothing to act on (`p` with no engines, `t` before any
  throughput, `a` with no engines to swap to) says so on the footer for a few
  seconds instead of being swallowed.
- The help overlay is titled `KEYS`.
- `docs/PRIVACY.md` lists what toktop reads, sends and stores.
- Every release publishes `toktop_<version>_buildinfo.txt` next to the
  binaries, naming the commit, Go toolchain, build tags, and flags behind the
  bytes. A checksum list proves a download arrived intact; this says what
  produced it.
- `agentusage.UnregisterSpec` removes a spec `RegisterSpec` added and restores
  the adapter it displaced, so a program (or its own tests) can take a
  registration back out of the process-wide registry.
- `agentusage.ResetDefinitions` drops every definition `LoadDefinitions`
  added, leaving the ones compiled into the build. It is the undo that call
  otherwise had none of, for a test that loads a definitions file.
- `agentusage.ErrCollidingDefinitions` names the one cause of a rejected
  definitions file that a caller can act on differently from bad JSON: two
  agent names in the file that reduce to the same key.
- `agentusage.Watcher.Err` reports that a watcher `agentusage.Watch` returned
  is nil because the agent keeps nothing readable, and matches the new
  `agentusage.ErrUnsupportedTool`. It is safe on a nil `*Watcher`, like
  `Tool` and `Dir`.
- `agentusage.SpecFor` reports the transcript location registered for an
  agent, roots as written, so a program can see which entries a definitions
  file registered and which it skipped. A `{dir}` root placeholder is
  documented on `agentusage.Spec`.
- `agentusage.Watcher.SetNow` overrides the clock that stamps published
  samples, so a caller running on an injected timeline gets samples stamped
  on it and derives the same event ids from the same readings. Transcript
  mtimes, `since` and the recency window stay wall time, because that is the
  clock the filesystem and the session stores record in.
- A first `ssh://` contact says the key was pinned, naming the host and the
  fingerprint. Trust on first use is silent, so a fresh config directory, a
  different account, or a container with no store accepted whatever key was
  presented, with nothing in the output to notice.
- toktop.ai writes one JSON object per line to Workers Logs when a request
  fails, each carrying the `cf-ray` of the request behind it, so a failure a
  visitor reports pivots to the edge request that caused it. Only failures log:
  a served page is the steady state, and a line per visit would bury the few
  that name a broken deploy. `site/README.md` lists the events.
- toktop.ai answers with a `Server-Timing: edge;dur=<ms>` header, so the
  number a visitor or a RUM script reads is the time to first byte from the
  Worker rather than an unbreakable share of a round trip.

### Security

- The release binaries are linked with `-bindnow`, so the Linux builds carry
  full RELRO rather than partial. The Go linker emits `DT_BIND_NOW` for
  nothing on its own, so the dynamic symbol table stayed writable for the
  life of the process. It is a no-op on macOS and Windows, so one flag set
  covers every release platform.

### Performance

- Polling costs less per frame on every host. A transcript record's working
  directory is resolved once per directory instead of once per record, a
  definition-backed agent re-derives its transcript roots only when the
  definitions file changes, a Prometheus scrape is matched against the few
  family names that can reach a metric instead of being lowercased and sorted
  wholesale, the amdgpu sysfs walk caches which cards are AMD's rather than
  re-globbing `/sys/class/drm` every poll, an ingested NDJSON line is no
  longer copied a second time just to name its JSON kind, and the zstd
  transcript decoder reserves its accumulator once and stops a frame at the
  budget instead of decoding one frame past it.

### Changed

- A `POST /v1/events` that times out now says which bound broke: no body bytes
  for a minute, or the 10 minute stream lifetime. Both arrived as the same
  `408` reading "request stalled", and the two need opposite fixes.
- An explicitly empty `--ssh-key` is a usage error instead of a silent
  fallback to `~/.ssh/config`, which authenticated with a key the operator
  did not name. An explicit empty `--bearer` already overrode the
  environment; a path flag now refuses the value too.
- A `$USER` or `$USERNAME` carrying whitespace or a control character is
  skipped as the default ssh login name, falling back to the passwd
  database, instead of being handed to the transport and reported later as
  an authentication failure.
- toktop.ai answers a revalidation or a refused encoding before it builds a
  compressed copy of the page. A 304 and a 406 carry no body, and both waited
  on the brotli, zstd and gzip pipeline first. An isolate that only ever
  serves revalidations now builds no representation at all.
- The feed panel's error line is rendered as the message arrives, and its
  badge now reads "feed error" rather than "ingest down". The agent watch
  reports through the same channel, so a monitored engine address that will
  not parse was being shown as an ingest outage with a "restart toktop to
  restore ingest" remedy that would not fix it.
- The in-app help overlay lists exactly the keys the footer advertises, under
  the same conditions. `p`, `t` and `a` are now left out of the reference when
  they have nothing to act on, instead of being advertised and then doing
  nothing when pressed.
- The throughput charts place each history sample on the time grid instead of
  testing every sample against every column. A 200-column frame over three
  engines' retention went from about 7.7ms to 2.8ms to draw, and what is drawn
  is unchanged: a sample still lands in every column within half a cadence of
  it, boundaries included.
- A remote's GPU row and driver versions drop when the remote stops reporting
  a GPU. The retained vitals sample was parsed into in place, so a card that
  went away (driver unloaded, vendor CLI uninstalled, the host turned into a
  VM) stayed on the dashboard for the rest of the run. A dump cut short
  before the last section still keeps the last good reading.
- `--demo` draws its probe samples from a second seeded stream, separate from
  the one the frames draw. A probe wave fired from the UI goroutine, or by
  `--probe` on real time, could land between two ticks and shift every value
  the next frame reported, so the same `--seed` replayed differently depending
  on when the key was pressed. Frame values now depend on the seed and the
  frames elapsed, probe values on the seed and the waves run.
- `$GAUNTLET_HOME` is honored only when it is an absolute path, like the XDG
  base directories. A relative one resolved `agents.json` against the working
  directory, where a missing file is not an error: the agents it defined
  never appeared, looking like agents producing no tokens. The ignored value
  is named at startup under `--agents`.
- The README no longer says a non-loopback `--ingest` bind is rejected. It is
  warned about at startup and runs, because a relay on another host is a
  legitimate setup.
- The same `--add` endpoint named twice is a usage error. Each `--add` builds
  its own provider and the dashboard sums them, so the duplicate read as
  double the tokens instead of as the mistake it was.
- Unknown `TOKTOP_*` variables are reported in sorted order, so the same set
  of names reads the same way in every capture of the startup output.
- `toktop --help` ends with the exit codes (`0`, `1`, `2`, `130`) and the
  split between results on stdout and status on stderr.
  `toktop update --help` gained the examples block the top-level screen
  already had, and says `--check` is pipeable.
- `toktop --help` lists the `TOKTOP_*` variables a run reads, their ranges,
  and that a flag beats the variable it mirrors, instead of only pointing at
  the README. Its exit-code line now says that `130` covers `--once` and
  `toktop update`, while the live dashboard quits on `q` or Ctrl+C with `0`.
- `toktop version --version` prints the version, as `toktop update
  --version` already did; only a real extra argument is a usage error now.
- A knob that `--once --plain` never reads is named, like every other flag
  passed into a mode that ignores it. `$TOKTOP_COLUMNS`, `$TOKTOP_LINES`
  and `--frames` set alongside `--plain` were silently dropped.
- An `Idempotency-Key` (or an event `id`) names one logical operation and one
  sender. The README says so, and its example mints a per-send key instead of
  a fixed `turn-1`: two POSTs under one key derive the same ids, so the
  second one's events decode and store nothing, visible only as `stored` below
  `accepted`.
- `--ingest` bound to a host *name* other than `localhost` now warns about
  the unauthenticated endpoint, as a literal non-loopback address always
  has; a loopback address or `localhost` stays quiet.
- A successful `POST /v1/events` answers `{"accepted":N,"stored":M}` and its
  log line carries `stored` too, so a sender (or an operator reading stderr)
  can see that a replayed id decoded and stored nothing.
- toktop.ai's section nav links `Run`, the first-run command block that had an
  anchor but no link.
- The `ssh://` discovery sweep sends at most the first 4096 bytes of each
  remote process's command line, which is all an engine match reads.
- The `DEMO` tag names the seed the run drew from (`DEMO seed 42`), and
  `--once --plain --demo` leads with `[demo seed 42]`. A demo frame is
  reproducible from that seed alone, so the frame has to carry it.
- A malformed event line in an NDJSON POST names the body offset it failed at,
  so the offending line is findable in a long stream.
- A dashboard image that the asset store cannot serve answers with the site's
  own `text/plain` error body instead of the store's HTML error page.
- Ingested events are deduplicated through an id index instead of a scan of
  the retained window, which normalized every retained id under the lock the
  emit path needs. A sender posting a large stream no longer pays a full
  window walk per line.
- Intel GPU metrics run one process per device concurrently, each with its own
  timeout. On a multi-device node the last device used to start only after
  three timeout windows had elapsed and could be dropped by them.
- Chart samples carry the instant they were taken instead of a time derived
  from their position in the history and the poll interval. A scrape that ran
  long, or coalesced ticks after a stalled frame, used to draw a window that
  was shorter than the time it claimed to cover; the axis now follows the
  recorded stamps, so a slow engine shows its real spacing and a stall shows
  as a gap.
- An engine request that answers with a redirect is no longer followed off the
  origin it started on: the hop is refused and the engine reports the
  redirect. Every engine request starts at an address nobody authenticated as
  a toktop peer (a scanned loopback port, or a port forwarded over `ssh://`),
  so a redirect turned that read into a request to any URL the host can reach,
  including instance metadata, and the answer reached the dashboard. A hop
  within the origin still follows, which also means an `https` to `http`
  downgrade is no longer followed with the engine token attached. An engine
  that answers a poll with a cross-origin redirect now fails that poll where
  it used to follow.
- toktop.ai revalidates a dashboard capture within an hour of its cache
  expiring instead of within a week. The captures are served under stable
  names, so nothing but a revalidation retires the copy a browser is holding
  when a deploy re-captures, and a week of that put a week-old screenshot on
  the page. The cost is one cheap conditional request on a visit that is
  already past `max-age`.

### Fixed

- An `ssh://` target `url.Parse` rejects said only "bad ssh target". The
  text is not echoed, because that is where an embedded password would be,
  so the error now names the accepted spelling instead of leaving the reader
  with nothing to change.
- Flag descriptions in `--help` hang under the flag name with spaces. The
  tab the flag package's own printer uses landed them on a tab stop.
- A wall clock stepped backwards (an NTP correction, a laptop resuming from
  sleep, a restored VM snapshot) no longer leaves a run's expiry windows
  counting up from a stamp in its own future. A negative age satisfied every
  "younger than the window" test, so until real time caught back up the probe
  wave gate stopped spacing its waves (a gateway can bill every probe token),
  transcript listings stopped re-walking and new sessions went unseen, the
  version endpoints were re-asked on every scrape, and an unreachable remote
  target kept its last vitals on screen.
- A session's own age is floored at zero rather than reported as ending before
  it began when the clock steps back mid-run. `--once --json` serialized the
  negative `uptime_secs` straight out.
- A model id an engine reports is trimmed, stripped of terminal control
  characters and capped at 256 characters before it becomes a snapshot entry,
  on every listing path (`/v1/models`, the LM Studio and Lemonade native
  feeds, Ollama's `/api/ps`). A model listing is engine-chosen data: a
  misbehaving or hostile server could return megabyte ids or embed an escape
  sequence, and each one rode every snapshot, the probe request body and the
  `--json` report at full length.
- `toktop update --help` names the `--repo` argument the way its own usage
  line does (`owner/name`) instead of the type name Go's flag package
  reports (`string`).
- A `TOKTOP_SSH_PASSWORD` that is set but empty is now named. A headless run
  said "set TOKTOP_SSH_PASSWORD" to an operator who had set it, and a
  terminal run prompted as if the variable had never been exported.
- `TOKTOP_SCREENSHOT_FONT` picks its Bold sibling by file name only. The
  substitution ran over the whole path, so a font under a directory whose
  name carried "Regular" resolved to nothing and every bold glyph was
  rendered in the regular weight.
- A host-key pin store left under `known_hosts.displaced` by a toktop killed
  between the two renames a Windows install makes is read back instead of read
  as no pins at all, which re-trusted every host the operator had connected to.
- `toktop update` on Windows restores an installed binary left under
  `toktop.exe.old` by a killed update before replacing it. The gap left a host
  with no binary to run the update that would have fixed it.
- The audit log folds the home directory to `~` in every line, in the message
  and in every attribute, not only where a call site remembered to. A request
  path, a rejected `X-Request-Id` or an error text carrying a path under
  `$HOME` names the account in a log that gets pasted into issues, and each of
  those was written by code that never thought about it.
- `make test-pkg RUN=<name>` no longer reports success for a name that
  matches no test. `go test -run` exits 0 with `[no tests to run]`, so a
  renamed or mistyped test read as a passing run; the target now lists the
  package's test names with the same regexp first and fails on an empty
  list, naming any near miss. A `RUN` containing a `/` selects subtests,
  which that listing cannot see, and passes through unchecked.
- The site's rejected-method test calls the worker's request helper instead
  of an `imageCall` that does not exist, which failed the `site` CI job and
  every `make pr` with a `ReferenceError` before any assertion ran.
- Redacting the home directory out of a diagnostic message no longer panics on
  macOS and Windows, where a path is matched without regard to case. The match
  is a rune-at-a-time fold, but the rewrite resumed at `len(home)`, and a home
  spelled with a character that folds to an ASCII one (K, U+212A) is a
  different number of bytes from the spelling in the message. The leftover
  bytes were re-emitted as a split multi-byte sequence, or the slice ran past
  the end of the string. The fold is now simple case folding, the same rule
  the bare-home comparison beside it already used.
- A plain-text version endpoint answered in a non-Latin script is no longer
  dropped for being over-long. The cap is 128 characters and the reject that
  screened for prose counted bytes, so a 128-character CJK version (384 bytes)
  was refused while the same version in ASCII was kept.
- `agentusage.Watcher.SetNow` now also ages the transcript recency and rescan
  windows, instead of leaving them on the wall clock. A program driving a
  simulated timeline stepped time forward and still read the file set a
  full-length run would have dropped, so a replay was not reproducible.
  Transcript mtimes and `since` stay wall time.
- An engine answering a version endpoint with a bare invalid byte, an escape
  sequence, or a quoted line break no longer has that text cached as its
  version and re-rendered every frame.
- `agentusage.LoadDefinitions` skips a `usage` entry naming an agent a
  compiled-in adapter already reads (claude, codex, dsh). Registering it left
  `SpecFor` reporting transcript roots that no watcher read, since the adapter
  outranks every definition. A definition still replaces a compiled-in
  *definition*, which is what pi, prime-agent and feynman are.
- The ingest endpoint audits an accept failure that stops it, on the same
  logger, at error level, with the bound address and the reason. It was one
  unstructured stderr line that ignored `$TOKTOP_LOG_LEVEL`, so a feed that
  stopped accepting left no line to filter for.
- A definitions entry that names no transcript root no longer rejects the
  whole file when a later entry reduces to the same agent name. It registered
  nothing, so it could not hold the name either; the one usable agent was
  lost to a collision with an entry the registry never saw.
- A shared transcript listing whose directory walk takes longer than the
  rescan window is no longer pruned while the walk is still running. The
  second caller then claimed the same root, walked it too, and the slower
  walk published over the newer listing, so a session that exists read as an
  empty store.
- A working directory written with a trailing separator shows its last two
  components without the stray slash (`toktop` used to render "log/" where
  "log" belongs).
- A probe time of 999.6 ms reads as "1.00s" rather than "1000ms", the
  boundary rule the count and rate formatters already follow.
- `toktop` exits 2 when every ssh target it was given fails to attach,
  instead of starting a dashboard showing only local engines with the reason
  on a stderr line the alternate screen hides.
- A monitored engine address that fails, recovers, and fails again is
  reported to the operator again. The first report silenced every recurrence
  of the same message, including one that appeared after a real recovery.
- A relative `XDG_DATA_HOME` is named at startup while opencode's session
  database is read, and a relative `XDG_CONFIG_HOME` with an `ssh://` target,
  the way a relative `GAUNTLET_HOME` already was. Both fell back to the
  default directory in silence, so the paths they named were never used.
- `toktop update` strips a trailing newline from `GITHUB_TOKEN` (what
  `export GITHUB_TOKEN=$(cat token)` leaves behind) and refuses one that
  appears anywhere in the value, instead of letting the request fail on an
  invalid header that named the transport rather than the variable.
- A process listing that runs longer than the sampler's refresh window no
  longer admits a second one behind it. On Windows the CIM enumeration costs
  more than the window, so every caller arriving while it ran swept the same
  process table, and whichever finished last published the older listing and
  a CPU tick baseline stamped before a newer one.
- A reloaded agent definition is followed by the poll that read it. The
  transcript listing a watcher reused for up to a second named the roots and
  suffixes the previous definition had, so a redirect was not walked until
  that window expired, and the tree the spec had just disowned kept being
  read in the meantime.
- An `ssh://` target that stops answering says so. A failed poll kept the last
  good sample and said nothing, so once that sample went past the staleness
  window the ssh readings dropped out of the frame and the local host's numbers
  passed for the watched one's. The header now names the target and the reason
  it stopped answering, and `--once --plain` prints the same after `via ssh:`.
- `GET /health` on the ingest endpoint answers `503` with
  `degraded: <in-flight>/<max> event streams in flight; events are being
  refused` while every decode slot is held, instead of `ok` at a moment the
  endpoint is refusing every POST. The `503` body in a rejected POST now names
  the same number the probe reports.
- A rate or a count no longer picks its unit on the value before rounding.
  999.5 tokens/s rendered as a unitless `1000` and 999,500 as `1000k`, while
  a count of 999,950 rendered as `1000.0k`; the unit is now chosen on the
  rounded value, so those read `1.0k`, `999.5k` and `1.0M`, and one magnitude
  keeps one spelling across a boundary.
- A probe row on a narrow pane keeps at least the first eight characters of
  the model name. The column width went to zero around 62 columns, which
  dropped the model from both probe rows entirely and left two lines that
  named no model at all.
- A frame with no stamp on it no longer reports every agent as live. An
  unstamped snapshot measured each agent against a span of billions of years,
  which is negative, so every recency threshold passed and the whole feed read
  `live`. A frame with no instant to measure against now leaves the recency
  cell blank, and an agent last stamped in the future by a sender whose clock
  runs ahead reads idle rather than live.
- A generation error reported by an engine, and a version string answered by
  one, are capped and stripped of terminal escape sequences on every path. The
  recognized error shapes passed through as sent, and a `"version"` member was
  held verbatim for as long as the cache kept it, so an engine that answered
  with megabytes of text or an escape sequence reached the readout and the
  render width.
- JSON keys, `/proc` and CLI field names, unit suffixes and DNS host labels
  fold ASCII case only. `strings.ToLower` also folds runes whose lowercase
  form is ASCII, so a key spelled with U+0130 (which lowercases to `i` plus a
  combining dot) or U+212A (KELVIN SIGN, which lowercases to `k`) satisfied a
  match its producer never wrote, which for a sensor key is a value the
  operator did not measure.
- `toktop update` asks the release host for identity bytes when it downloads
  the binary, as it already did for the checksums archive. A transport that
  transparently decompressed the body would write the wrong bytes and hash
  bytes nobody else hashed, and the check would fail on a good release.
- A result piped to a reader that exits before the write finished exits `0`,
  as `--help` documents. The Go runtime re-raises `SIGPIPE` with its default
  disposition for a write to stdout, so `toktop version | true` died of signal
  13 (141 to a shell) instead of returning the `0` that the broken-pipe
  handling in the same binary produces for every other early reader.
- `toktop --help` and `toktop update --help` list every flag in the long
  `--long-form` the examples, the prose and the README already use, and name
  its argument with the same word the README's flag table does (`--add URL`,
  `--seed N`). Go's flag package printed its own single-dash spelling
  (`-add value`, `--seed int64`) and gave `-h` a line of its own beside
  `--help`, so the flag list read as a different CLI from the screen around
  it.
- A flag parse error names the flag the same way. `toktop --bogus` said
  "flag provided but not defined: -bogus" and `toktop --interval 1` named
  `-interval`, spellings shown nowhere else; both now use `--`. A boolean
  given a non-boolean value also gains the "flag" the other messages already
  had.
- A dsh session log whose record straddles a Zstandard frame boundary is read
  as one record. The read stopped at frame boundaries and counted each half as
  its own line, so neither parsed and both were dropped, under-reporting the
  turn. An unterminated trailing record is now held over and re-parsed with
  its head on the next poll, the way the plain JSONL path already did.
- The ssh host-key store's stale-lock break no longer spins. A lock too old to
  belong to a live process was removed and retried immediately, skipping the
  wait deadline, so a lock that could not be unlinked kept the loop running
  with no sleep and no way out. The break is retried only once the removal
  actually took, and the deadline and the poll gap hold on every path.
- A `CUDA Version:` line is read from `/proc/driver/nvidia/version` whether it
  sits before or after the `Driver Version:` line. The scan for it was gated on
  the driver line having already been seen, and separately returned at the
  driver line without looking further, while `CUDA Version:` is a trailing line
  of its own in some driver builds, so a driver that wrote it in either order
  reported its version with the CUDA row silently missing.
- An event `id` (or a derived `Idempotency-Key` id) stays deduplicated for 15
  minutes instead of only while the event sits in the 512-event display ring.
  A sender whose POST was retried after the ring moved on found its ids
  evicted and had every line of the replay counted a second time. The ledger
  is bounded by that horizon and by a count cap, so an id ages out rather than
  pinning a key forever.
- An `ssh://` host-key pin store that holds no records at all is refused
  instead of read as an empty one. A file truncated to nothing, or one a backup
  or a dotfile manager restored empty, read as "nothing pinned yet" and
  re-trusted every host on the next connect with nothing in the output. The
  error names the file and how to recover; deleting it is how the re-trust is
  asked for on purpose.
- `toktop update` flushes the directory the new binary was renamed into, the
  way the pin store already did. A crash after an update that reported success
  could otherwise leave the previous version installed, with the message
  saying otherwise.
- `make site-rollback` runs once. `wrangler rollback` with no version undoes
  the most recent deployment whoever shipped it, so a second run rolled back a
  rollback and put the version that broke back on the site. A deploy that
  reported success now records `dist/site.deployed` and a rollback moves it to
  `dist/site.rolled-back`, so a second rollback finds nothing of this tree's
  to undo, says so, and exits 0 without calling wrangler.
- A tool toktop shells out for a deadline no longer leaves what it spawned
  running past the deadline. The GPU vendor CLIs, the macOS process listing,
  and the macOS agent discovery tools each run on a poll, and the kill that
  fires when a tool hangs reached only the process toktop started: a
  grandchild was reparented to init and kept running, one more per poll for as
  long as the dashboard was up. Each of those commands now runs in its own
  process group and is killed as a group.
- `make site-rollback` waits for `/health` before it reports success, the way
  `make site-deploy` already did. A rollback that restored a Worker which
  never came up exited 0, so the only signal that the site was down was a
  visitor.
- `toktop update --check` writes the release URL and nothing else to stdout,
  so `url=$(toktop update --check)` is a URL whether or not this build is
  already current. The "New release" and "is current" lines moved to stderr,
  where the update help already said progress belonged.
- `toktop update` reads the checksums tar.gz the way the file is, not through
  a transport coding. A host that served it with a `Content-Encoding: gzip`
  of its own had the archive's own gzip stripped before the checksum list was
  read, so the update failed with `gzip: invalid header` on a release that was
  perfectly good.
- The agent feed API section of the README names the `503` and its
  `Retry-After: 1` that a POST gets while 64 bodies are already decoding.
  The cap was described only in the release notes, so a sender reading the
  endpoint contract had no status to handle and no backoff to honor.
- An `ssh://` target naming a directory as its key no longer prints the
  expanded path in the `--ssh-key` diagnostic. Every other ssh diagnostic
  already folds your home directory to `~`; that one branch spelled the path
  and so named the account in a line meant to be pasted into an issue.
- An ingested event's free-form note is stored with your home directory
  folded to `~`, the rewrite a watched agent's working directory already
  got. A harness that names where it is working no longer puts the account
  into the retained feed, the dashboard, or a `--once --plain` report
  redirected into a file.
- A probe no longer reports throughput thousands of times too high when an
  engine's `eval_duration` is plausible only in microseconds or milliseconds
  and the decode itself is fast. The unit fit kept the raw value whenever
  nothing scaled into the band, which read a microsecond report as
  nanoseconds; a decode that finishes more than four times faster than the
  request carrying it is now measured against the wall clock instead.
- A stream that fails mid-body says how many earlier events were recorded
  out of how many arrived, so a replay whose lines are all already in the feed
  is not told they were recorded.
- `$TOKTOP_COLUMNS` / `$TOKTOP_LINES` are no longer validated for
  `--once --plain`, which renders no sized frame. A set-but-invalid value
  exited 2 before the "has no effect with --plain" line that names the
  variable, so a value the run never reads could still refuse it.
- `$TOKTOP_COLUMNS` / `$TOKTOP_LINES` set without `--once` are named as
  having no effect without `--once`, the reason that applies. `--plain` on
  its own already says it has no effect there, and no text report is ever
  produced.
- A flag written after an `ssh://` target says it has to come before the
  targets. Flag parsing stops at the first positional, so `toktop ssh://box
  --agents` reported only "unexpected argument", with nothing to change.
- An engine's version is asked again after ten minutes instead of once per
  session. A container re-pulled, an engine upgraded, or a remote host behind
  the ssh forward restarted with a new image left the version readout showing
  the old one until the dashboard was quit and relaunched. The last known
  version is kept while the engine is unreachable, so a restart does not blank
  the row.
- A vendor GPU CLI that moves or is removed is resolved again after ten
  minutes. A cached path was executed for the rest of the session, so a driver
  or container reinstall left the GPU row empty until the next start.
- A release no longer publishes whatever else was in `dist/`. The publish step
  uploads every top-level file it finds there, so a coverage profile from an
  earlier `make cover`, or a note left by a previous local release, rode along
  as a release asset. `make release` now purges those first.
- The tag push compares the bytes it is about to ship. The reproducibility gate
  only ran on pull requests and pushes to main, so a release built bytes nobody
  had diffed; the release job now builds two platforms twice before publishing.
- `POST /v1/events` bodies decoded at the same time are capped, and one past
  the cap is refused with `503` and a `Retry-After` instead of read. The
  endpoint answered every connection it accepted, and a body that stopped
  mid-stream held its handler until the idle deadline, so a peer opening
  connections and withholding bodies could hold a descriptor and a goroutine
  each for as long as it liked.
- An ssh failure no longer reports the home directory. A refused key, an
  unreadable host key store or a changed host key named the absolute path it
  worked on, and `$HOME` names the account; those paths now read `~/...`, as
  a failed `toktop update` already did. A malformed agent definitions file
  reports the same way.
- `toktop help update extra` and `toktop help version extra` name the
  subcommand whose help is wanted instead of pointing at the top-level
  screen, the same as every other leftover. `help` and `version` passed
  after an ssh:// target say the subcommand has to come first, like `update`
  already did.
- The help overlay is no longer built through a clipping helper that no longer
  exists; opening `?` on a small pane rendered nothing.
- A transcript that was on disk but had gone idle for more than two minutes
  when `--agents` attached is no longer read from its first byte. Attach now
  records where every existing transcript ends, not only the recently written
  ones, so the next append reports only the growth; a cumulative adapter
  (codex) previously reported the whole session total as new output.
- A host key store whose record does not parse, or that records one host twice
  with different keys, is refused instead of read as a shorter store. Skipping
  the bad line turned corruption, or a line appended to the file, into a
  silent re-TOFU for that host on the next connect.
- Two `toktop` processes adding hosts at once no longer lose a pin. The ssh
  host key store takes a lock file around the whole read-and-write, not just
  the write, and a lock left behind by a killed process is broken rather than
  waited on. The store's rename is flushed to disk, so a crash cannot revert
  to the previous one and drop the pin just added.
- The error a mid-stream `POST /v1/events` failure returns now says how to
  recover from it. A stream whose events carry their own `id`, or that was
  sent without `Idempotency-Key`, is resumed by sending the remaining lines.
  A stream under `Idempotency-Key` whose events omit `id` must be replayed in
  full: a resumed POST numbers its first line 1 again, so its events take the
  ids of lines already recorded and are dropped as duplicates.
- `toktop update` and the `ssh://` host key store sweep their staging files
  before writing, so a run killed between staging a file and renaming it does
  not leave a partial file in the install directory or the config directory
  for good. A staging file young enough to be a download in flight is left
  alone.
- A transcript rewritten to a shorter length is counted once, not twice. The
  watcher re-read such a file from its start, and the records it had already
  billed were added a second time.
- A transcript rotated under a running watcher can lower an agent's reported
  totals, the same way the file itself now says. The dashboard records no
  negative growth, so the next reading is measured from what the transcripts
  actually hold.
- An `agentusage` session whose first header line was larger than the owner
  scan's old 4 MiB cap but no larger than the record path accepts stopped
  being read for the life of the dashboard: the scan returned "undecided",
  and every poll after it bailed. The scan now buffers the same line cap the
  record path uses.
- Agent discovery matches a process name under Unicode NFC, the same
  normalization the agent registry uses, so a binary whose name the
  filesystem stores decomposed (a macOS `café` as `cafe` + U+0301) is found by
  an agent registered precomposed. Before, the running agent never appeared.
- A byte count under 1 MiB renders in KiB (`900KiB` in the panes, `900K` in
  the compact system strip) instead of rounding to a flat `0MiB` or `1M`,
  which read as no allocation at all for a small `size_vram` or a small
  process.
- An engine-reported `eval_duration` is normalized against the measured round
  trip before tokens/s is computed. The field is a bare integer with no unit
  on the wire: Ollama sends nanoseconds, but llama.cpp-derived and several
  gateway builds send microseconds or milliseconds, and those readings came out
  1000x or 1e6x slow.
- Agent token totals saturate instead of wrapping. One event carrying an
  absurd transcript accumulator no longer turns that agent's windowed prompt,
  output, and thinking totals negative.
- On macOS, memory used is capped at memory total (compressed pages are
  already counted inside active and inactive), the `ps(1)` RSS shift and the
  page sums saturate, and a negative or infinite load average reads as zero
  instead of printing `ld -0.42`.
- Over `ssh://`, a kernel-chosen loopback port that collided with a remote
  port still to be forwarded could point one engine at another engine's relay.
  The ephemeral bind now rebinds until the pick is clear of the forwarded set,
  and a duplicate port in the forward set is bound once.
- A panic raised while parsing an agent's transcript releases the watcher's
  locks instead of leaving one held with no goroutine left to unlock it, and
  holding `p` dispatches the probe as a program-owned command rather than
  spawning an untracked goroutine per key press.
- A `zstd` transcript window no longer decompresses without bound across its
  frames. The per-decode cap reset on each frame, so a window of small RLE
  frames (about 10 compressed bytes yielding 128 KiB each) grew the
  accumulator to tens of gigabytes with every individual frame inside the
  cap. The walk stops at the cap and the rest is read on the next poll.
- A `hwmon` name or thermal-zone type is stripped of escape sequences before
  it reaches the terminal. Those are file contents on a host toktop may not
  own, and they were stored as read.
- The `ssh://` host-key store's lock is real on a first connect. The lock file
  lives beside the store and the store's directory is created by the write
  inside the lock, so the exclusive create failed with `ENOENT` instead of
  `EEXIST` and the unlocked fallback ran: two first contacts racing on a
  fresh install, the loser renaming away the winner's pin, and the next
  connect re-trusting that host.
- An `ssh://` handshake that stalls stops when the run is cancelled. The
  transport observes no context of its own and the connection deadline is
  lifted once the version banner arrives, so a peer that sends the banner and
  then waits held the dashboard unkillable by Ctrl+C.
- An `ssh://` target named more than once is attached once. The second
  spelling opened its own connection and forwarded the same remote ports onto
  a second set of local listeners, so that host's engines appeared twice under
  different local addresses and the totals added one engine's tokens to
  themselves. Forwarding a port that is already forwarded now reports the
  local port it has instead of binding a second listener.

## [0.14.1] - 2026-09-26

Binaries, checksums, and a CycloneDX SBOM are on
[GitHub Releases](https://github.com/maci0/toktop/releases/tag/v0.14.1).

### Changed

- The bundled SQLite driver (`modernc.org/sqlite`) is 1.59.0.

## [0.14.0] - 2026-09-26

Binaries, checksums, and a CycloneDX SBOM are on
[GitHub Releases](https://github.com/maci0/toktop/releases/tag/v0.14.0).

### Added

- `toktop update --help` shows subcommand help, and `toktop help` and
  `toktop version` accept standard Go help flag variants (`-h`, `--h`,
  `-help`, `--help`).
- Web site accessibility improvements: skip-to-content navigation link,
  landmark regions, motion preference checks for smooth scrolling, and
  focus-visible styling.
- Transcript lines, agent definition files, and `POST /v1/events` bodies may
  begin with a UTF-8 BOM; the BOM is ignored instead of failing the parse.
- Agent identifiers, session directories, and derived event IDs are compared
  under Unicode NFC normalization, so NFC and NFD spellings of the same name
  no longer split one agent into two or defeat duplicate detection.

### Changed

- SBOM generation omits timestamps and serial numbers for byte-reproducible
  CycloneDX output, and the SBOM is generated as part of `make release`.

### Fixed

- `toktop` treats broken pipes (`EPIPE` and `io.ErrClosedPipe`) on stdout as
  clean exits with code 0 instead of reporting write errors when piped to
  utilities like `head` or `grep`. On Windows, `ERROR_BROKEN_PIPE` and
  `ERROR_NO_DATA` are treated the same way.
- Prevented potential cache aliasing and stale state by cloning cached
  slices in process and system monitoring, pruning expired transcript
  listings on cache hits in `agentusage`, and clearing cached KV percentages
  when a provider model is unloaded.
- Hardened input validation and credential handling across packages: reject
  mixed-script identities in `agentwatch` to prevent homograph spoofing,
  disallow CRLF in bearer tokens to prevent HTTP header injection, strip
  authorization headers and reject userinfo on `toktop update` HTTP
  redirects, and validate remote target hostnames and user names.
- Fixed baseline seeding for zstd-compressed dsh session logs in
  `agentusage`.
- A quoted `ssh_config` argument is read without its quotes, so a Windows
  `IdentityFile` under a user directory with a space in it (`"C:\Users\a
  b\key"`) is found instead of being looked up with the quote characters
  still attached.
- `toktop update` failures redact the home directory whether the message
  spells it the way `USERPROFILE` does or the way the process that named it
  wrote it, so the tilde rewrite also lands on Windows and macOS.
- Added integer overflow saturation and handled NaN/infinite values in
  duration, memory, rate, and UI percentage conversions.
- Safeguarded background ticker loops against non-positive intervals to
  prevent invalid timers.

## [0.13.0] - 2026-09-22

Binaries, checksums, and a CycloneDX SBOM are on
[GitHub Releases](https://github.com/maci0/toktop/releases/tag/v0.13.0).

### Changed

- Frame rendering does one agent-rate pass instead of two where both
  directions are needed, filters via-engine events in place instead of
  copying the feed, and pre-sizes the chart series buffer. Rendered output
  is unchanged.
- The `!sqlite` build stubs for the database-backed agent sources live in
  one file instead of two. No behavior change.

## [0.12.0] - 2026-09-19

Binaries, checksums, and a CycloneDX SBOM are on
[GitHub Releases](https://github.com/maci0/toktop/releases/tag/v0.12.0).

### Added

- The dashboard opens on the agents view when `--agents` is on and no engines
  were found, instead of the setup card complaining about engines nobody asked
  for. `a` toggles which side gets the panel estate: engines (the default) or
  agents. The side not in focus keeps the header, the shared throughput chart
  and the host strip, so neither half of the machine disappears. The compact
  strip for panes too small for the dashboard drops the missing-engines line
  on an `--agents` run too.

## [0.11.0] - 2026-09-19

Binaries, checksums, and a CycloneDX SBOM are on
[GitHub Releases](https://github.com/maci0/toktop/releases/tag/v0.11.0).

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

## 0.1.0 to 0.4.5

These releases predate this file, so nothing here describes what changed for
you across them, and no migration notes exist for that range. Each release
page, from
[v0.1.0](https://github.com/maci0/toktop/releases/tag/v0.1.0) through
[v0.4.5](https://github.com/maci0/toktop/releases/tag/v0.4.5), carries notes
auto-generated from its commits, and the diff between the tag you run and the
tag you want is the record of what moved. The README and `--help` of the tag
you upgrade to are the CLI contract for that version; this file covers 0.5.0
and later only.

[Unreleased]: https://github.com/maci0/toktop/compare/v0.19.0...HEAD
[0.19.0]: https://github.com/maci0/toktop/compare/v0.18.2...v0.19.0
[0.18.2]: https://github.com/maci0/toktop/compare/v0.18.1...v0.18.2
[0.18.1]: https://github.com/maci0/toktop/compare/v0.18.0...v0.18.1
[0.18.0]: https://github.com/maci0/toktop/compare/v0.17.1...v0.18.0
[0.17.1]: https://github.com/maci0/toktop/compare/v0.17.0...v0.17.1
[0.17.0]: https://github.com/maci0/toktop/compare/v0.16.0...v0.17.0
[0.16.0]: https://github.com/maci0/toktop/compare/v0.15.0...v0.16.0
[0.15.0]: https://github.com/maci0/toktop/compare/v0.14.1...v0.15.0
[0.14.1]: https://github.com/maci0/toktop/compare/v0.14.0...v0.14.1
[0.14.0]: https://github.com/maci0/toktop/compare/v0.13.0...v0.14.0
[0.13.0]: https://github.com/maci0/toktop/compare/v0.12.0...v0.13.0
[0.12.0]: https://github.com/maci0/toktop/compare/v0.11.0...v0.12.0
[0.11.0]: https://github.com/maci0/toktop/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/maci0/toktop/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/maci0/toktop/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/maci0/toktop/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/maci0/toktop/compare/v0.6.1...v0.7.0
[0.6.1]: https://github.com/maci0/toktop/compare/v0.6.0...v0.6.1
[0.6.0]: https://github.com/maci0/toktop/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/maci0/toktop/compare/v0.4.5...v0.5.0
