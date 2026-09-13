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
- `bun` at the version in `.bun-version` for `make site-check`. The target
  refuses a different version on PATH, matching CI's `bun-version-file`.
- `uv` >= 0.12.6 for `make scripts-check` (CI installs 0.12.6; black/ruff
  pins in `scripts/requirements-dev.txt`). The target names a too-old uv
  rather than failing on an unknown flag.
- No services or databases: everything is stdlib plus the modules in
  `go.mod`.

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
and rendered by `scripts/screenshot.py`:

```
make build VERSION=0.9.0
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
magick docs/images/dashboard.png -strip -resize 1920x .scratch/hero-1920.png
magick docs/images/dashboard.png -strip -resize 1280x .scratch/hero-1280.png
avifenc -q 50 -s 2 -y 444 --ignore-exif --ignore-xmp \
  .scratch/hero-1920.png site/public/dashboard.avif
avifenc -q 50 -s 2 -y 444 --ignore-exif --ignore-xmp \
  .scratch/hero-1280.png site/public/dashboard-1280.avif
```

AVIF is what browsers that speak it download (about half the WebP); 1280w
covers phones and 1x desktops. The PNG stays at capture resolution for
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
| `make pr` | every PR merge gate except the OS matrix: `ci` + `site-check` + `scripts-check` |
| `make fmt` | rewrite files with gofmt -s |
| `make fix` | apply `go fix` modernization autofixes, then gofmt |
| `make lint` | staticcheck over both halves of the sqlite tag gate |
| `make govulncheck` | `govulncheck` at the Makefile pin (same pin as CI) |
| `make scripts-check` | black and ruff over `scripts/` (same pins as CI) |
| `make site-check` | `bun test site/` |
| `make vet-cross` | vet + staticcheck on every release platform (the pre-ship gate release.yml runs) |

## Before opening a PR

CI (`.github/workflows/ci.yml`) runs gofmt -s, `go mod tidy -diff`, and
`govulncheck ./...` on Linux only (all three are platform-independent),
`staticcheck` and `go vet ./...` and `go test -race -shuffle=on ./...` on
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
both sqlite tag halves), `make site-check` (`bun test site/`), and
`make scripts-check`. `scripts-check` installs the exact versions in
`scripts/requirements-dev.txt` into an isolated env (black, ruff, plus the
renderer deps). Pure-Python pins carry a wheel sha256; bumping one of those
lines means updating the hash too. Do not run unpinned `uvx black` /
`uvx ruff`: those resolve to whatever PyPI returns today. Platform-specific
files also need `make vet-cross` (the same gate `release.yml` runs before
shipping).

Keep platform-specific code behind build tags or runtime checks; the
cross-compile job catches code that only builds, or only vets and lints
cleanly, on the author's OS.

## Releases

Versions are 0.x: the CLI flags, the ingest `/v1/events` body, and the
`agentusage` Go API may change without a major bump. Move the Unreleased
section in [CHANGELOG.md](CHANGELOG.md) under the new version before tagging.

The source stamp is empty. `make build` writes `dev` via `-ldflags
-X main.version=...`; a release tag writes the version with the `v` prefix
stripped. `go install` without ldflags reads the module version Go embeds, so
`--version` and `toktop update` see the tag that was installed, not `dev` and
not a leftover `0.1.0`.

Push a tag `v*`: GitHub Actions tests (both halves of the sqlite tag gate),
cross-compiles every platform, generates checksums and a CycloneDX SBOM, and
attaches binaries to the release. The host-platform artifact is smoke-tested
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
