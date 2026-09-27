# Dependencies

Every external package this repository ships or runs on, and the reason it is
here. A dependency that cannot answer "what replaced writing it" does not land,
and `cmd/toktop/deps_test.go` fails the build when a module in go.mod's direct
require block is neither imported nor listed below.

Pins live in one place per ecosystem: go.mod plus go.sum for Go, the
requirements files under scripts/ for Python, the Makefile for the tools that
run from a `go run` or `bunx` pin. No floating ranges anywhere.

## Go, linked into the binary

| Module | License | Why it is here |
| --- | --- | --- |
| github.com/charmbracelet/bubbletea | MIT | TUI event loop, alt-screen handling, and the key decoding that every interactive control needs. |
| github.com/charmbracelet/lipgloss | MIT | Style, layout, and color profile detection for the theme in internal/ui. |
| github.com/muesli/termenv | MIT | Terminal capability detection, read by the UI perf benchmark. Test-only, kept direct so the benchmark measures one pinned version. |
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

## Go, tooling only

`honnef.co/go/tools` is pinned by the `tool` directive and run with `go tool`, so
no analyzer lands in the binary. `govulncheck` and `cyclonedx-gomod` run from a
`go run` version pin in the Makefile, not from the module graph, so their
newer x/tools requirement cannot drag the shipped build up.

## Python, scripts/ only

`scripts/screenshot.py` runs on a developer's machine and is never linked into
a release artifact. The pins are exact, with sha256 hashes on the pure-Python
packages (a registry swap of those files fails the install). pillow and pytokens
ship per-platform or per-interpreter wheels, so they stay version pins: hashing
one wheel would refuse every other OS/arch/CPython.

- runtime: pyte (LGPL-3.0), wcwidth (MIT), pillow (MIT)
- tools: black, ruff, and their transitive closure (click, packaging,
  pathspec, platformdirs, mypy-extensions, pytokens)

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
- `cmd/toktop/deps_test.go`: a direct require that nothing imports, or that has
  no entry in this file, fails the test run. The same file holds the module of
  every `tool` directive, every pin in the requirements files, and every tool
  the Makefile fetches, so a module, a distribution, or a `go run`/`bunx`
  version that arrives without a reason here, or without a version, fails the
  test run too.

A release ships the checksums file and the SBOM, not a sigstore attestation, so
a downloaded binary is verified against the checksum its own release page
publishes rather than against a transparency-log entry.
