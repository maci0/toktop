# Dependencies

Every external package this repository ships or runs on, and the reason it is
here. A dependency that cannot answer "what replaced writing it" does not land,
and `internal/repogate/deps_test.go` fails the build when a module in go.mod's
direct require block is neither imported nor listed below.

Pins live in one place per ecosystem: go.mod plus go.sum for Go, the
requirements files under scripts/ for Python, the Makefile for the tools that
run from a `go run` or `bunx` pin. No floating ranges anywhere. There is no
`uv.lock`: pyproject.toml declares no project, nothing in the tree runs
`uv sync`, and a lock file beside a project-less pyproject records a
`requires-python` no recipe here honors. `uv` installs the two requirements
files and nothing else.

## Go, linked into the binary

| Module | License | Why it is here |
| --- | --- | --- |
| github.com/charmbracelet/bubbletea | MIT | TUI event loop, alt-screen handling, and the key decoding that every interactive control needs. |
| github.com/charmbracelet/lipgloss | MIT | Style, layout, and color profile detection for the theme in internal/ui. |
| github.com/klauspost/compress | Apache-2.0, BSD-3-Clause | zstd decode for dsh session stores. stdlib has no zstd, and the zstd payloads there are large enough to matter. |
| github.com/rivo/uniseg | MIT | Grapheme cluster iteration, so truncation and width never split an emoji or a combining sequence. |
| golang.org/x/crypto | BSD-3-Clause | SSH client, agent, and known-hosts handling for the remote collector. |
| golang.org/x/sys | BSD-3-Clause | The host vitals syscalls stdlib does not expose: sysctl, uname and clock reads on Unix, the named-pipe error codes and lazy system DLLs on Windows. |
| golang.org/x/term | BSD-3-Clause | Raw-mode terminal control and window size. |
| golang.org/x/text | BSD-3-Clause | Unicode normalization (NFC) and case folding for agent names and paths, which are compared across filesystems that disagree about form. |
| modernc.org/sqlite | MIT, SQLite public domain | Pure-Go SQLite driver for the crush and opencode session databases, behind the sqlite build tag. CGO stays off so cross-compilation and reproducible builds are unaffected. |

The klauspost/compress license carries an Apache-2.0 patent grant, and
modernc.org/sqlite ships the SQLite sources in the public domain. Both are
compatible with this repository's MIT license; the release SBOM records them per
release (`make sbom`).

## Go, never linked into a release binary

| Module | License | Why it is here |
| --- | --- | --- |
| github.com/muesli/termenv | MIT | Terminal capability detection, read by the UI perf benchmark. Direct so the benchmark measures one pinned version, which no released binary imports. |

## Go, tooling only

`honnef.co/go/tools` is pinned by the `tool` directive and run with `go tool`, so
no analyzer lands in the binary.

## Fetched by a recipe

These are dependencies too: each is resolved through a registry while a target
runs, and a Makefile target that reaches for one without a row here is a package
this tree took a dependency on with no record of why. The pin lives in the named
Makefile variable, and `TestToolPinsAreExact` and `TestFetchedToolsAreDocumented`
hold both halves of it: the version and the reason.

| Makefile pin | Tool | License | Why it is here |
| --- | --- | --- | --- |
| `GOVULNCHECK` | `golang.org/x/vuln/cmd/govulncheck` | BSD-3-Clause | Reachable-call-graph advisory scan for the standard library and every module above. |
| `SBOM_TOOL` | `github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod` | Apache-2.0 | Writes the CycloneDX inventory a release ships, licenses included. |
| `BIOME` | `@biomejs/biome` | MIT OR Apache-2.0 | Formats and lints the Worker. The Rust binary, not a JS tree, so the Worker keeps no manifest. |
| `WRANGLER` | `wrangler` | MIT OR Apache-2.0 | Publishes and rolls back the Worker. The only thing here that talks to Cloudflare. |

The two `go run` tools are pinned outside the module graph on purpose: their x/tools
requirement is newer than the shipped build's, so joining the module graph would drag
every released binary up with them.

## Python, scripts/ only

`scripts/screenshot.py` runs on a developer's machine and is never linked into
a release artifact. `make check-yaml` is here for the same reason: it lints
`.github/workflows/` and `.github/dependabot.yml`, which nothing else in the
tree parses. The interpreter is pinned exactly in `.python-version` and
the Makefile passes it to `uv venv` as `--python`, so the pins below fix both
what is installed and what it is installed into. The pins are exact, with
sha256 hashes on the pure-Python
packages (a registry swap of those files fails the install). pillow, pytokens
and pyyaml ship per-platform or per-interpreter wheels only, so they stay
version pins: hashing
one wheel would refuse every other OS/arch/CPython. mypy and its compiled
runtime deps (librt, ast-serialize) are in that second group for the same
reason: mypy 2.x publishes per-interpreter wheels only. The install runs with
`--no-deps`, so the two files are the entire closure: nothing is resolved out
of the index to satisfy a dependency the files do not name, and a tool that
grows one fails its first run rather than pulling an unpinned package. That
includes the analyzers' own requirements: yamllint used to run through `uvx`,
which pinned the linter and left pyyaml and pathspec to be resolved out of the
index on every run, so a linter's behavior could change under a gate that
nothing in the tree could reproduce.

- runtime: pyte (LGPL-3.0), wcwidth (MIT), pillow (MIT)
- tools: black, ruff, mypy, yamllint (LGPL-2.1), and their transitive closure
  (click, packaging, pathspec, platformdirs, mypy-extensions, pytokens, librt,
  ast-serialize, typing-extensions, pyyaml)

## JavaScript, site/ only

The Cloudflare Worker has no dependencies and no manifest. Biome and wrangler
run from version pins in the Makefile (`make site-lint`, `make site-deploy`), so
a linter and a deploy tool cannot pull a tree into the repo for themselves.
`site/wrangler.jsonc` names the compatibility date the Worker is written
against.

## Gates that keep this honest

- `go mod tidy -diff` in CI: the manifest matches the imports. Every build
  runs with `-mod=readonly`, so a module missing from go.sum or a manifest out
  of step with the source fails the build rather than resolving around it.
- `make govulncheck`, both sqlite tag halves: no reachable vulnerability in the
  standard library or the modules above.
- `make sbom` on every release: a CycloneDX inventory with per-module licenses
  ships next to the binaries.
- `internal/repogate/deps_test.go`: a direct require that nothing imports, or
  that has no entry in this file, fails the test run. The same file requires an
  entry here for every `tool` directive and every pin in the requirements
  files, a version on every `go run`/`bunx`/`uvx`/`npx`/`pip install`
  invocation the Makefile or a workflow step fetches with, and an entry here
  for the tool that invocation names.
- `TestPythonRuntimePinsAreUsed`, same file: every pin in
  `scripts/requirements.txt` is either imported by a file under `scripts/` or
  named in `pythonClosure` as a requirement of a pin that is. The install runs
  `--no-deps`, so a pin nothing reaches for is a package fetched from PyPI on
  every developer run and every `make scripts-check`. `pythonImportNames` holds
  the pins whose distribution name is not the module name a file writes
  (pillow is `PIL`), and both maps fail the test when they name a pin that is
  gone.
- `TestWorkflowActionsAreCommitPinned`, same file: every third-party `uses:`
  in `.github/workflows/` is a 40-character commit id. A tag or a branch is a
  name the publisher can move, and the action runs with the job's token and
  network, so a moved ref changes what CI executes with no review in the tree.

A release ships the checksums file and the SBOM, not a sigstore attestation, so
a downloaded binary is verified against the checksum its own release page
publishes rather than against a transparency-log entry.
