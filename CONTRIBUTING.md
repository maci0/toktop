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
- `uv` at or above the version in `.uv-version` for `make scripts-check`
  (CI installs that file's version via `version-file`; black/ruff pins in
  `scripts/requirements-dev.txt`). The target names a too-old uv
  rather than failing on an unknown flag.
- No services or databases: everything is stdlib plus the modules in
  `go.mod`.
- Only to regenerate the README screenshot (below), and never for the
  edit-test loop or any merge gate: `tmux` to capture the live frame,
  `magick` (ImageMagick 7) to resize the PNG into the WebP variants, and
  `avifenc` (libavif) to encode the AVIF ones. Nothing in `make pr` runs
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
make test        # all tests, race detector, shuffled order
make demo        # build and run against a simulated fleet
```

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
uv run --isolated --no-project --with-requirements scripts/requirements.txt \
  scripts/screenshot.py .scratch/capture.txt docs/images/dashboard.png 2 180 50
```

The trailing arguments are scale, columns and rows; passing the pane geometry
keeps the renderer from re-deriving it and wrapping. `VERSION` is stamped into
the header, so pass the version being released rather than `dev`. Rendering
needs a Meslo Nerd Font installed, or `TOKTOP_SCREENSHOT_FONT` pointing at a
regular-weight `.ttf`. The `uv run` flags match `make scripts-check` so uv
does not create a `.venv` from `pyproject.toml`. `-c "$PWD"` is what puts the
pane in this checkout: a detached session otherwise starts wherever the tmux
server did, and `./toktop` is not there.

The image's pixel size is repeated in `site/worker.js` as the `og:image`
dimensions. Copy the PNG into `site/public/dashboard.png` and rebuild the
hero variants the page actually sends:

```
magick docs/images/dashboard.png -strip -resize 1920x -quality 82 \
  site/public/dashboard.webp
magick docs/images/dashboard.png -strip -resize 1280x -quality 82 \
  site/public/dashboard-1280.webp
magick docs/images/dashboard.png -strip -resize 768x -quality 82 \
  site/public/dashboard-768.webp
magick docs/images/dashboard.png -strip -resize 1920x .scratch/hero-1920.png
magick docs/images/dashboard.png -strip -resize 1280x .scratch/hero-1280.png
magick docs/images/dashboard.png -strip -resize 768x .scratch/hero-768.png
avifenc -q 50 -s 2 -y 444 --ignore-exif --ignore-xmp \
  .scratch/hero-1920.png site/public/dashboard.avif
avifenc -q 50 -s 2 -y 444 --ignore-exif --ignore-xmp \
  .scratch/hero-1280.png site/public/dashboard-1280.avif
avifenc -q 50 -s 2 -y 444 --ignore-exif --ignore-xmp \
  .scratch/hero-768.png site/public/dashboard-768.avif
```

AVIF is what browsers that speak it download (about half the WebP). The three
widths match the `srcset` in `site/worker.js`: 768w is the ~720px slot,
1280w covers phones at 3x and desktops at 1x, 1920w is the 2x desktop. The
PNG stays at capture resolution for
share cards. `bun test site/` pins the HTML transfer sizes and the AVIF/WebP
byte ceilings, so a recapture that blows the budget fails there.

## Make targets

`make help` lists everything. The ones expected in day-to-day work:

| target | what it does |
|---|---|
| `make build` | host binary with version stamping |
| `make demo` / `make run` | build, then launch |
| `make test` | all tests, `-race -shuffle=on` (same flags as CI); `RACE=0` skips `-race` |
| `make test-pkg` | one package or test: `PKG=./internal/ui` `[RUN=TestName]` `[TESTTAGS=sqlite]` `[RACE=0]` |
| `make cover` | coverage summary per package into `dist/` |
| `make check` | go.mod tidy-diff + gofmt -s + staticcheck + vet |
| `make ci` | Go merge gates: tidy-diff, fmt, lint, vet, govulncheck, race tests |
| `make pr` | every PR merge gate except the OS matrix: `ci` + `site-lint` + `site-check` + `scripts-check` + `repro-check-pair` |
| `make fmt` / `make format` | rewrite files with gofmt -s |
| `make fix` | apply `go fix` modernization autofixes, then gofmt |
| `make tidy` | run `go mod tidy` to clean up go.mod and go.sum |
| `make lint` | staticcheck over both halves of the sqlite tag gate |
| `make govulncheck` | `govulncheck` over both sqlite tag halves at the Makefile pin (same pin as CI) |
| `make scripts-check` | black and ruff over `scripts/` (same pins as CI) |
| `make site-lint` | biome over `site/` at the Makefile `BIOME` pin (CI parity) |
| `make site-check` | `bun test site/` |
| `make site-deploy` | run `site-lint` and `site-check`, then deploy the site Worker at the `WRANGLER` pin and poll `/health` |
| `make site-rollback` | roll the site Worker back to the version before the last deploy, then poll `/health`; a second run with no deploy of this tree to undo is a no-op, and no gate runs, so it works on a tree that does not pass |
| `make vet-cross` | vet + staticcheck on every release platform (the pre-ship gate release.yml runs) |
| `make check-changelog` | verify CHANGELOG.md has release section and link for VERSION |
| `make buildinfo` | write the toolchain, commit, and flags behind `dist/` to a manifest |
| `make repro-check` | build every release platform twice, from two different source paths and two different build caches, then diff |
| `make repro-check-pair` | the same gate over `REPRO_PLATFORMS`, the pair the PR gate and the release job both build twice |

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
without `-tags sqlite`) are vetted and staticchecked everywhere. Everything
except the three-OS test matrix and the cross-compile job is one command
locally:

```
make pr
```

That is `make ci` (gofmt, tidy, staticcheck, vet, govulncheck, race tests for
both sqlite tag halves), `make site-lint` (biome over the Worker, at the
`BIOME` pin in the Makefile, config in `biome.jsonc`), `make site-check`
(`bun test site/`), `make scripts-check`, and `make repro-check-pair`.
`scripts-check` installs the exact versions in
`scripts/requirements-dev.txt` into an isolated env (black, ruff, plus the
renderer deps). Pure-Python pins carry a wheel sha256; bumping one of those
lines means updating the hash too. Do not run unpinned `uvx black` /
`uvx ruff`: those resolve to whatever PyPI returns today. Platform-specific
files also need `make vet-cross` (the same gate `release.yml` runs before
shipping).

Keep platform-specific code behind build tags or runtime checks; the
cross-compile job catches code that only builds, or only vets and lints
cleanly, on the author's OS.

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
and nothing is left under Unreleased. Run it locally with
`make check-changelog VERSION=0.15.0` before you tag.

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
