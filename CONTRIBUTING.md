# Contributing to toktop

## Prerequisites

- Go, at the version pinned in `go.mod`. `make` sets `GOTOOLCHAIN` to that
  exact version so a newer compiler on the host cannot change the artifact.
  CI installs the same version via `go-version-file: go.mod`.
- GNU Make and bash (`SHELL := /bin/bash` in the Makefile). On Windows, Git
  Bash plus `make`, or WSL.
- A C compiler (`gcc` or `clang`) for `go test -race`. `make test` and
  `make test-pkg` default to `-race` and `CGO_ENABLED=1`. `RACE=0` skips
  both (no C compiler needed). Everything else (`make build`, `make vet`,
  `make lint`, cross-compiles) keeps cgo off so analysis matches the
  released artifacts. Plain builds and cross-compiles are pure Go and need
  nothing else.
- `bun` at the version in `.bun-version` for `make site-check` and
  `make site-lint`. The targets refuse a different version on PATH, matching
  CI's `bun-version-file`.
- `uv` at or above the version in the `uv` line of `.tool-versions` for
  `make scripts-check` (CI installs that file's version via `version-file`;
  black/ruff pins in `scripts/requirements-dev.txt`). The target names a
  too-old uv rather than failing on an unknown flag.
- No services or databases: everything is stdlib plus the modules in
  `go.mod`.
- Only to regenerate the README screenshot (below), and never for the
  edit-test loop or any merge gate: `tmux` to capture the live frame, and
  `magick` (ImageMagick 7) plus `avifenc` (libavif) for `make site-assets`,
  which rebuilds the captures under `site/public/`. Nothing in `make pr` runs
  them, so a machine without them passes every gate and only fails when it
  tries to recapture.
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
make prereqs     # check go, a C compiler, bun and uv against the pins above
make test        # all tests, race detector, shuffled order
make demo        # build and run against a simulated fleet
```

`make prereqs` lists every missing or mismatched tool in one run and exits
non-zero if any is left, so the whole set gets installed before the first
failure instead of one tool per round trip. The per-target checks still fire
where the tool is used, so it never replaces them.

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
and rendered by `scripts/screenshot.py`. This is the only workflow that
needs `tmux`, `magick` and `avifenc`; see Prerequisites.

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

AVIF is what browsers that speak it download (about half the WebP). The three
widths match the `srcset` in `site/worker.js`: 768w is the ~720px slot,
1280w covers phones at 3x and desktops at 1x, 1920w is the 2x desktop.
`-q 40` rather than 50: the capture is flat color and hard edges, and at 1:1
on the densest text the two are indistinguishable, while 40 takes a third off
the phone capture and a quarter off the desktop one. Compare a crop before
moving it further; the gain below 40 is visible.
`dashboard-card.png` is the share card, not a fifth hero candidate: the
`og:image` crawlers fetch one URL and draw it at card size, so it is the
capture at 1200px, the width a `summary_large_image` is laid out at. The
capture is 262 flat colors, so `-colors 128` is not a visible cut and takes
it from 303,865 bytes to 68,924. The full-size PNG stays as the `<img src>`
fallback for a client with neither AVIF nor WebP. `bun test site/` pins the
HTML transfer sizes, the AVIF/WebP byte ceilings and the card's width and
weight, so a recapture that blows the budget fails there.

## Make targets

`make help` lists everything. The ones expected in day-to-day work:

| target | what it does |
|---|---|
| `make build` | host binary with version stamping |
| `make prereqs` | check go, a C compiler, bun and uv against the pins, naming every gap at once |
| `make demo` / `make run` | build, then launch |
| `make test` | all tests, `-race -shuffle=on` (same flags as CI); `RACE=0` skips `-race` |
| `make test-pkg` | one package or test: `PKG=./internal/ui` `[RUN=TestName]` `[TESTTAGS=sqlite]` `[RACE=0]` |
| `make cover` | coverage summary per package into `dist/` |
| `make check` | go.mod tidy-diff + gofmt -s + staticcheck + vet |
| `make ci` | Go merge gates: tidy-diff, fmt, lint, vet, govulncheck, race tests |
| `make pr` | every PR merge gate except the OS matrix: `ci` + `site-lint` + `site-check` + `scripts-check` + `repro-check-pair` |
| `make fmt` | rewrite files with gofmt -s |
| `make fix` | apply `go fix` modernization autofixes, then gofmt |
| `make tidy` | run `go mod tidy` to clean up go.mod and go.sum |
| `make lint` | staticcheck over both halves of the sqlite tag gate |
| `make govulncheck` | `govulncheck` over both sqlite tag halves at the Makefile pin (same pin as CI) |
| `make scripts-check` | black and ruff over `scripts/` (same pins as CI) |
| `make site-lint` | biome format-check and lint over the files `biome.jsonc` includes (the Worker and the jsonc configs) at the Makefile `BIOME` pin (CI parity) |
| `make site-fmt` | rewrite those files with the biome formatter, then re-lint |
| `make site-check` | `bun test site/` |
| `make site-assets` | rebuild the shipped dashboard captures in `site/public/` from `docs/images/dashboard.png`, then run `bun test site/` (needs `magick` and `avifenc`) |
| `make site-deploy` | run `site-lint` and `site-check`, then deploy the site Worker at the `WRANGLER` pin and poll `/health` |
| `make check-wrangler-doc` | fail unless CONTRIBUTING.md's login command names the Makefile's `WRANGLER` pin (`site-deploy` runs it) |
| `make check-ci-tags` | fail unless every `go test` / `go vet` / staticcheck line in `.github/workflows/` carries the zone tag (`make check` runs it) |
| `make site-rollback` | roll the site Worker back to the version before the last deploy, then poll `/health`; a second run with no deploy of this tree to undo is a no-op, and no gate runs, so it works on a tree that does not pass |
| `make vet-cross` | vet + staticcheck on every release platform (the pre-ship gate release.yml runs) |
| `make check-changelog` | verify CHANGELOG.md has release section and link for VERSION |
| `make buildinfo` | write the toolchain, commit, and flags behind `dist/` to a manifest |
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
Worker at the same time. That lock is per checkout: `dist/` is gitignored, so
two clones can each hold it. The platform's own deployment history is what
resolves a genuine collision, through the Cloudflare dashboard's deploy log.

## Before opening a PR

CI (`.github/workflows/ci.yml`) runs gofmt -s and `go mod tidy -diff` on
Linux only, plus `make govulncheck` for both sqlite tag halves on Linux.
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
`make check` and in the Linux leg of the test job. Everything
except the three-OS test matrix and the cross-compile job is one command
locally:

```
make pr
```

That is `make ci` (gofmt, tidy, staticcheck, vet, govulncheck, race tests for
both sqlite tag halves), `make site-lint` (biome formatter and linter over the
Worker and the jsonc configs, at the `BIOME` pin in the Makefile, config in
`biome.jsonc`; run
`make site-fmt` to apply the formatter), `make site-check`
(`bun test site/`), `make scripts-check`, and `make repro-check-pair`.
`scripts-check` installs the exact versions in
`scripts/requirements-dev.txt` into an isolated env under `dist/`
(`make scripts-env`, black, ruff, plus the renderer deps). Pure-Python pins
carry a wheel sha256; bumping one of those lines means updating the hash too,
and the install fails if a fetched file does not match. Do not run unpinned
`uvx black` /
`uvx ruff`: those resolve to whatever PyPI returns today. Platform-specific
files also need `make vet-cross` (the same gate `release.yml` runs before
shipping).

Keep platform-specific code behind build tags or runtime checks; the
cross-compile job catches code that only builds, or only vets and lints
cleanly, on the author's OS.

A new dependency needs an entry in [docs/DEPENDENCIES.md](docs/DEPENDENCIES.md)
with its license and the reason it beats writing the code here;
`cmd/toktop/deps_test.go` fails when a module in go.mod's direct require block
is imported nowhere or has no entry in that file. Check the stdlib and the
platform before adding one.

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

Keep one heading per impact, in the order the file uses: `Breaking`, then
`Added`, `Security`, `Changed`, `Fixed`. A change a sender or a Go caller
would notice belongs under `Breaking` even at 0.x, and it names the before
and after plus what to do about it.

The source stamp is empty. `make build` writes `dev` via `-ldflags
-X main.version=...`; a release tag writes the version with the `v` prefix
stripped. `go install` without ldflags reads the module version Go embeds, so
`--version` and `toktop update` see the tag that was installed, not `dev` and
not a leftover `0.1.0`.

Push a tag `v*`: GitHub Actions tests (both halves of the sqlite tag gate),
cross-compiles every platform, generates checksums, a buildinfo manifest, and
a CycloneDX SBOM, and attaches binaries to the release. The buildinfo file
records the commit, toolchain, and flags behind the bytes, so a rebuild
attempt has something to match; it is listed in checksums.txt too, as is
the SBOM. `make dist-clean` keeps only the files the current `VERSION`
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
-buildvcs=false -mod=readonly -buildmode=pie`. This needs GNU tar; where the
system tar is bsdtar (macOS), install GNU tar as `gtar`.

A tag is a version, not a branch: cut it once and leave it. Moving one re-runs
the release job, and the job refuses to run when a release of that tag already
exists, because replacing the binaries under a version people have installed
and verified breaks the checksum they recorded. Cut a new version instead.

The publish step uploads every top-level file `dist/` holds, so `make release`
first runs `dist-clean`, which deletes the regular files an earlier `make cover`
or a previous local run left behind. Directories such as the site deploy lock
and the nested `dist/bin` and `dist/repro` output are not touched, and
anything named `toktop_*` or `toktop-*` is kept, so a `make -j release` cannot
drop the SBOM another prerequisite just wrote.

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
gh release upload v0.15.0 dist/toktop_0.15.0_* --clobber
make release-verify VERSION=0.15.0
```

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
