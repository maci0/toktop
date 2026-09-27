# toktop

`btop` for AI: a terminal dashboard for LLM inference engines and the agents
hammering them.

<p align="center">
  <img src="docs/images/dashboard.png" alt="toktop dashboard" width="900">
</p>

```
toktop --demo            # simulated fleet, works instantly
toktop                   # auto-discovers local engines (ports + processes)
toktop --agents          # also watch the coding agents on this machine
toktop ssh://user@box    # watch engines on another host
```

## Install

```
CGO_ENABLED=0 go install -tags sqlite github.com/maci0/toktop/cmd/toktop@latest
```

On Windows the leading assignment is POSIX shell syntax and does not run in
`cmd.exe`; PowerShell spells the same command:

```
$env:CGO_ENABLED=0; go install -tags sqlite github.com/maci0/toktop/cmd/toktop@latest
```

Building from source needs Go 1.27, the version `go.mod` pins. A
downloaded release binary needs nothing but the platform it was built for.

`-tags sqlite` matches the GitHub binaries and `make build`: crush and
opencode session databases cannot be read without it. `CGO_ENABLED=0`
matches those artifacts too (pure-Go net resolver, no libc); a host with
gcc would otherwise produce a cgo-linked binary. Or download a binary
for linux, macOS, and Windows (amd64 + arm64) from the
[releases](https://github.com/maci0/toktop/releases). An installed binary
updates itself in place:

```
toktop update --check    # report the latest release, change nothing
toktop update            # download it and replace the running binary
```


## AI coding agents

toktop also watches the coding agents running on this machine, not just
inference engines. It finds them by process, reads the token counts they
already write to their own session logs, and shows their throughput beside the
engines:

```
AGENTS  local, read from their own session logs
  claude   ▲ 1.1k tok/s   ▲2.4k ▼8.1k   ● live
  codex    ▲ 340 tok/s    ▲18k ▼40k     via 127.0.0.1:11434  ● live
```

`--agents` turns this on. It is off by default because it means scanning this
machine's processes and reading session files nobody pointed toktop at:
watching engines you configured does not imply consent to that. The watch
needs each process's working directory to attribute transcripts; Windows
does not expose that through a documented API, so `--agents` finds no
agents there.

With no engines attached, agents mode is the full dashboard: header rates,
throughput charts and the host strip, all driven from the session logs.
Prompt, output and (when the agent reports it) reasoning tokens are shown
per agent. An agent generating through an engine toktop is already measuring
still appears in the list, labelled `via <engine>`, but those tokens are not
added on top of the engine's own numbers.

Once asked for, nothing else has to be configured and the agent does not have
to cooperate: claude, codex, qwen, copilot, pi, prime-agent, feynman, clanker,
crush, opencode, and dsh all keep records carrying the provider's own counts
(JSONL transcripts, except opencode and crush which keep SQLite stores).
dsh's
default log is concatenated zstd frames (`session.v<N>.jsonl.zstd`, or
`session.jsonl.zstd` for generation zero); uncompressed JSONL is read too.
Agents that report nothing show no rate rather than a zero.

Two agents keep databases instead of transcripts, and both need the `sqlite`
build tag, which decides whether a driver is compiled in at all (released
binaries and `make build` carry it; `make build TAGS=` leaves it out).

opencode keeps one session store for the whole machine, so it is gated twice:
the tag links the driver, and `--opencode-db` decides whether a binary that has
it opens the operator's database. `--opencode-db` is on by default with
`--agents`; pass `--opencode-db=false` to leave the store alone. A build
without the driver reports nothing for opencode, and asking for it explicitly
says so on stderr rather than reporting a silent zero.

crush keeps its database inside the project it is working on
(`.crush/crush.db`, at the project root it resolves), with
`sessions.completion_tokens` as output and `sessions.prompt_tokens` as
prompt. The store is already scoped to that project, so there is no second
gate: with the tag, it is read. The only JSONL crush writes is its log,
which carries no counters.

Reading is done by this repo's own `agentusage` package, which gauntlet also
imports, so both tools report the same numbers. Agents defined in
`~/.gauntlet/agents.json` are picked up here too; a malformed file is reported
at startup rather than silently shrinking the watch to the built-in agents.
The file and each agent entry must be JSON objects, not `null`; use `{}`
for an empty definitions file. Invalid files leave the loaded registry unchanged.
Set `GAUNTLET_HOME` to read that file from somewhere else; a value that is not
an absolute path is ignored and named at startup. Only the `usage`
block matters here (launch fields are ignored):

```json
{
  "myagent": {
    "usage": {
      "roots": ["~/.myagent/sessions"]
    }
  }
}
```

`roots` are searched directories, `suffix` filters the files under them
(default `.jsonl`), `suffixes` does the same for an agent that writes more
than one extension, `cumulative` marks counters that already include
everything before them, and `header_cwd` says the working directory appears
once in a session header rather than on every record. A root may contain
`{dir}`, which stands for the agent process's working directory, for an agent
that keeps its transcripts inside the project it works in:

```json
{
  "myagent": {
    "usage": {
      "roots": ["{dir}/.myagent/sessions"]
    }
  }
}
```

### Using the Go package

```
go get github.com/maci0/toktop/agentusage
```

It needs Go 1.27, the version `go.mod` pins, and the `sqlite` build tag if you
want crush and opencode (`go build -tags sqlite`); without it the package still
compiles and `Supported` reports those two unreadable. Import
`github.com/maci0/toktop/agentusage` to discover agent processes and read the
token counts they already write:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/maci0/toktop/agentusage"
)

func main() {
	path := agentusage.DefinitionsPath()
	if err := agentusage.LoadDefinitions(path); err != nil {
		fmt.Println(err) // malformed or unreadable; a missing file is not an error
	}
	if !agentusage.EnableOpenCodeDB(true) {
		fmt.Println("opencode: build without -tags sqlite")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, p := range agentusage.Discover() {
		w := p.Watch(time.Now())
		if w.Err() != nil {
			continue
		}
		wg.Go(func() {
			var prev agentusage.Sample
			w.Run(ctx, agentusage.DefaultPollInterval, func(cur agentusage.Sample) {
				if d, ok := cur.Delta(prev); ok {
					fmt.Printf("%s pid %d: %d output, %d prompt\n", p.Tool, p.PID, d.Output, d.Input)
				}
				prev = cur
			})
		})
	}
	wg.Wait()
}
```

The example watches the discovered processes concurrently for ten seconds.
Only usage written after attachment is reported; existing transcript counts
are skipped. Keep an agent generating during that window to see output.

Two rules the example follows, because both are easy to get wrong and neither
is enforced by the types. `Watch` returns a nil `*Watcher` for an agent that
keeps nothing readable, and every method on that nil is safe to call, so the
test is `w.Err()` rather than the pointer: `Err` names the case, `Tool` and
`Dir` return empty strings, and the error is `ErrUnsupportedTool` under
`errors.Is`. And `Run` hands over a running total, not the interval's usage, so
each callback reports `cur.Delta(prev)` and keeps `cur` as the next baseline.
Printing the totals instead bills the same tokens once per poll. A delta that
reports no growth is still the baseline to carry forward: the agent was quiet,
or its transcript was rewritten under the watcher.

`RegisterSpec` teaches the package about an agent it was not compiled to know,
and `UnregisterSpec` takes it back, restoring the adapter it displaced. The
same goes for a definitions file: `LoadDefinitions` adds to the process-wide
registry and `ResetDefinitions` drops everything it added, leaving the
compiled-in ones. A test that loads a definitions file (or registers a fake
agent) needs that undo, since every later test in the same binary inherits it
otherwise; the package's own `ExampleRegisterSpec_fakeAgent` is that pattern
end to end, writing the transcript after the watcher attaches because records
already on disk belong to an earlier run. `errors.Is` matches `ErrEmptyTool`
and `ErrNoRoots` on a rejected spec, `ErrInvalidDefinitions` on a malformed
definitions file, and `ErrCollidingDefinitions` on two agent names in one file
that reduce to the same key; a file that exists but cannot be read
returns the wrapped `os` error instead, matching neither, and a missing
one is not an error. `SpecFor` is the read side: it reports the transcript
location registered for an agent, roots as written, which is how a program
finds out which entries a definitions file registered and which it skipped
(the agents read by a compiled-in adapter are not reported, so a `usage` entry
naming one is skipped on load rather than registered). Use `RegisterSpec` to
read such an agent elsewhere. A definition does replace a compiled-in
*definition*, pi, prime-agent and feynman. `Supported` covers every agent.
`Watch` returns
a nil `*Watcher` when an agent keeps nothing readable; `Watcher.Err` says so,
and matches `ErrUnsupportedTool`. Every method on it is safe to call, so a
caller tests `w.Err() != nil` rather than the pointer. A sample is the total
since the watcher attached, so a program reporting events takes the growth
between two of them from `Sample.Delta`, which reports nothing when a
transcript was rewritten under the watcher rather than a negative count.
`Rate` is output
tokens per second between two samples; `InputRate` is the same for billed
prompt tokens, and `ThinkingRate` for the reasoning share. All three report
whether a rate could be computed at all, and none of them extrapolates from
one reading. `Watcher.SetNow` replaces the clock that stamps published
samples, so a program driving a simulated timeline gets samples stamped on
it, and the recency and rescan windows age on that clock rather than on wall
time. Transcript mtimes and `since` stay wall time, because that is the clock
the filesystem and the session stores record in; anchor the injected clock to
the instant the run started and the two agree.

`Agents` lists every agent name the package knows (built in, defined, or
registered), and `Supported` says whether one of them can be read here. A
transcript store that cannot be walked is reported rather than read as empty,
on the process logger from `log/slog` unless `SetLogger` is handed the logger
the embedding program already writes to (nil restores the default).

A
dashboard also needs to know when an agent's tokens are already being counted
by an engine it watches: `Peers` lists the TCP endpoints a process is
connected to, `ConnectedTo` answers that for one process, and
`MatchingEndpoints` maps many processes to the first of a set of endpoints each
holds a connection to, reading the kernel's connection tables once. An empty
result from any of them means "cannot tell", which reads as "not connected".

## What it shows

- **Engines** - every engine found locally or via ssh, with model, version,
  KV-cache pressure, queue depth and throughput. Fingerprinted kinds:
  Ollama, llama.cpp/llamafile/ramalama, vLLM, SGLang, TRT-LLM/Triton,
  LM Studio, MLX (mlx-lm / LM Studio), KoboldCpp, LocalAI, TGI, LiteLLM,
  GPUStack, Lemonade, OmniRoute (auto-detected via its routing header;
  per-model context windows shown) - plus a generic OpenAI-compatible
  fallback so nothing is left out (text-generation-webui and TabbyAPI are
  discovered by process/port but identified as generic OpenAI).
- **Throughput charts** - aggregate decode + prompt tokens/sec as heat-colored
  area charts; engine-published tok/s gauges are trusted when present.
  Agents with no local engine (or whose engine is not monitored) add their
  own rates; tokens already counted by a watched engine are not added again.
- **Probes** (`p`, `--probe N`) - tiny streaming generations measuring real
  TTFT and decode speed per engine.
- **Agent feed** - any harness can POST usage events:
  ```
  curl -X POST localhost:8420/v1/events \
    -H "Idempotency-Key: coder-turn-1042" -d \
    '{"agent":"coder","kind":"tool","prompt_tokens":4200,"output_tokens":310,"thinking_tokens":40,"note":"shell(git status)"}'
  ```
  The key names one turn, so a resend after a lost answer is counted once. A
  key built from the clock (`$(date +%s)`) is a different key on every
  attempt, which is a fresh operation to the feed: the retry counts again.
  `--agents` also fills this from local session logs. Per-agent rows show
  output, prompt and reasoning rates; an agent using a monitored engine is
  labelled `via` that engine so its tokens are not added twice.
- **System strip** - RAM/swap/load, CPU model, OS+kernel, GPU driver versions
  (incl. CUDA), NPU enumeration (Intel NPU, AMD XDNA NPU, Qualcomm Cloud
  AI100, Apple Neural Engine with chip generation), GPU temp/util/VRAM/
  power - on Apple Silicon including live wired-memory and
  utilization from IOAccelerator - and a second identity row for sensors.
- **Braille charts** - dot-matrix rendering with btop-style fading bloom;
  timescale compresses leftward (`t` toggles) with faint grid marks showing
  where each doubling begins.
- **Agents view** - `--agents` with no engines found opens on the agents
  themselves instead of the engines setup card, and `a` swaps the panel
  estate between engines and agents on a machine running both; the side not
  in focus keeps the header, the shared throughput chart and the host strip.
- **Hot reload** - on Unix, rebuild the binary while it runs and toktop
  re-execs into the fresh build (`--no-hot-reload` to disable). Windows
  cannot replace a running image: the dashboard exits and asks you to
  start it again.

## Agent feed API

The ingest server runs by default on `127.0.0.1:8420` (`--ingest ADDR` to
move it, `--no-ingest` to turn it off) and speaks plain HTTP/JSON:

| endpoint | purpose |
|---|---|
| `POST /v1/events` | record events; body is one JSON object or an NDJSON stream |
| `GET`, `HEAD` `/healthz` | liveness probe, answers `ok`; `503` with `Retry-After: 1` naming the in-flight count while every event slot is held |

Event fields are all optional; anything omitted gets the default:

| field | type | default | notes |
|---|---|---|---|
| `id` | string | - | caller-chosen key, at most 128 characters; an id past the cap, or one that is nothing but whitespace or control characters, is a `400` naming the field rather than a truncated key, because the id is what the feed deduplicates on and two keys clamped onto one stored id would drop the second event as a duplicate. A repeat of a key recorded within the last 15 minutes is ignored. When omitted, a request `Idempotency-Key` header is used: the first eight bytes of its SHA-256 hash, encoded as 16 hexadecimal characters, followed by the 1-based line index (`<hash>:1`, `<hash>:2`, and so on). The handler hashes the received key NFC-normalized, without truncation or whitespace collapsing; hash collisions remain possible |
| `ts` | RFC 3339 string | arrival instant | offset required (`2026-01-02T03:04:05Z`); stamps more than two minutes ahead of arrival are clamped to the arrival instant |
| `agent` | string | `anonymous` | capped at 64 characters |
| `model` | string | - | capped at 128 characters |
| `kind` | string | `turn` | known kinds: `turn`, `tool`, `error`, `note`; custom kinds pass through lowercased, capped at 24 characters |
| `prompt_tokens` / `output_tokens` / `thinking_tokens` | integer | `0` | negative values and values above 2^40 clamp to `0`; a whole JSON number such as `100.0` counts; a count outside the 64-bit integer range is a `400` naming the field instead of a clamp; thinking is the reasoning share of output when the agent says so |
| `via_engine` | string | - | monitored engine already counting this output; aggregates skip the event; capped at 128 characters |
| `note` | string | - | free-form, capped at 512 characters; a note that is nothing but a directory is reduced to its last two components, with a path under `$HOME` folded to `~`, so client and project names above the checkout never reach the feed |

One POST answers `202` with `{"accepted":N,"stored":M}` once every event in
the stream is decoded, where `accepted` is what the wire carried and
`stored` is what the retained feed took. A replayed event (an id already
recorded within the last 15 minutes) decodes fine and stores nothing, so the
two counts differ
on a retry after a lost 202. So does an event stamped behind the whole
retained window: the feed holds the newest 512 events, and one older than all
of them would be dropped before any consumer read it. `ts` is not clamped
backwards, so a sender whose clock runs behind sees the gap on the same
count. The same pair is on the POST's log line.
Other statuses: `400` for malformed JSON, a bad `ts`, a token count outside the
64-bit range, or an `id` that cannot be stored whole, `408` when a stream
stalls mid-body (the body names which bound broke: no bytes for a minute, or
the 10 minute lifetime), and `413` past the 1 MiB body cap. `503` with
`Retry-After: 1` means 64 bodies were already decoding, which is a pile-up
and not a fault: wait the named second and resend the same request, under the
same `Idempotency-Key` if it had one. A POST carrying an
`Origin` header (browser-driven; scripts and agents never send one) is
refused with `403`, so a web page cannot forge rows into a running
dashboard. Wrong methods on these paths answer `405` with `Allow` and a
body naming the path and the methods it takes.
Unknown paths answer `404` naming the two endpoints, so a POST to `/events`
is not a generic not-found page. Error bodies are short plain-text reasons
that name the field or expected shape, and a malformed line in a stream also
names the body offset it failed at; unknown fields are ignored, so
harnesses can include their own. The request `Content-Type` header is not
checked: the body is always read as JSON/NDJSON, so plain `curl -d` works
unmodified.
Every POST is logged to stderr as one structured line (`req`, `method`,
`path`, `status`, `accepted`, `stored`, `duration`, `remote`; failures add
`error`).
Wrong-method and unknown-path requests log the same way, so a harness
posting to `/events` is not silent. `GET /healthz` is not logged. It answers
`503` with `Retry-After: 1` and a one-line reason while all 64 event slots are
held, because the endpoint is refusing every POST then and `ok` would describe
a service that accepts nothing. A refused POST audits `in_flight` and
`slot_cap` beside the reason, so a run of them says how close the cap is, not
just that it was hit. Event
bodies are not logged. A handler panic is one ERROR
line with `req` and a single-line `stack`. An accept failure that ends the
endpoint writes one ERROR line naming the bound address and the reason.
Responses carry `X-Request-Id`, echoed from the request when the sender set
one.

Streams are recorded line by line: if a later line fails, events before it
stay recorded and the error states how many. Retrying a stream (or a
successful POST whose 202 was lost) is safe when each event carries a stable
`id`, or when the POST carries `Idempotency-Key` (filled in for events that
omit `id`). Without either, replaying the kept lines would duplicate them.
Such a replay answers `202` with `stored` below `accepted` and logs the
same pair, so a sender can tell the two apart.

Recovery after a mid-stream failure depends on how the events were keyed, and
the error says which one to use. Resume the stream (send the remaining lines)
when the events carry their own `id` or the POST had no `Idempotency-Key`.
Replay the whole request under the same `Idempotency-Key` when its events
omit `id`: a derived id is the key plus the line's position, so a resumed
POST numbers its first line 1 again and the feed would drop it as a
duplicate of an event it never sent.

An `Idempotency-Key` (and an event `id`) names one logical operation and
nothing else: mint it per operation, and namespace it per sender. Two POSTs
under the same key derive the same ids, so the second one's events decode
and store nothing: they show up only as `stored` below `accepted` on that
POST and on its log line, and the agent's token totals silently miss them.
A key is not a session, a turn counter, or a fixed string, and the server
cannot tell a retry from a second sender that picked the same one.

## Zero vendor libraries

Host vitals and engine stats come from procfs/sysfs/sysctl, vendor CLIs it shells out to
(`nvidia-smi`, `rocm-smi`, `xpu-smi`, `system_profiler`, `ioreg` - each the
vendor's documented interface with no in-process alternative) or plain HTTP
from the engines themselves. SSH transport is an embedded pure-Go client, so
remote monitoring needs no ssh binary either. No NVML, no Level Zero, no
cgo: single static binary, trivially cross-compiled.

Linux reads `/proc` + `/sys`; macOS uses sysctls and `system_profiler`;
Windows uses `GlobalMemoryStatusEx`, `RtlGetVersion` and one CIM query for
process command lines.

## Engines: how discovery works

1. Running **processes** matching well-known names (`ollama`, `llama-server`,
   `vllm`, `sglang`, `koboldcpp`, `lm studio`, `lemonade-server`, ...) give
   candidate URLs, honouring `--port` flags.
2. A scan of well-known ports follows (11434, 30000, 8000, 13305, 8080,
   1234, 5001, 5000, 4000, 1337, 4891, 7860, 20128, ...).
3. Each candidate is fingerprinted by its HTTP surface; anything unrecognized
   that still speaks OpenAI is shown as such.

Attach anything explicitly:

```
toktop --add http://10.0.0.5:8000        # repeatable
toktop ssh://user@host                   # remote engines + host vitals (no password in the URL)
```

SSH mode is built in (pure Go, no ssh binary needed) and the remote only
needs a POSIX shell - no agent is installed. Discovery reads the remote
`/proc` directly: listening sockets from `/proc/net/tcp(+6)` (with an active
port probe as fallback) plus engine processes with their `--port` flags, so
engines on custom ports are found just like locally. Engine traffic rides
ssh direct-tcpip channels on that same connection. Each remote engine port is
reached through a loopback listener bound to `127.0.0.1` with an ephemeral
port, so local clients attach the same way they would to a local engine;
those listeners are reachable by any process on this host. Host vitals
stream the same way: load, memory, uptime, CPU model, OS, kernel and GPU
rows (`nvidia-smi`, or `rocm-smi` on AMD boxes).

Auth tries, in order: `--ssh-key PATH`, keys from `~/.ssh/config`
(`HostName`, `User`, `Port`, `IdentityFile` are honored), your default
keys, ssh-agent, and finally a password prompt when stdin is a terminal
(or set `TOKTOP_SSH_PASSWORD` for headless runs). `ssh://user:pass@host`
is rejected: the password would sit in argv, and is not how auth is
configured. A path, query, or fragment on the URL is rejected rather than
ignored. Host keys use trust-on-first-use, stored at
`$XDG_CONFIG_HOME/toktop/known_hosts` (default `~/.config/toktop/known_hosts`);
a changed key is refused loudly. `SSH_AUTH_SOCK` selects the agent; on Windows
the OpenSSH named pipe is used when that variable is unset.

## Keys

| key | action |
|---|---|
| `q` / `ctrl+c` | quit |
| `esc` | close help / quit |
| `space` | pause / resume streaming |
| `p` | probe every engine with a real generation |
| `t` | toggle compressed timescale + grid |
| `a` | focus engines or agents (whichever gets the panel estate) |
| `?` / `h` | toggle help |

## Accessibility

toktop is usable without a mouse, without color vision, and with assistive
technology:

- **Keyboard only** - every action has a key (table above); nothing requires
  pointing or clicking, and `?` always shows the full key map.
- **Pause freezes everything** - `space` stops the streaming data and the
  header clock, so a still frame can be read at leisure with a screen reader
  or magnifier.
- **Status never rides on color alone** - down engines show `✗` plus their
  error text, probes show `✓`/`✗`, gauges print their percentage, and the
  engine count is spelled out numerically in the header.
- **Non-visual output** - `--once` prints one static frame instead of running
  the full-screen UI; a live-repainting dashboard defeats most screen readers,
  so the static frame is the intended path. Pair it with
  `TOKTOP_COLUMNS` / `TOKTOP_LINES` for a fixed size. `--once --plain` goes
  further and prints the same numbers as a linear text report: no braille
  chart glyphs (which screen readers announce as endless dot-pattern noise or
  skip entirely), no box-drawing borders, no multi-column panels - just the
  data in reading order.

  ```
  $ toktop --once --plain
  toktop v0.15.0

  5/5 engines up · out 1.5k tok/s · in 10k tok/s · 2 agents · session 24s

  ENGINES
  up   vllm-a100 (vllm)
         Qwen/Qwen2.5-32B-Instruct-AWQ
         out 155 tok/s · in 662 tok/s · kv cache 68% · running 2 · waiting 0 · ttft 115ms

  SYSTEM
  memory 64% (248G/384G) · swap 17% · load 5.06
  gpu nv0 A100-SXM4-80GB 82° 69% util vram 57G/80G 397W

  PROBES
  ok meta-llama/Llama-3.3-70B-Instruct-engine ttft 113ms 135 tok/s

  AGENT FEED
  ops-agent 293 tok/s · research-agent 247 tok/s
  03:29:23 note ops-agent model Qwen/Qwen2.5-32B-Instruct-AWQ prompt 9.2k output 783 note browser(search docs)
  ```

- **Tested contrast** - unit tests hold the palette to WCAG 2.2 AA: text
  colors at >= 4.5:1 on the background, and chart marks at >= 3:1 even at
  the deepest point of the age fade (`internal/ui/theme_test.go`).
- **No color** - `NO_COLOR` strips styling as usual; layout and text carry
  the same information without it.

## Flags

```
toktop update     subcommand: install the latest release (--check to only
                  report it, --repo owner/name for a fork). With --check,
                  stdout is the release URL and nothing else, or nothing at
                  all when the release names no GitHub release page, which
                  stderr then says
toktop help       same as --help; `toktop help update` / `toktop help version`
toktop version    same as --version
--demo            simulated fleet, zero setup
--add URL         attach an openai-compatible http(s) endpoint (repeatable,
                  once per endpoint; host required; no userinfo, query or
                  fragment, since the URL is echoed to the log and the
                  reports; use --bearer / $TOKTOP_BEARER)
ssh://user@host   positional; monitor remote hosts (repeatable;
                  ssh://[user@]host[:port] only, no password in the URL;
                  the host and user may not carry a bidi control,
                  zero-width or other format character, tag character
                  or variation selector, and a target that does is
                  refused at startup)
--ssh-key PATH    private key for ssh targets (overrides ~/.ssh/config;
                  ~ is expanded; an empty value aborts at startup, and a
                  missing file aborts only with a non-demo ssh:// target)
--bearer TOKEN    bearer token sent to --add endpoints only; OmniRoute API
                  keys etc. (env: OMNIROUTE_API_KEY, then TOKTOP_BEARER;
                  an explicit --bearer, even empty, wins)
--agents          watch AI coding agents on this machine (session logs)
--opencode-db     with --agents: read opencode's SQLite session database
                  (default on; needs a build with the sqlite tag; pass
                  --opencode-db=false to skip it)
--probe N         auto-probe every N seconds (0=off, max 86400; with
                  --demo, every N simulated seconds, so the run still
                  replays)
--interval D      poll interval (Go duration such as 1s or 500ms; default 1s;
                  min 50ms, max 1h; nonzero values require a unit)
--ingest ADDR     agent event listen address, host:port
                  (default 127.0.0.1:8420; empty is rejected)
--no-ingest       disable the event endpoint (`--ingest` is then ignored)
--once            render one frame and exit (use when piping or redirecting)
--plain           with --once: linear text report instead of the dashboard
                  frame (screen-reader friendly)
--json            with --once: print the final snapshot as one JSON object on
                  stdout instead of a frame, for scripts (--plain has no
                  effect alongside it; TOKTOP_COLUMNS / TOKTOP_LINES have no
                  effect with either)
--frames N        with --once: snapshots to accumulate before rendering
                  (max 180, the chart history length; with --plain and
                  --json only the wait before rendering changes)
--seed N          demo RNG seed; the demo frame shows the seed it ran
                  with, and the same seed replays the run
--origin TIME     with --demo: pin the instant the simulated timeline
                  starts at (RFC3339 or Unix seconds), so a seed and an
                  origin replay a run byte for byte; without it the timeline
                  starts at the wall clock, and the JSON report omits
                  `demo_origin`
--no-hot-reload   disable restart-on-rebuild while running
--version         print version and exit
--help, -h        show usage, examples and environment fallbacks
```

Password auth for ssh targets: interactive prompt, or `TOKTOP_SSH_PASSWORD`.

Flags come before the positional `ssh://` targets; a flag written after one is
a usage error that says so. Results (the rendered frame, the JSON report, the
version, the release URL) go to stdout and progress, warnings and errors to
stderr, so `toktop --once >frame.txt` and `toktop version` stay pipeable.
Exit codes: `0`
success, `1` runtime failure, `2` usage error, `130` interrupted (`--once`,
`toktop update`, and the live dashboard when SIGINT or SIGTERM stops it; the
dashboard's own `q` and Ctrl+C are a clean `0`).

## Scripting

`toktop --once --json` prints the last snapshot as one JSON object: the
aggregate throughput, every engine with its rates, queue depths and models,
the agent feed and per-agent rates, the probe samples and the host vitals.
It is the machine-readable counterpart of `--once --plain`, for a script that
wants the numbers rather than a report:

```sh
toktop --demo --once --json | jq -r '.engines[] | select(.ok) | .label'
```

The chart histories are not in it: they are the frame's own buffer, sized by
how long the process ran, so a consumer wanting a series should sample
`--once --json` at a steady `--interval` instead.

A demo run reproduces from its seed and its origin, the two inputs a replay
needs. With both, two runs render the same bytes:

```sh
toktop --demo --seed 7 --origin 2026-01-01T00:00:00Z --once --json --frames 5 \
  >run-a.json
toktop --demo --seed 7 --origin 2026-01-01T00:00:00Z --once --json --frames 5 \
  >run-b.json
cmp run-a.json run-b.json
```

Without `--origin` the timeline starts at the wall clock, so the values repeat
and the timestamps do not; the JSON report then omits `demo_origin`.

`--probe` keeps that promise under `--demo`: the auto-probe cadence is
simulated too, so how many probe waves ran and the instant each is stamped
follow the run rather than how long the process took.

## Environment variables

| variable | what it does |
|---|---|
| `OMNIROUTE_API_KEY` | bearer token fallback for `--bearer` (checked first unless `--bearer` is passed) |
| `TOKTOP_BEARER` | bearer token fallback for `--bearer` (checked after `OMNIROUTE_API_KEY`) |
| `TOKTOP_SSH_PASSWORD` | ssh password for headless runs; otherwise an interactive prompt. A trailing newline (from `$(cat file)`) is stripped, everything else is sent as typed. Set but empty is named rather than passed over: a headless run fails saying so, and a terminal run says it is prompting instead |
| `TOKTOP_COLUMNS` / `TOKTOP_LINES` | fixed frame size for `--once` output (screenshots, capture); must be 41-1024 / 21-512, and a set-but-invalid value aborts with exit code 2. `--once --plain` renders no sized frame, so both are named as unused and never validated |
| `TOKTOP_LOG_LEVEL` | audit log floor for every subsystem that writes one (ingest endpoint, engine collector, ssh client, `--add` attach, agent watch): `debug`, `info` (default), `warn`, or `error`; a set-but-invalid value aborts with exit code 2 |
| `TOKTOP_SCREENSHOT_FONT` | used only by `scripts/screenshot.py` (path to a regular-weight `.ttf`); the `toktop` binary ignores it |
| `GITHUB_TOKEN` | optional; authenticates `toktop update`'s GitHub API calls past the anonymous rate limit. A trailing newline (from `$(cat file)`) is stripped; a line break anywhere else is refused by name, since it cannot be sent as a header |
| `GAUNTLET_HOME` | directory holding `agents.json` (default `~/.gauntlet`); a relative value is ignored and named at startup, matching the XDG rows, and so is an absolute one with no `agents.json` under it |
| `XDG_DATA_HOME` | with `--opencode-db` (on by default with `--agents`): directory under which `opencode/opencode.db` is read (default `~/.local/share`); a relative value is ignored and named at startup |
| `XDG_CONFIG_HOME` | directory for the ssh trust-on-first-use host-key store (`toktop/known_hosts`; default `~/.config`); a relative value is ignored rather than placing the store under the working directory, and is named at startup with an `ssh://` target; a run on Linux with one fails at connect |
| `SSH_AUTH_SOCK` | ssh-agent socket for `ssh://` targets; on Windows the OpenSSH named pipe is used when unset |
| `NO_COLOR` | strips terminal styling when set to a non-empty value (honored by the terminal renderer, as usual) |

An explicit `--bearer`, even empty, wins over its env fallbacks; otherwise
the environment is used. Prefer an env var over `--bearer` for tokens:
command-line arguments are visible in process listings to every user on
the host, and passing `--bearer` prints a reminder of that. The token
travels only to endpoints named with `--add`; engines found by port scanning
receive no credentials, so a hostile listener on a probed port cannot
collect your gateway key. `--add` URLs must be `http://` or `https://` with
a host and must not embed userinfo (`user:pass@`); the same endpoint may not
be named twice, because two polls of it read as twice the tokens. An endpoint
that needs
the key is attached as `toktop --add http://127.0.0.1:20128` with the token
in the environment. `--ingest` must be `host:port` (an empty address would
bind every interface on an ephemeral port and is rejected). A bind that is not
loopback is not rejected, because a relay on another host is a legitimate
setup, but it is named at startup: the endpoint authenticates nothing, so every
reachable peer can post events. Unknown
`TOKTOP_*`
variables are reported at startup, so a typo fails loudly instead of doing
nothing (`TOKTOP_SCREENSHOT_FONT` is recognized so a developer export is
not reported as a typo). `$TOKTOP_BEARER` / `$OMNIROUTE_API_KEY` without
`--add`, `$TOKTOP_SSH_PASSWORD` without an `ssh://` target, and
`$TOKTOP_LOG_LEVEL` with `--demo --no-ingest` (and no `--agents`) are named as unused, matching the
flag warnings, as is a `GAUNTLET_HOME` that is not an absolute path under
`--agents` (or one with no `agents.json` under it), a relative `XDG_DATA_HOME` while opencode's database is read, and
a relative `XDG_CONFIG_HOME` with an `ssh://` target. Out-of-range flag values (`--interval 0`, `--interval` below
50ms or above 1h, negative `--probe`, `--probe` above 86400, `--frames < 1`
or above 180 with `--once`, an empty `--repo`, a malformed or duplicated
`--add` or
`--ingest`, an `ssh://` URL with a password, path, query, or fragment)
abort with exit code 2 instead of being silently adjusted; so do
out-of-range `TOKTOP_COLUMNS` / `TOKTOP_LINES` when `--once` renders, and
a set-but-invalid `TOKTOP_LOG_LEVEL`. A bare `--interval 1` is rejected
because it has no unit; use `1s` or `500ms`. A `--add` endpoint reached over
plain `http://` on a host that is not this machine is named at startup: the
bearer token would cross the network in cleartext. Startup prints one line of the knobs
that apply (`interval`, `ingest`, mode flags); bearer tokens appear only
as `bearer=set`.

## Build & test

```
make help                          # every task, one line each
make prereqs                       # check go, a C compiler, bun, uv against the pins
make build                         # host binary, version-stamped
make demo                          # build, then run the simulated fleet
make test                          # all tests, -race -shuffle=on
make test RACE=0                   # full suite, no race detector
make pr                            # every PR merge gate except the OS matrix
make test-pkg PKG=./internal/ui    # one package while iterating
make test-pkg PKG=./internal/core RUN=TestSanitizeTextPreservesUTF8
make test-pkg PKG=./agentusage     # both halves of the sqlite tag gate
make test-pkg PKG=./internal/ui RACE=0   # faster loop, no race detector
make install                    # install into PREFIX/bin (default ~/.local/bin)
```

Cross-compiles (no cgo anywhere); `make test-dist` is the same flags the
release uses (`-trimpath -buildvcs=false -mod=readonly -buildmode=pie`, plus
`-s -w -buildid= -bindnow` in `-ldflags`, so the ELF binaries carry full
RELRO):

```
make test-dist VERSION=x.y.z    # every release platform into dist/
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for prerequisites, the edit-test loop,
and what CI runs, [docs/THREAT_MODEL.md](docs/THREAT_MODEL.md) for the
attack surface, what toktop trusts, and the mitigations already in place,
[docs/DEPENDENCIES.md](docs/DEPENDENCIES.md) for every external package and
the reason it is here, and [docs/PRIVACY.md](docs/PRIVACY.md) for what it
reads, sends and stores.

Releases: push a tag `v*` and GitHub Actions attaches binaries for
linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64 and
windows/arm64, plus a CycloneDX SBOM of every dependency (`make sbom`) and a
buildinfo manifest naming the commit, toolchain, and flags behind those
bytes.
Versions are 0.x: the CLI, the ingest `/v1/events` body, and the
`agentusage` Go API may change without a major bump. Consumer-facing notes
live in [CHANGELOG.md](CHANGELOG.md). CI
runs `govulncheck` on every push; Dependabot keeps go modules, workflow
actions, and `scripts/` pip pins current.
