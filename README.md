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
toktop ssh://maci@box    # watch engines on another host
```

## Install

```
CGO_ENABLED=0 go install -tags sqlite github.com/maci0/toktop/cmd/toktop@latest
```

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
and dsh all keep transcripts that carry the provider's own counts. dsh's
default log is concatenated zstd frames; uncompressed JSONL is read too.
Agents that report nothing show no rate rather than a zero.

Two agents keep databases instead of transcripts, and both need the `sqlite`
build tag, which decides whether a driver is compiled in at all (released
binaries and `make build` carry it; `make build TAGS=` leaves it out).

opencode keeps one session store for the whole machine, so it is gated twice:
the tag links the driver, and `--opencode-db` decides whether a binary that has
it opens the operator's database. Asking for it in a build without the driver
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
Set `GAUNTLET_HOME` to read that file from somewhere else. Only the `usage`
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

### Using the Go package

Import `github.com/maci0/toktop/agentusage` to discover agent processes and
read the token counts they already write:

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
		if w == nil {
			continue
		}
		wg.Go(func() {
			w.Run(ctx, 250*time.Millisecond, func(s agentusage.Sample) {
				fmt.Printf("%s pid %d: %d output, %d prompt\n", p.Tool, p.PID, s.Output, s.Input)
			})
		})
	}
	wg.Wait()
}
```

The example watches the discovered processes concurrently for ten seconds.
Only usage written after attachment is reported; existing transcript counts
are skipped. Keep an agent generating during that window to see output.

`RegisterSpec` teaches the package about an agent it was not compiled to know.
`errors.Is` matches `ErrEmptyTool` and `ErrNoRoots` on a rejected spec, and
`ErrInvalidDefinitions` on a malformed definitions file or colliding agent names
after normalization. `Rate` is output
tokens per second between two samples; `InputRate` is the same for billed
prompt tokens.

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
    -H 'Idempotency-Key: turn-1' -d \
    '{"agent":"coder","kind":"tool","prompt_tokens":4200,"output_tokens":310,"thinking_tokens":40,"note":"shell(git status)"}'
  ```
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
| `GET /healthz` | liveness probe, answers `ok` |

Event fields are all optional; anything omitted gets the default:

| field | type | default | notes |
|---|---|---|---|
| `id` | string | - | caller-chosen key, capped at 128 characters; a repeat of a key still in the retained feed (last 512 events) is ignored. When omitted, a request `Idempotency-Key` header is used: the first eight bytes of its SHA-256 hash, encoded as 16 hexadecimal characters, followed by the 1-based line index (`<hash>:1`, `<hash>:2`, and so on). The handler hashes the received key without truncation or whitespace collapsing; hash collisions remain possible |
| `ts` | RFC 3339 string | arrival instant | offset required (`2026-01-02T03:04:05Z`); stamps more than two minutes ahead of arrival are clamped to the arrival instant |
| `agent` | string | `anonymous` | capped at 64 characters |
| `model` | string | - | capped at 128 characters |
| `kind` | string | `turn` | known kinds: `turn`, `tool`, `error`, `note`; custom kinds pass through lowercased, capped at 24 characters |
| `prompt_tokens` / `output_tokens` / `thinking_tokens` | integer | `0` | negative values and values above 2^40 clamp to `0`; a whole JSON number such as `100.0` counts; thinking is the reasoning share of output when the agent says so |
| `via_engine` | string | - | monitored engine already counting this output; aggregates skip the event; capped at 128 characters |
| `note` | string | - | free-form, capped at 512 characters |

One POST answers `202` with `{"accepted":N}` once every event in the stream
is recorded, `400` for malformed JSON or a bad `ts`, `408` when a stream
stalls mid-body, and `413` past the 1 MiB body cap. A POST carrying an
`Origin` header (browser-driven; scripts and agents never send one) is
refused with `403`, so a web page cannot forge rows into a running
dashboard. Wrong methods on these paths answer `405` with `Allow`.
Unknown paths answer `404` naming the two endpoints, so a POST to `/events`
is not a generic not-found page. Error bodies are short plain-text reasons
that name the field or expected shape; unknown fields are ignored, so
harnesses can include their own. The request `Content-Type` header is not
checked: the body is always read as JSON/NDJSON, so plain `curl -d` works
unmodified.
Every POST is logged to stderr as one structured line (`req`, `method`,
`path`, `status`, `accepted`, `duration`, `remote`; failures add `error`).
Wrong-method and unknown-path requests log the same way, so a harness
posting to `/events` is not silent. `GET /healthz` is not logged. Event
bodies are not logged. A handler panic is one ERROR
line with `req` and a single-line `stack`. Responses carry `X-Request-Id`,
echoed from the request when the sender set one.

Streams are recorded line by line: if a later line fails, events before it
stay recorded and the error states how many. Retrying a stream (or a
successful POST whose 202 was lost) is safe when each event carries a stable
`id`, or when the POST carries `Idempotency-Key` (filled in for events that
omit `id`). Without either, replaying the kept lines would duplicate them.

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
                  report it, --repo owner/name for a fork)
toktop help       same as --help; `toktop help update` / `toktop help version`
toktop version    same as --version
--demo            simulated fleet, zero setup
--add URL         attach an openai-compatible http(s) endpoint (repeatable;
                  host required, no userinfo; use --bearer / $TOKTOP_BEARER)
ssh://user@host   positional; monitor remote hosts (repeatable;
                  ssh://[user@]host[:port] only, no password in the URL)
--ssh-key PATH    private key for ssh targets (overrides ~/.ssh/config;
                  ~ is expanded; a missing file aborts at startup)
--bearer TOKEN    bearer token sent to --add endpoints only; OmniRoute API
                  keys etc. (env: OMNIROUTE_API_KEY, then TOKTOP_BEARER;
                  an explicit --bearer, even empty, wins)
--agents          watch AI coding agents on this machine (session logs)
--opencode-db     with --agents: also read opencode's SQLite session
                  database (needs a build with the sqlite tag)
--probe N         auto-probe every N seconds (0=off, max 86400)
--interval D      poll interval (Go duration such as 1s or 500ms; default 1s;
                  min 50ms, max 1h; a bare number is nanoseconds and is rejected)
--ingest ADDR     agent event listen address, host:port
                  (default 127.0.0.1:8420; empty is rejected)
--no-ingest       disable the event endpoint (`--ingest` is then ignored)
--once            render one frame and exit (use when piping or redirecting)
--plain           with --once: linear text report instead of the dashboard
                  frame (screen-reader friendly)
--frames N        with --once: snapshots to accumulate before rendering
                  (max 180, the chart history length)
--seed N          demo RNG seed
--no-hot-reload   disable restart-on-rebuild while running
--version         print version and exit
--help, -h        show usage, examples and environment fallbacks
```

Password auth for ssh targets: interactive prompt, or `TOKTOP_SSH_PASSWORD`.

## Environment variables

| variable | what it does |
|---|---|
| `OMNIROUTE_API_KEY` | bearer token fallback for `--bearer` (checked first unless `--bearer` is passed) |
| `TOKTOP_BEARER` | bearer token fallback for `--bearer` (checked after `OMNIROUTE_API_KEY`) |
| `TOKTOP_SSH_PASSWORD` | ssh password for headless runs; otherwise an interactive prompt |
| `TOKTOP_COLUMNS` / `TOKTOP_LINES` | fixed frame size for `--once` output (screenshots, capture); must be 41-1024 / 21-512, and a set-but-invalid value aborts with exit code 2 |
| `TOKTOP_LOG_LEVEL` | ingest audit log floor: `debug`, `info` (default), `warn`, or `error`; a set-but-invalid value aborts with exit code 2 |
| `TOKTOP_SCREENSHOT_FONT` | used only by `scripts/screenshot.py` (path to a regular-weight `.ttf`); the `toktop` binary ignores it |
| `GITHUB_TOKEN` | optional; authenticates `toktop update`'s GitHub API calls past the anonymous rate limit |
| `GAUNTLET_HOME` | directory holding `agents.json` (default `~/.gauntlet`) |
| `XDG_DATA_HOME` | with `--opencode-db`: directory under which `opencode/opencode.db` is read (default `~/.local/share`) |
| `XDG_CONFIG_HOME` | directory for the ssh trust-on-first-use host-key store (`toktop/known_hosts`; default `~/.config`) |
| `SSH_AUTH_SOCK` | ssh-agent socket for `ssh://` targets; on Windows the OpenSSH named pipe is used when unset |
| `NO_COLOR` | strips terminal styling when set to any value (honored by the renderer) |

An explicit `--bearer`, even empty, wins over its env fallbacks; otherwise
the environment is used. Prefer an env var over `--bearer` for tokens:
command-line arguments are visible in process listings to every user on
the host, and passing `--bearer` prints a reminder of that. The token
travels only to endpoints named with `--add`; engines found by port scanning
receive no credentials, so a hostile listener on a probed port cannot
collect your gateway key. `--add` URLs must be `http://` or `https://` with
a host and must not embed userinfo (`user:pass@`); an endpoint that needs
the key is attached as `toktop --add http://127.0.0.1:20128` with the token
in the environment. `--ingest` must be `host:port` (empty would bind every
interface on an ephemeral port and is rejected). Unknown `TOKTOP_*`
variables are reported at startup, so a typo fails loudly instead of doing
nothing (`TOKTOP_SCREENSHOT_FONT` is recognized so a developer export is
not reported as a typo). `$TOKTOP_BEARER` / `$OMNIROUTE_API_KEY` without
`--add`, `$TOKTOP_SSH_PASSWORD` without an `ssh://` target, and
`$TOKTOP_LOG_LEVEL` with `--no-ingest` are named as unused, matching the
flag warnings. Out-of-range flag values (`--interval 0`, `--interval` below
50ms or above 1h, negative `--probe`, `--probe` above 86400, `--frames < 1`
or above 180 with `--once`, an empty `--repo`, a malformed `--add` or
`--ingest`, an `ssh://` URL with a password, path, query, or fragment)
abort with exit code 2 instead of being silently adjusted; so do
out-of-range `TOKTOP_COLUMNS` / `TOKTOP_LINES` when `--once` renders, and
a set-but-invalid `TOKTOP_LOG_LEVEL`. A bare `--interval 1` is 1
nanosecond in Go and is rejected. Startup prints one line of the knobs
that apply (`interval`, `ingest`, mode flags); bearer tokens appear only
as `bearer=set`.

## Build & test

```
make help                          # every task, one line each
make build                         # host binary, version-stamped
make demo                          # build, then run the simulated fleet
make test                          # all tests, -race -shuffle=on
make test RACE=0                   # full suite, no race detector
make pr                            # every PR merge gate except the OS matrix
make test-pkg PKG=./internal/ui    # one package while iterating
make test-pkg PKG=./internal/core RUN=TestSanitizeTextPreservesUTF8
make test-pkg PKG=./agentusage     # both halves of the sqlite tag gate
make test-pkg PKG=./internal/ui RACE=0   # faster loop, no race detector
```

Cross-compiles (no cgo anywhere); `make test-dist` is the same flags the
release uses (`-trimpath -buildvcs=false -mod=readonly -buildmode=pie`):

```
make test-dist VERSION=x.y.z    # every release platform into dist/
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for prerequisites, the edit-test loop,
and what CI runs, and [docs/THREAT_MODEL.md](docs/THREAT_MODEL.md) for the
attack surface, what toktop trusts, and the mitigations already in place.

Releases: push a tag `v*` and GitHub Actions attaches binaries for
linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64 and
windows/arm64, plus a CycloneDX SBOM of every dependency (`make sbom`).
Versions are 0.x: the CLI, the ingest `/v1/events` body, and the
`agentusage` Go API may change without a major bump. Consumer-facing notes
live in [CHANGELOG.md](CHANGELOG.md). CI
runs `govulncheck` on every push; Dependabot keeps go modules, workflow
actions, and `scripts/` pip pins current.
