# Contributing to toktop

## Prerequisites

- Go, at the version pinned in `go.mod`. `make` sets `GOTOOLCHAIN` to that
  exact version so a newer compiler on the host cannot change the artifact.
  The pin is exact, not a floor: a host on any other patch of the same minor
  is fine, because `GOTOOLCHAIN` makes the go command fetch the pinned
  toolchain on the first build, but that first run needs network access.
  `make prereqs` says which of the two states the machine is in.
  CI installs the same version via `go-version-file: go.mod`.
- GNU Make and bash (`SHELL := /bin/bash` in the Makefile). Make 3.82 or
  newer: `.SHELLFLAGS` carries `errexit` and `pipefail` into every recipe, and
  the stock macOS make is 3.81, where that variable is an unknown name and
  the recipes run without either, so a gate whose first command fails
  reports the last one's status. `brew install make` supplies a newer one. On
  Windows, Git Bash plus `make`, or WSL.
- A C compiler (`gcc` or `clang`) for `go test -race`. `make test`,
  `make test-pkg` and `make cover` default to `-race` and `CGO_ENABLED=1`,
  and `make ci` runs `make test RACE=1`. `RACE=0` skips the race flag (no C
  compiler needed). The analysis targets (`make build`, `make vet`,
  `make lint`, cross-compiles) keep cgo off so analysis matches the
  released artifacts; those are pure Go and need nothing else.
- `bun` at the version in `.bun-version` for `make site-check` and
  `make site-lint`. The targets refuse a different version on PATH, matching
  CI's `bun-version-file`.
- `uv` at or above the version in the `uv` line of `.tool-versions` for
  `make scripts-check` (CI installs that file's version via `version-file`;
  black/ruff/mypy pins in `scripts/requirements-dev.txt`). The target names a
  too-old uv rather than failing on an unknown flag.
- `shellcheck` at or above the `SHELLCHECK_MIN` in the Makefile for
  `make check-shell`, which analyzes the bash completion script
  `toktop completion bash` prints, and for `make check-workflow-shell`, which
  analyzes the bash in every workflow `run:` block. The bash ubuntu runner
  already carries a newer one; both targets refuse an older version rather than
  reporting a pass from a rule set that predates a script's own
  `# shellcheck disable`.
- `zsh` and `fish` at or above `ZSH_MIN` and `FISH_MIN` in the Makefile, for
  the same target and the other two completion scripts. shellcheck has no zsh
  or fish mode, so each script is parsed by the shell that will source it; a
  completion that does not parse is a script the shell rejects outright. Both
  are ordinary distribution packages, and the Linux CI leg installs them rather
  than trusting the runner image to carry them.
- The Python in `.python-version`, exact like the other toolchains. `make
  scripts-env` passes it to `uv venv` as `--python`, so uv downloads that
  build when the host does not have it; nothing reads whatever `python3` the
  machine happens to offer. `target-version` in `pyproject.toml` is the
  floor the code must keep supporting, not the interpreter it runs on.
- No go command state to keep. `make` exports `GOENV=off` and clears
  `GOFLAGS`, `GOEXPERIMENT`, `GODEBUG`, `GOPRIVATE`, `GONOSUMDB`,
  `GONOPROXY` and `GOINSECURE`, and names `GOSUMDB=sum.golang.org`, so the go
  command reads neither the environment it inherits
  nor `~/.config/go/env`, whatever `go env -w` last wrote there. A value in
  that file is read before the Makefile's exports reach the go command and
  wins over them, so a machine carrying `go env -w GOAMD64=v3` would build a
  different binary from every other machine, and one carrying
  `GOPRIVATE=github.com/*` would skip the checksum database on modules the
  go.sum lines cover. `GOSUMDB` is named rather than cleared because `go.sum`
  does not cover every tool the build runs: `make govulncheck` and
  `make sbom` fetch govulncheck and cyclonedx-gomod at a
  `go run <tool>@<version>` pin, so the checksum database is the only check
  on those bytes. Everything the build needs is in the Makefile,
  `go.mod` and `GOWORK=off`. CI spells the same list in its workflow env
  block, because `make` exports reach its own recipes and not a workflow's
  `go` steps; `make check-ci-env` fails when the two copies disagree.
- No services or databases: everything is stdlib plus the modules in
  `go.mod`.
- Only to deploy, and never for the edit-test loop or any merge gate:
  `curl`, which reads `https://toktop.ai/health` so `make site-deploy` and
  `make site-rollback` can confirm the site is serving before reporting
  success. Both targets refuse to start without it, because without curl
  every health attempt fails for a reason that has nothing to do with the
  site: the poll would run all its attempts, sleep between each, and then
  report "the site is not serving", which reads as a broken or half-applied
  upload and sends the operator to roll back a deploy that published
  perfectly.
- Only to regenerate the README screenshot (below), and never for the
  edit-test loop or any merge gate: `tmux` to capture the live frame, and
  `magick` (ImageMagick 7) plus `avifenc` (libavif) for `make site-assets`,
  which rebuilds the captures under `site/public/`. Nothing in `make pr` runs
  them, so a machine without them passes every gate and only fails when it
  tries to recapture. `make site-assets` writes the two versions to
  `site/encoders.txt`, which is not tracked yet, so the check against it is
  inert until the first recapture commits the record; from then on a machine
  whose encoders disagree with it is refused, so a recapture cannot quietly
  rewrite every shipped capture with a different encoder's bytes. Bumping
  them is a deliberate act: delete `site/encoders.txt`, recapture, and commit
  the new record.
- Network access on first run. `make` pins `GOTOOLCHAIN` to the `go.mod`
  version, so a host with a different compiler downloads that toolchain; the
  first `make lint` and `make vet-cross` download the `staticcheck` tool the
  module pins; and `make govulncheck` (part of `make ci` and `make pr`)
  fetches the vulnerability database from vuln.go.dev. The edit-test loop
  (`make build`, `make test`, `make test-pkg`) works offline once the
  modules are in the local cache.

## Quickstart

```
git clone https://github.com/maci0/toktop && cd toktop
make prereqs     # check go, a C compiler, bun, uv, shellcheck, zsh and fish against the pins above
make test        # all tests, race detector, shuffled order
make demo        # build and run against a simulated fleet
```

`make prereqs` lists every missing or mismatched tool in one run and exits
non-zero if any is left, so the whole set gets installed before the first
failure instead of one tool per round trip. It also reports whether the
`go.mod` toolchain is already here or still to be downloaded, which is the
one gap that does not fail the run but does cost the first build a network
round trip. The per-target checks still fire where the tool is used, so it
never replaces them.

## The edit-test loop

Run one package or one test while iterating (drop `RUN` for a whole package).
`make` pins the `go.mod` compiler, `-mod=readonly`, `-race`, and `-shuffle=on`,
and turns cgo on, matching CI. `RACE=0` skips the race detector (and the C
compiler) for a faster cycle on `make test` and `make test-pkg`. For
`./agentusage`, omitting `TESTTAGS` runs both halves of the sqlite tag gate;
`TESTTAGS=sqlite` is one half only:

```
make test-pkg PKG=./internal/ui
make test-pkg PKG=./internal/core RUN=TestSanitizeTextPreservesUTF8
make test-pkg PKG=./agentusage
make test-pkg PKG=./agentusage TESTTAGS=sqlite
make test-pkg PKG=./internal/ui RACE=0
make test RACE=0
```

`RUN` is a regexp, and a name that matches nothing would otherwise exit 0
with `[no tests to run]`, so a renamed or mistyped test reads as a pass.
`test-pkg` lists the package's test names with the same regexp first and
fails on an empty list, naming any near miss and the command to list the
real names. A `RUN` containing a `/` selects subtests, which that check
cannot see, so it is passed through unchecked.

`RACE=0` drops `-race` and nothing else: the zone tag and `-shuffle=on` stay
on, so the fast loop tests the same binary, in the same shuffled order, as
the default one. `make check-test-flags` fails if a `go test` line in the
Makefile drops the zone tag or `-shuffle=on`, or glues one flag onto the
other, which is a different argument rather than a missing one and so
stays invisible to a `go test` that exits 0.

To see the dashboard render without an interactive terminal:

```
./toktop --demo --once --frames 2
TOKTOP_COLUMNS=120 TOKTOP_LINES=38 ./toktop --demo --once   # fixed size, for screenshots
```

While a TUI instance runs on Unix, rebuilding the binary re-execs into the
fresh build (hot reload); pass `--no-hot-reload` to disable. Windows cannot
exec over a running image, so the dashboard exits instead.

## Regenerating the README screenshot

`docs/images/dashboard.png` is captured from a live demo frame under tmux
and rendered by `scripts/screenshot.py`. This is the only workflow that needs
`tmux`; `magick` and `avifenc` are shared with `make site-assets`, which needs
no tmux. See Prerequisites.

```
make build VERSION=0.10.0
mkdir -p .scratch
tmux new-session -d -c "$PWD" -x 180 -y 50 -s shot './toktop --demo --seed 7 --no-hot-reload'
sleep 60 && tmux send-keys -t shot p   # let the charts fill, then probe
sleep 40                               # and let the probes answer
tmux capture-pane -e -p -t shot > .scratch/capture.txt
tmux kill-session -t shot
make screenshot CAPTURE=.scratch/capture.txt OUT=docs/images/dashboard.png \
  SCALE=2 COLS=180 ROWS=50
```

The trailing arguments are scale, columns and rows; passing the pane geometry
keeps the renderer from re-deriving it and wrapping. `VERSION` is stamped into
the header, so pass the version being released rather than `dev`. Rendering
needs a Meslo Nerd Font installed, or `TOKTOP_SCREENSHOT_FONT` pointing at a
regular-weight `.ttf`. `make screenshot` builds the same env under `dist/`
that `make scripts-check` uses, so the renderer runs the same pins with the
same wheel hashes. `-c "$PWD"` is what puts the pane in this checkout: a
detached session otherwise starts wherever the tmux server did, and
`./toktop` is not there.

The share card's pixel size (1200x704) is repeated in `site/worker.js` as the
`og:image` dimensions, since that card, not the full-size capture, is what
`og:image` points at. `make site-assets` rebuilds every capture the page serves
from the full-size PNG and then runs `bun test site/`, so the set that ships
and the set the worker names cannot drift:

```
make site-assets
```

AVIF is what browsers that speak it download (about a third of the WebP). The three
widths match the `srcset` in `site/worker.js`: 768w is the ~720px slot,
1280w covers phones at 3x and desktops at 1x, 1920w is the 2x desktop.
`-q 32` rather than 40 or 50: the capture is flat color and hard edges, and
the page downscales each candidate, so 32 is where the bytes stop paying:
10,577 bytes at 768w against 13,563 at `-q 40`, a 22% cut of the image that is
72% of a phone's 14,669-byte visit, at 30.0 dB PSNR against the resized
source. The gain
below 32 is not on screen at the size anyone reads it, so compare a crop
before moving it.
`dashboard-card.png` is the share card, not a fifth hero candidate: the
`og:image` crawlers fetch one URL and draw it at card size, so it is the
capture at 1200px, the width a `summary_large_image` is laid out at. The
capture is 262 flat colors, so `-colors 128` is not a visible cut and takes
it from 303,865 bytes to 68,924. The full-size PNG stays as the `<img src>`
fallback for a client with neither AVIF nor WebP. `bun test site/` pins the
HTML identity size, the compressed transfer sizes under the initial congestion
window, the AVIF/WebP byte ceilings and the card's width and
weight, so a recapture that blows the budget fails there.

The README shows the same frame and is the other browser surface in the
repository. GitHub's renderer drops `srcset` and `picture`, so it gets the one
file the README names, at whatever size that file is, and it used to name the
3240px PNG: 303,865 bytes on the first thing a reader downloads, against
45,559 for `docs/images/dashboard.avif`, the site's 1920w candidate at the
same encode settings. `make readme-assets` rebuilds that one file from the
same source with the same encoders, so the two captures stay the same bytes
and a recapture cannot leave the repository and the landing page showing
different dashboards. `bun test site/` pins the README's byte budget and that
the file is the capture `site/public/dashboard.avif` already ships.

## Make targets

`make help` lists every target that carries a `##` summary. The ones expected
in day-to-day work:

| target | what it does |
|---|---|
| `make build` | host binary with version stamping |
| `make prereqs` | check go, a C compiler, bun, uv, shellcheck, zsh and fish against the pins, naming every gap at once |
| `make demo` / `make run` | build, then launch |
| `make test` | all tests, `-race -shuffle=on` (same flags as CI); `RACE=0` skips `-race` |
| `make test-asan` | all tests again under `-asan` (both sqlite tag halves); the go command refuses `-race -asan` together, so this is a second run and not a flag on `make test`. It skips `TestStaticFrameAllocBudget` by name, because an instrumented allocator makes an exact allocation count meaningless; `make test` still asserts that budget. Linux CI and `make ci` run it; it needs a C compiler and does not enter the edit-test loop |
| `make test-pkg` | one package or test: `PKG=./internal/ui` `[RUN=TestName]` `[TESTTAGS=sqlite]` `[RACE=0]` |
| `make cover` | coverage summary per package into `dist/` |
| `make check` | go.mod tidy-diff + gofmt -s + staticcheck + vet + shellcheck plus zsh/fish parse over the completion scripts + shellcheck over the workflows' `run:` blocks + yamllint over `.github/workflows/`, `.github/dependabot.yml` and `docs/openapi.yaml` + the doc and CI guards (`check-test-flags`, `check-ci-tags`, `check-ci-env`, `check-ci-platforms`, `check-install-prefix`, `check-shell`, `check-workflow-shell`, `check-yaml`, `check-help-docs`, `check-changelog-structure`, `check-changelog-covers`) |
| `make ci` | Go merge gates: tidy-diff, fmt, lint, vet, govulncheck, race tests, address-sanitized tests |
| `make pr` | every PR merge gate except the OS matrix: `ci` + `site-lint` + `site-check` + `check-wrangler-doc` + `scripts-check` + `repro-check-pair` |
| `make fmt` | rewrite files with gofmt -s |
| `make fix` | apply `go fix` modernization autofixes, then gofmt |
| `make tidy` | run `go mod tidy` to clean up go.mod and go.sum |
| `make lint` | staticcheck over both halves of the sqlite tag gate |
| `make govulncheck` | `govulncheck` over both sqlite tag halves at the Makefile pin (same pin as CI) |
| `make scripts-check` | black, ruff and mypy (strict) over `scripts/` (same pins as CI) |
| `make site-lint` | biome format-check and lint over the files `biome.jsonc` includes (the Worker and the jsonc configs) at the Makefile `BIOME` pin (CI parity) |
| `make site-fmt` | rewrite those files with the biome formatter, then re-lint |
| `make site-check` | `bun test site/` |
| `make site-assets` | rebuild the shipped dashboard captures in `site/public/` from `docs/images/dashboard.png`, then run `bun test site/` (needs `magick`, `avifenc`, and the pinned `bun`) |
| `make readme-assets` | rebuild the README's dashboard capture in `docs/images/dashboard.avif` from the same source frame, then run `bun test site/` (same tools) |
| `make site-deploy` | run `site-lint`, `site-check`, and `check-deploy-source`, then record what it is about to upload in `dist/site.deployed/manifest` *before* deploying the site Worker at the `WRANGLER` pin, and poll `/health` (the record lands first, so a run killed mid-deploy still leaves `make site-rollback` something to undo) |
| `make check-site-records` | fail unless `site-deploy` records the deploy before it calls wrangler and `site-rollback` consumes that record before it does (`make check` runs it) |
| `make check-site-rollback-states` | fail unless `site-rollback` keeps a rollback this tree already did at exit 0 and answers a machine with no deploy record at a non-zero exit naming the platform's deployment list (`make check` runs it) |
| `make check-wrangler-doc` | fail unless CONTRIBUTING.md's login command and docs/THREAT_MODEL.md's deploy path name the Makefile's `WRANGLER` pin (`make pr` and `site-deploy` run it) |
| `make check-ci-tags` | fail unless every `go test` / `go vet` / staticcheck line in `.github/workflows/` carries the zone tag, and every `go vet` line carries `-tests=true` (`make check` runs it) |
| `make check-test-flags` | fail unless every `go test` line in the Makefile carries the zone tag, keeps `$(race_flag)` off the tag value, hands `-tags` one quoted argument, and keeps `-shuffle=on` (`make check` runs it) |
| `make check-ci-env` | fail unless every workflow env block pins the build inputs the Makefile exports, name and value (`make check` runs it) |
| `make check-ci-platforms` | fail unless the `ci.yml` build matrix and the Makefile's `PLATFORMS` are the same set (`make check` runs it) |
| `make check-install-prefix` | fail unless `PREFIX` resolves to an installable prefix on every platform `PLATFORMS` ships a binary for: it reads `HOME` then `USERPROFILE`, and a Windows shell names the home directory with the second and leaves the first unset (`make check` runs it) |
| `make check-yaml` | fail unless every workflow in `.github/workflows/`, `.github/dependabot.yml` and `docs/openapi.yaml` are valid YAML and pass the `.yamllint` rule set, at the `yamllint` pin in `scripts/requirements-dev.txt` (`make check` runs it) |
| `make check-site-tools` | fail unless the site deploy path checks for `curl` before it takes the lock, since the `/health` poll both recipes end in is a curl the Makefile would otherwise take from the machine unnamed (`make check` runs it) |
| `make check-shell` | fail unless `shellcheck` (at or above the Makefile's `SHELLCHECK_MIN`) clears the bash completion script and `zsh -n` / `fish --no-execute` parse the zsh and fish ones; all three are generated, so a copy kept beside the Go source cannot go stale (`make check` runs it, on the Linux CI leg) |
| `make check-workflow-shell` | fail unless `shellcheck` clears the bash in every workflow `run:` block: the code every merge gate is reached through, which yamllint parses the document around without ever reading. `scripts/workflow-run-blocks.awk` extracts one file per block under `dist/`, named for the workflow and the line the `run:` block starts on, so a finding names the step and a `# shellcheck disable` scopes to that step alone. SC2154 is excluded for these files because the names they use come from the job's `env:` block, not from the step that reads them. Runs from `make check` on the Linux CI leg |
| `make check-help-docs` | fail unless every target in this table carries the `## ` description `make help` reads, so a documented target is never missing from the listing (`make check` runs it) |
| `make check-changelog-structure` | fail unless no `CHANGELOG.md` section repeats an impact heading, so the entries a second `### Added` collected do not ship unlabelled (`make check` runs it) |
| `make check-changelog-covers` | fail unless `CHANGELOG.md` moved since the last release tag whenever `README.md`, `cmd/toktop/help.go`, `docs/openapi.yaml`, `agentusage/`, `internal/ui/json.go`, `site/worker.js` or `site/README.md` did. Every other changelog gate reads the file's shape, not the diff it describes, so this is the one that notices a change shipping under notes written for other commits (`make check` and `make release` run it) |
| `make site-rollback` | roll the site Worker back to the version before the last deploy, then poll `/health`; a second run with a rollback already on record is a no-op, and no gate runs, so it works on a tree that does not pass; with no marker of either kind it names the deployment list to read and exits 2 rather than reporting nothing to do while a broken Worker is live |
| `make vet-cross` | vet + staticcheck on every release platform (the pre-ship gate release.yml runs) |
| `make check-changelog` | verify CHANGELOG.md has release section and link for VERSION |
| `make check-api` | verify VERSION drops no declaration `agentusage` exported at the last release (`make release` runs it; a patch is refused, a minor bump passes only once CHANGELOG.md records the removal under `Breaking`) |
| `make check-release-source` | fail unless a non-dev VERSION builds from a clean, git-backed tree with a nonzero `SOURCE_DATE_EPOCH` (`make release` runs it; `ALLOW_DIRTY=1` overrides the tree check) |
| `make check-deploy-source` | fail unless `site/` and `wrangler.jsonc` hold no uncommitted changes, so the Worker and the captures it serves are in a commit (`make site-deploy` runs it; `ALLOW_DIRTY=1` overrides the tree check, the same override the release path uses) |
| `make buildinfo` | write the toolchain, commit, and flags behind `dist/` to a manifest |
| `make licenses` / `make license` | write the license text of every module the binary links, and this project's own LICENSE, into `dist/` (`make release` publishes and checksums both) |
| `make host-dist` | print the path of this host's `VERSION` artifact in `dist/`, or fail when that platform is not in `PLATFORMS` (the release job smoke-tests this path) |
| `make release-verify` | fetch every asset a published `VERSION` holds back from GitHub and re-verify each digest against that release's own `checksums.txt` (the restore drill the release job runs) |
| `make repro-check` | build every release platform twice, from two different source paths and two different build caches, then diff |
| `make repro-check-pair` | the same gate over `REPRO_PLATFORMS`, the pair the PR gate and the release job both build twice |

## Deploying the site

`make site-deploy` and `make site-rollback` are the only paths that touch
`toktop.ai`, and both drive wrangler, which needs a Cloudflare credential.
Either export `CLOUDFLARE_API_TOKEN` (an API token with Workers Scripts
edit permission) and `CLOUDFLARE_ACCOUNT_ID` for the account that owns the
Worker, or run `bunx wrangler@4.126.0 login` once and let wrangler keep the
OAuth token in `~/.wrangler`. The two come from the Cloudflare dashboard
(My Profile, API Tokens) and from the account's Workers overview; nothing
about them belongs in this repository, and no CI job deploys the site.

The Makefile takes `dist/site.lock` for the whole of either target, so a
second deploy or a rollback on another machine will not run against the
Worker at the same time. `make clean` refuses while that lock is held, since
removing `dist/` from under a running deploy would free it. That lock is per
checkout: `dist/` is gitignored, so
two clones can each hold it. The platform's own deployment history is what
resolves a genuine collision, through the Cloudflare dashboard's deploy log.

## Before opening a PR

CI (`.github/workflows/ci.yml`) runs gofmt -s and `go mod tidy -diff` on
Linux only, plus `make govulncheck` for both sqlite tag halves on Linux, and
the Linux leg of the test job runs twelve of the `make check` guards
(`check-ci-tags`, `check-ci-env`, `check-test-flags`, `check-ci-platforms`,
`check-install-prefix`, `check-help-docs`, `check-site-records`,
`check-site-rollback-states`,
`check-site-tools`, `check-changelog-structure`, `check-changelog-covers`,
`check-shell`). `check-workflow-shell` also runs there, and
`check-yaml` runs in the
`scripts` job, which
installs the pinned tool env that target lints with.
Vulnerability analysis follows the host platform's build constraints.
`staticcheck` and `go vet ./...` and `go test -race -shuffle=on ./...` run on
Linux, macOS and Windows, plus cross-compiles of linux/amd64, linux/arm64,
darwin/amd64, darwin/arm64, windows/amd64 and windows/arm64. Each
cross-compile job also runs `go vet ./...` and staticcheck under its
GOOS/GOARCH, so platform-specific files get the same static analysis as
the host build. Both halves of the sqlite tag gate (`agentusage` with and
without `-tags sqlite`) are vetted and staticchecked everywhere. Those `go`
lines are written out in ci.yml instead of calling the Makefile targets, so
each carries the tags the targets pass, `timetzdata` above all: a test binary
without the embedded zone database resolves `time.Local` against the host's
zone files, which is the fallback the released binaries no longer have, and
after that `make test` and CI are testing two different programs.
`make check-ci-tags` fails on any such line that lost the tag; it runs in
`make check` and in the Linux leg of the test job. It fails the same way on a
`go vet` line that lost `-tests=true`, the flag the Makefile targets pass: vet
skips the test files without it, and a test function whose name and signature
no longer match what `go test` runs is then never executed and never named.
Everything
except the three-OS test matrix and the cross-compile job is one command
locally:

```
make pr
```

That is `make ci` (gofmt, tidy, staticcheck, vet, yamllint over the
workflows and the feed contract, shellcheck plus a zsh and a fish parse over
the completion scripts, govulncheck, race tests for both sqlite tag halves),
`make site-lint` (biome formatter and linter over the
Worker and the jsonc configs, at the `BIOME` pin in the Makefile, config in
`biome.jsonc`; run
`make site-fmt` to apply the formatter), `make site-check`
(`bun test site/`), `make check-wrangler-doc` (the wrangler pin in the
Makefile against the login and deploy commands the docs name),
`make scripts-check`, and `make repro-check-pair`.
`scripts-check` installs the exact versions in
`scripts/requirements-dev.txt` into an isolated env under `dist/`
(`make scripts-env`, black, ruff, mypy, yamllint, plus the renderer deps).
Pure-Python pins
carry a wheel sha256; bumping one of those lines means updating the hash too,
and the install fails if a fetched file does not match. Do not run unpinned
`uvx black` /
`uvx ruff` /
`uvx mypy`: those resolve to whatever PyPI returns today. `make check-yaml`
installs from the same file for the same reason; the workflows are the one
thing here no Go analyzer reads, and a gate whose linter can change under it
is a gate nobody can reproduce a failure of. Platform-specific
files also need `make vet-cross` (the same gate `release.yml` runs before
shipping).

Keep platform-specific code behind build tags or runtime checks; the
cross-compile job catches code that only builds, or only vets and lints
cleanly, on the author's OS.

A new dependency needs an entry in [docs/DEPENDENCIES.md](docs/DEPENDENCIES.md)
with its license and the reason it beats writing the code here;
`internal/repogate/deps_test.go` fails when a module in go.mod's direct
require block is imported nowhere or has no entry in that file. Check the
stdlib and the platform before adding one.

A `repro` job builds two shipped platforms twice and fails if the bytes
differ. The second pass builds a copy of the working tree staged under
`dist/repro/src-b`, so the two passes read the same source from different
absolute paths, and each pass gets its own `GOCACHE`. That is what puts
`-trimpath` under test: locale, timezone, toolchain, and instruction-set
baselines are already pinned globally in the Makefile, so varying them
again inside a pass would vary nothing. It is the guard on the
reproducibility flags above, and
`make pr` runs it over the same pair, so a reproducibility failure shows up
before the push rather than after it. The release job runs
`make repro-check-pair` over that same pair, since neither job lists the
platforms itself. `make repro-check-pair` runs that pair on its own;
`make repro-check` runs the full `PLATFORMS` list and is what to
run before a release.

## Releases

Versions are 0.x: the CLI flags, the ingest `/v1/events` body, and the
`agentusage` Go API may change without a major bump. Move the Unreleased
section in [CHANGELOG.md](CHANGELOG.md) under the new version before tagging,
and leave an empty `## [Unreleased]` stub behind. `make check-changelog`
enforces this on the tag push: the release build fails unless the section and
its `[version]:` compare link both exist, the link ends at the tag being cut,
the section heading carries its release date, the section holds at least one
entry, and nothing is left under Unreleased. The `## [Unreleased]` heading and
its `[Unreleased]:` compare link have to survive the move too, since the next
release is written under them. Run it locally with
`make check-changelog VERSION=0.15.0` before you tag.

That gate reads CHANGELOG.md against the version being cut, so it says whether
the last cut was written down, never whether the work since it was. The
`TestChangelogRecordsWorkSinceLastRelease` guard in `internal/repogate` covers
the other half and runs with the rest of the merge gates: a tree that has
moved past the last tag without CHANGELOG.md moving with it, and whose
commits since that tag are not all of the `docs`, `build`, `ci`, `test`,
`chore`, `merge` and `refactor` types, has left those commits unrecorded, and
the test fails and names them. A `perf` commit is owed an entry, since what it
makes faster is what a user times. Write the entry as the change lands; an
entry found at the next cut is one the next author had to reverse-engineer
from a diff. The guard lets a tree with no tag, no git, or a CHANGELOG.md that
moved since the tag past it, so the release cut itself is not the omission it
looks for.

That guard reads commit subjects, so it asks what was committed and not what
was read. `make check-changelog-covers` asks the other half: `README.md`,
`cmd/toktop/help.go`, `docs/openapi.yaml`, `agentusage/`,
`internal/ui/json.go`, `site/worker.js` and `site/README.md` are what a
consumer reads rather than compiles, and one of them moving since the last tag
with CHANGELOG.md untouched is a change whose commit subject
(`chore: update dsh.go`) says nothing about what a reader meets.
It runs on the merge gates and again on the cut, where the Go guard does not
run at all. The rest of `docs/` is not watched: `ARCHITECTURE.md` is a map of
the tree and `RECOVERY.md` a set of procedures, both correctable without a
change an upgrading user can observe.

`docs/openapi.yaml` carries the feed's own version, and
`TestOpenAPIVersionNamesTheReleaseItDescribes` in `internal/repogate` holds it
to the release the document's last change shipped in. A client generator pins
`info.version`, so a feed that moved under a version that stood still leaves a
generated client unable to tell it is stale. The changelog entry the file owes
and the version it owes are both asked for separately, because a release can
carry the entry and still leave the field behind.

`make check-api` runs beside it on the tag push. It reads the exported
declarations `agentusage` carries now and the ones it carried at the last
release, and refuses a version that drops one, since a Go caller meets that as
a compile error in their own tree while this one still builds and tests clean.
The rule is the changelog gate's rule: 0.x, so a breaking change rides a minor
bump and a patch is refused. A minor bump is let through, and only against a
`### Breaking` heading under the version being cut: the gate refuses the
release when a removal it found has nowhere in the changelog to be recorded,
since the caller upgrading reads those notes and not the diff. Run it with
`make check-api VERSION=0.15.0`. A checkout with no released tag before the
commit being cut has nothing to compare against and passes, and so does a
repository's first commit, which has no parent. A shallow checkout is
different: it has that parent in the real history and the clone simply does
not carry it, so the gate refuses it with a message naming `fetch-depth: 0`
rather than comparing nothing and passing. `ALLOW_SHALLOW=1` overrides, for
the cut you accept going unchecked. The release workflow checks the tag push
out at full depth for that reason.

Keep one heading per impact, in the order the file uses: `Breaking`, then
`Added`, `Security`, `Changed`, `Fixed`. A change a sender or a Go caller
would notice belongs under `Breaking` even at 0.x, and it names the before
and after plus what to do about it. The same gate reads the heading and the
previous release's version: a `Breaking` entry under a patch bump fails,
because 0.x still owes the caller a minor, not a patch, for a break. The
version is compared with any prerelease or build suffix stripped, so
`0.18.3-rc.1` is the patch bump it is and a `Breaking` entry under it fails
the same way. Adding a field to an exported `agentusage` struct lands there,
since a Go caller building it unkeyed no longer compiles.

The source stamp is empty. `make build` writes `dev` via `-ldflags
-X main.version=...`; a release tag writes the version with the `v` prefix
stripped. `go install` without ldflags reads the module version Go embeds, so
`--version` and `toktop update` see the tag that was installed, not `dev` and
not a leftover `0.1.0`.

Push a tag `v*`: GitHub Actions tests (both halves of the sqlite tag gate),
cross-compiles every platform, generates checksums, a buildinfo manifest, a
CycloneDX SBOM, the license text of every module the binary links and this
project's own LICENSE, and attaches binaries to the release. The buildinfo file
records the commit, toolchain, and flags behind the bytes, so a rebuild
attempt has something to match; it is listed in checksums.txt too, as is
the SBOM and the two license files. `make dist-clean` keeps only the files the current `VERSION`
publishes, so a leftover binary, SBOM, or tarball from an earlier local
`make release` cannot be checksummed and shipped with this one. Versions with a prerelease suffix, such as
`v0.6.0-rc.1`, are marked as prereleases and excluded from the stable
`toktop update` channel. Hyphens in build metadata do not mark a prerelease.
The host-platform artifact is smoke-tested
for `--version` and for the sqlite driver actually being linked. Locally,
`make release VERSION=x.y.z` reproduces the same artifacts in `dist/`. The
checksums tarball is built deterministically: members are sorted,
checksums.txt lines are sorted by filename, timestamps come from
`SOURCE_DATE_EPOCH` (defaulting to the commit time), ownership is
normalized, mode is forced to 0644 (so a builder's umask cannot change
the archive), atime/ctime PAX headers are dropped, and gzip's name/mtime
header is stripped at compression level 6, so two builds of one source
produce byte-identical archives. Binaries are built with `-trimpath
-buildvcs=false -mod=readonly -buildmode=pie`. A release refuses to package
an uncommitted working tree or a source export with no git behind it, so the
bytes always match the commit the manifest names; `ALLOW_DIRTY=1` overrides
the first. Deterministic packaging needs GNU tar; where the system tar is
bsdtar (macOS), install GNU tar as `gtar`.

A tag is a version, not a branch: cut it once and leave it. Moving one re-runs
the release job, and the job refuses to run when a release of that tag already
exists, because replacing the binaries under a version people have installed
and verified breaks the checksum they recorded. Cut a new version instead.

The publish step uploads every top-level file `dist/` holds, so `make release`
first runs `dist-clean`, which deletes the regular files an earlier `make cover`
or a previous local run left behind. Directories such as the site deploy lock
and the nested `dist/bin` and `dist/repro` output are not touched, and the keep
list is version-scoped: `toktop_<version>_*` and `toktop-sbom-<version>*` for
this `VERSION` only. An artifact from an earlier version is dropped, and so is
`toktop_<version>_checksums.tar.gz`, which the tar step is about to rewrite;
both are deliberate. The version scoping is what stops a `make -j release`
from dropping the SBOM another prerequisite just wrote.

## Recovering a release

A release is the only place the shipped binaries exist, and the tag's tree in
the repository is what they are rebuilt from. The upload step exiting zero is
not evidence of that: `fail_on_unmatched_files` reports a glob that matched
nothing, not an asset that never arrived. So the release job ends with
`make release-verify`, which is the restore drill. It reads the published
asset list, compares it against what `PLATFORMS` says a `VERSION` holds,
downloads every asset, and re-verifies each digest against the release's own
`checksums.txt`. It needs `gh` and `GH_TOKEN`; the downloads land under
`dist/`, which `make clean` takes.

```
make release-verify VERSION=0.15.0
```

Run it against a published version, not only right after a release: a version
whose assets are short, empty, or unlisted is one `toktop update` cannot
install from, because it refuses a binary it has no checksum for.

A published release cannot be re-run, so a partial one is not repaired by
pushing the tag again. Rebuild the same bytes and upload what is missing:

```
git checkout v0.15.0
make release VERSION=0.15.0
gh release upload v0.15.0 dist/toktop_0.15.0_* dist/toktop-sbom-0.15.0* --clobber
make release-verify VERSION=0.15.0
```

The two globs are the whole asset set `release-verify` expects: the
`toktop_<version>_*` binaries, buildinfo manifest, third-party license texts,
the project's own LICENSE and checksums tarball, and the `toktop-sbom-<version>*`
file, which is spelled with a dash and is therefore not in the first glob.

The rebuild is byte-identical to what was published, which is what makes this
safe under the rule that forbids replacing a published version: the checksums
already recorded by everyone who installed it stay true. Built from any other
commit the artifacts carry different bytes, and the version has to be cut
again instead.

The RPO and RTO for a release are those two commands. There is no
point-in-time to select: a version has one set of assets, and the tag names
the source they are built from. What protects a version from deletion is
GitHub's own retention on the tag and the release, which this repository does
not configure and cannot verify from here.
