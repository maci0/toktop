BINARY  := toktop
CMD     := ./cmd/toktop
DIST    := dist
VERSION ?= dev
# VERSION is interpolated into -ldflags and dist filenames. Refuse values
# that would break the shell, the linker flag, or the artifact name.
CHECK_VERSION = printf '%s' '$(VERSION)' | grep -qE '^[A-Za-z0-9._+-]+$$' || { echo "make: VERSION must match [A-Za-z0-9._+-]+ (got '$(VERSION)')" >&2; exit 1; }
# A cut leaves an empty '## [Unreleased]' stub, and the stub is a heading
# without a version. Only a versioned heading closes the section and names the
# version the bump is compared against, so a stub anywhere below the new
# section cannot pass for the release that preceded it.
CHECK_CHANGELOG = if [ '$(VERSION)' != 'dev' ]; then \
	awk -v v='$(VERSION)' 'index($$0, "\#\# [" v "] ") == 1 || $$0 == "\#\# [" v "]" {f=1} END{exit !f}' CHANGELOG.md || { echo "make: CHANGELOG.md missing '\#\# [$(VERSION)]' section" >&2; exit 1; }; \
	awk -v v='$(VERSION)' 'index($$0, "[Unreleased]: ") == 1 {f=1} END{exit !f}' CHANGELOG.md || { echo "make: CHANGELOG.md missing '[Unreleased]:' link reference" >&2; exit 1; }; \
	awk '$$0 == "\#\# [Unreleased]" {f=1} END{exit !f}' CHANGELOG.md || { echo "make: CHANGELOG.md is missing the '\#\# [Unreleased]' stub the next release fills" >&2; exit 1; }; \
	awk -v v='$(VERSION)' '(index($$0, "\#\# [" v "] ") == 1 || $$0 == "\#\# [" v "]") && $$0 !~ /- [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]/ {d=1} END{exit d}' CHANGELOG.md || { echo "make: CHANGELOG.md '\#\# [$(VERSION)]' heading carries no release date" >&2; exit 1; }; \
	awk -v v='$(VERSION)' 'index($$0, "[" v "]: ") == 1 {f=1} END{exit !f}' CHANGELOG.md || { echo "make: CHANGELOG.md missing '[$(VERSION)]:' link reference" >&2; exit 1; }; \
	awk -v v='$(VERSION)' 'index($$0, "[" v "]: ") == 1 && $$0 !~ "compare/.*v" v "$$" {f=1} END{exit f}' CHANGELOG.md || { echo "make: CHANGELOG.md '[$(VERSION)]:' link does not end at tag v$(VERSION)" >&2; exit 1; }; \
	awk '/^\#\# \[Unreleased\]/{f=1;next} /^\#\# \[/{f=0} f && /^- /{n++} END{exit (n>0)}' CHANGELOG.md || { echo "make: CHANGELOG.md still has entries under [Unreleased]; move them under [$(VERSION)] first" >&2; exit 1; }; \
	awk -v v='$(VERSION)' 'index($$0, "\#\# [" v "]") == 1 {f=1;next} /^\#\# \[/{f=0} f && /^- /{n++} END{exit (n==0)}' CHANGELOG.md || { echo "make: CHANGELOG.md section for $(VERSION) has no entries; a release ships notes or does not ship" >&2; exit 1; }; \
	awk -v v='$(VERSION)' 'index($$0, "\#\# [" v "]") == 1 {f=1;next} /^\#\# \[/{f=0} f && /^\#\#\# /{if (seen[$$0]++) d=1} END{exit (d==1)}' CHANGELOG.md || { echo "make: CHANGELOG.md section for $(VERSION) repeats an impact heading; one heading per impact" >&2; exit 1; }; \
	awk -v v='$(VERSION)' 'BEGIN{ if (v !~ /^[0-9]+\.[0-9]+\.[0-9]+$$/) exit 0 } \
		index($$0, "\#\# [" v "]") == 1 {f=1; next} \
		/^\#\# \[/ {if (f==1 && $$0 != "\#\# [Unreleased]") {f=0; if (match($$0, /\#\# \[[0-9]+\.[0-9]+\.[0-9]+\]/)) prev=substr($$0, RSTART+4, RLENGTH-5)} next} \
		f==1 && $$0 == "\#\#\# Breaking" {breaking=1} \
		END { \
			if (!breaking || prev == "") exit 0; \
			split(v, a, "."); split(prev, b, "."); \
			exit (a[1] == b[1] && a[2] == b[2]) \
		}' CHANGELOG.md || { echo "make: CHANGELOG.md section for $(VERSION) carries a 'Breaking' entry but $(VERSION) is a patch bump; the project is 0.x, so a breaking change rides a minor bump" >&2; exit 1; }; \
fi

GO          ?= go
# go.mod's go line is the compiler pin. GOTOOLCHAIN=auto would keep a newer
# host toolchain (and its GOEXPERIMENT defaults), so two machines would emit
# different binaries from the same source.
GO_VERSION  := $(shell awk '/^go / { print $$2; exit }' go.mod)
ifeq ($(GO_VERSION),)
$(error go.mod has no 'go' line; cannot pin GOTOOLCHAIN)
endif
export GOTOOLCHAIN := go$(GO_VERSION)
export GOWORK := off
# Instruction-set baselines: an ambient GOAMD64=v3 would change amd64 artifacts.
export GOAMD64 := v1
export GOARM64 := v8.0
# Race tests turn cgo on in their recipes. Everything else matches the
# released artifacts, including vet/staticcheck so they analyze the same
# net resolver the binaries ship.
export CGO_ENABLED := 0
# Drop ambient GOFLAGS/GOEXPERIMENT so a developer's shell cannot change
# the artifact (GOFLAGS=-race on make build, extra experiments on codegen).
export GOFLAGS :=
export GOEXPERIMENT :=
# Strip paths, omit git stamps (checkout vs tarball would disagree), honor
# go.sum, produce a PIE.
GO_BUILDFLAGS := -trimpath -buildvcs=false -mod=readonly -buildmode=pie
STATICCHECK := $(GO) tool staticcheck
# go run @version, not a go.mod tool: govulncheck's module graph is newer than
# staticcheck's (x/tools, x/mod) and would force those up if it joined the
# tool block. One string so Makefile and CI cannot drift.
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.7.0
SBOM_TOOL   := github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.12.0
# bunx, not a package.json: the Worker ships with no npm dependencies, and a
# manifest plus lockfile would exist only to pin this one linter.
BIOME_VERSION := 2.5.14
BIOME         := @biomejs/biome@$(BIOME_VERSION)
# Cloudflare deploy tool for site/. Deploying with whatever `wrangler` a
# machine happens to have installed (or a bare `cf deploy`) makes the upload
# depend on PATH, so the pin is named here and every deploy path reads it.
WRANGLER    := 4.126.0
# The Worker answers /health with `ok`; site-deploy and site-rollback poll it
# until the site is answering `ok` at all, or give up. The body names no
# version, so this is an availability check, not a confirmation that this
# tree is what is serving.
SITE_HEALTH_URL   := https://toktop.ai/health
SITE_HEALTH_TRIES := 6
SITE_HEALTH_WAIT  := 10
# Serializes site-deploy against site-deploy and site-rollback. Under dist/,
# which .gitignore already covers, so a released lock is never committed.
SITE_LOCK         := $(DIST)/site.lock
# `wrangler rollback` with no argument undoes whichever deployment is most
# recent, whoever shipped it. That is the one operation here whose second run
# does damage: a rollback of a rollback puts the version that broke back on
# the site, at the exact moment somebody is trying to get away from it. These
# two record whether a deploy from this tree is still waiting to be undone, so
# the second rollback finds nothing of this tree's to undo and says so.
# Directories, like the lock: dist-clean deletes the top-level files a release
# must not ship and leaves these alone, and `make clean` takes them with
# dist/.
SITE_DEPLOYED     := $(DIST)/site.deployed
SITE_ROLLED_BACK  := $(DIST)/site.rolled-back
# -bindnow is the Go spelling of -Wl,-z,now: without it the linux ELF ships
# partial RELRO, because the internal linker (CGO stays off) emits DT_BIND_NOW
# for nothing. A no-op on darwin and windows, so one LDFLAGS covers PLATFORMS.
# Empty -buildid= so the GNU build-id note is not a second, toolchain-hash-shaped
# input to the bytes.
LDFLAGS     := -s -w -buildid= -bindnow -X main.version=$(VERSION)
# gofmt from the selected toolchain, not a different major on PATH.
GOFMT = $$($(GO) env GOROOT)/bin/gofmt

# nounset/errexit/pipefail on every recipe. bash because Debian's /bin/sh
# is dash, which has no pipefail; macOS /bin/bash 3.2 does.
SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

# crush and opencode keep sessions in SQLite rather than JSONL, so reading
# them means linking a database driver. Released binaries carry it (pure Go,
# so cross-compilation is unaffected). opencode is read when the tag is on and
# --opencode-db, on by default, has not been disabled with
# --opencode-db=false; crush is read whenever the tag is on, because its
# database lives inside the watched project. Build with TAGS= to leave the
# driver out entirely.
TAGS    ?= sqlite
# timetzdata embeds the IANA zone database in every binary. The header clock,
# the feed timestamps and the text report all render through time.Local
# (internal/ui/header.go, feed.go, plain.go), and a released binary resolves
# that against the host's zone files: with none present (a scratch or distroless
# container, a user who installed the download and has no Go toolchain) the Go
# runtime falls back to UTC in silence, so an operator in Europe/Warsaw reads a
# clock two hours behind and a feed whose events are stamped at instants they
# never sent. Embedding the database costs about 440 KiB and removes both the
# missing files and the builder's tzdata version from the artifact, and the
# toolchain that supplies it is already pinned by GOTOOLCHAIN above. The zone
# files on a host that has them still win, so nothing changes for a normal
# desktop install.
# It is a build tag of its own, never part of TAGS: TAGS is the driver gate
# (`make TAGS=` builds without SQLite), and a build that silently lost its zone
# database to that switch would read as a deliberate choice.
ZONE_TAG := timetzdata
# go's -tags is one flag, so a second -tags would replace the first rather
# than add to it; the two lists are joined into a single value.
GOTAGS  := -tags "$(strip $(TAGS) $(ZONE_TAG))"
# The other half of the two-tag gate: the same run without TAGS, so the zone
# database is in the test binary too and a test cannot pass against a runtime
# that resolves time.Local differently from the release.
GOTAGS_BARE := -tags "$(ZONE_TAG)"
# Race detector is on by default so the loop matches `make test` / CI.
# RACE=0 skips it (and the C compiler) for a faster edit cycle.
RACE    ?= 1
race_flag = $(if $(filter 0,$(RACE)),,-race )
# Lower bound for the targets that install the Python tool env
# (`make scripts-check`, `make check-yaml`), read from the uv line of
# .tool-versions. That file is what CI installs (setup-uv version-file), so one
# string covers both; a newer uv on PATH is fine. setup-uv v10 parses only the
# formats it documents (uv.toml, pyproject.toml, .tool-versions, requirements
# files, uv.lock), and .tool-versions is the one that carries a bare version.
UV_MIN := $(shell awk '$$1 == "uv" { print $$2; exit }' .tool-versions 2>/dev/null)
ifeq ($(UV_MIN),)
$(error .tool-versions has no uv line; scripts-check, check-yaml and CI need a uv version)
endif
# The Python tool env, built under dist/ (gitignored) by `uv pip install`.
# Not `uv run --with-requirements`: that resolves and installs the same pins
# but discards the `--hash` lines in the requirements files, so a swapped file
# on the index installed silently. `uv pip install` verifies each hash it is
# given, which is what scripts/requirements-dev.txt documents.
# --no-deps because the requirements files already spell out black's and
# ruff's closure. Without it a tool that grows a dependency has it resolved
# and installed at whatever the index serves that minute, unhashed and
# unreviewed, which is the one install the exact pins do not govern. With it
# the list is the whole closure, and a tool whose requirements outgrow the
# file fails at the first run instead of pulling a package nobody pinned.
SCRIPTS_ENV := $(CURDIR)/$(DIST)/scripts-env
SCRIPTS_BIN := $(SCRIPTS_ENV)/bin
# True when $1 is a uv version below UV_MIN. Defined once so `make
# scripts-check` and `make prereqs` cannot accept different uv. One line, so
# it drops into a recipe that is a single continued command.
#
# Compared field by field in awk, not with `sort -V`: version sort is a GNU
# extension, and where BSD sort (macOS) rejects it the pipeline printed
# nothing, which read as "uv is too old" and failed every macOS run of
# prereqs and scripts-check. awk is POSIX and already used by these recipes.
UV_TOO_OLD = uv_too_old() { awk -v min='$(UV_MIN)' -v have="$$1" 'BEGIN { n = split(min, a, "."); m = split(have, b, "."); for (i = 1; i <= (n > m ? n : m); i++) { x = a[i] + 0; y = b[i] + 0; if (y < x) exit 0; if (y > x) exit 1 } exit 1 }'; }
# bun's exact pin, from the file CI installs (bun-version-file), so
# require-bun and prereqs compare against the same string.
BUN_PIN := $(shell tr -d ' \t\r\n' < .bun-version 2>/dev/null)
ifeq ($(BUN_PIN),)
$(error .bun-version missing or empty; site-check and CI need a bun version)
endif
# The interpreter the Python tool env is built on, exact like the other
# toolchains. `uv venv` without --python takes the newest interpreter on the
# host, so the exact requirement pins fixed what is installed into the env
# but not the interpreter it went into, and a format or lint verdict could
# move with the host's Python. uv reads .python-version on its own; naming it
# here too makes the Makefile the enforcement point and turns a missing file
# into a named error instead of a silent fallback, and uv downloads the pinned
# build when the host does not have it, which is what GOTOOLCHAIN does for the
# Go toolchain.
PY_PIN := $(shell tr -d ' \t\r\n' < .python-version 2>/dev/null)
ifeq ($(PY_PIN),)
$(error .python-version missing or empty; scripts-env and scripts-check need a Python version)
endif
# Where make site-assets writes the captures, and the checked-in record of the
# encoder builds that produced the ones now there. The record sits beside the
# assets rather than inside them, so it is a build record and not a file the
# Worker serves.
SITE_PUBLIC    := site/public
DOCS_IMAGES    := docs/images
ENCODER_RECORD := site/encoders.txt

# Pin locale and timezone for every recipe: glob expansion order and formatted
# dates must not follow the invoking shell's environment into artifacts
# (reproducible-builds.org practice).
export LC_ALL := C
export TZ := UTC
# gzip concatenates $GZIP with argv (a user's -9 would change the tarball);
# GNU tar does the same with $TAR_OPTIONS.
export GZIP :=
export TAR_OPTIONS :=

# One timestamp for archive metadata: SOURCE_DATE_EPOCH when the caller sets
# it, otherwise this commit's time, so two builds of the same source agree.
# Exported so gzip/tar/any honoring tool sees the same value, not only Make.
export SOURCE_DATE_EPOCH ?= $(shell git log -1 --pretty=%ct 2>/dev/null || echo 0)

# Normalizing tar metadata (--sort/--mtime/--owner/--mode) needs GNU tar;
# bsdtar cannot express it. Where the system tar is bsdtar (macOS) GNU tar
# is often installed as gtar, so prefer that name when it exists.
# --mode=0644: checksums.txt mode otherwise follows the builder's umask.
# --pax-option: drop atime/ctime PAX headers GNU tar may emit.
TAR       := $(shell command -v gtar >/dev/null 2>&1 && echo gtar || echo tar)
TAR_REPRO := --sort=name --mtime="@$(SOURCE_DATE_EPOCH)" --owner=0 --group=0 --numeric-owner --mode=0644 --pax-option=exthdr.name=%d/PaxHeaders/%f,delete=atime,delete=ctime

PLATFORMS := \
	linux/amd64 linux/arm64 \
	darwin/amd64 darwin/arm64 \
	windows/amd64 windows/arm64

# The subset the merge gate builds twice. Every platform in PLATFORMS is built
# before a release, but the per-PR gate pays for two of them: the point is to
# catch a timestamp or path leak, and any pair surfaces one. ci.yml calls
# repro-check-pair, so the PR job and the local `make pr` cannot pick
# different platforms.
REPRO_PLATFORMS ?= linux/amd64 windows/amd64

# The repository `release-verify` reads a published release from. One string so
# the fork that publishes elsewhere changes it in one place, the way WRANGLER
# and BIOME_VERSION are changed in one place. The release job uses the
# workflow's own repository, so a fork's tag push verifies its own releases
# without this needing to be right there.
RELEASE_REPO ?= $(shell $(GO) list -m)

.DEFAULT_GOAL := help

# Width of the target column in `make help`, one past the longest target
# (check-release-source). A narrower column pushes the longest names'
# descriptions out of alignment, and alignment is the one thing a help listing
# has to get right.
HELP_WIDTH := 21

.PHONY: help
help: ## show available targets
	@if [ -t 1 ] && [ -z "$${NO_COLOR:-}" ] && [ "$${TERM:-}" != "dumb" ]; then \
		color=1; \
	else \
		color=; \
	fi; \
	grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | \
		awk -v color="$$color" -v width="$(HELP_WIDTH)" 'BEGIN {FS = ":.*## "} { if (color) printf "  \033[36m%-*s\033[0m %s\n", width, $$1, $$2; else printf "  %-*s %s\n", width, $$1, $$2 }'

# One run over every tool a gate needs, so a contributor installs the set
# before the first failure rather than one missing tool per round trip. The
# per-target checks below still fire where the tool is actually used: this
# reports, it does not replace them. Every gap is listed, then one verdict.
#
# The C compiler check mirrors NEED_CC rather than re-deriving it: a set CC is
# the compiler and the only one the race tests will use, with no fallback to
# gcc or clang. Testing the fallback as well reported ok for a CC that does not
# exist, so prereqs cleared a machine that make test then refused to build.
.PHONY: prereqs
prereqs: ## check every tool the merge gates need, naming all gaps at once
	@fail=0; \
	ok() { printf '  ok       %s\n' "$$1"; }; \
	gap() { printf '  MISSING  %s\n' "$$1" >&2; fail=1; }; \
	$(UV_TOO_OLD); \
	if command -v $(GO) >/dev/null 2>&1; then \
		ok "go $$($(GO) env GOVERSION) (go.mod pins $(GO_VERSION); make selects it)"; \
	else \
		gap "go is not on PATH; install the version go.mod pins ($(GO_VERSION))"; \
	fi; \
	if [ "$(RACE)" = "0" ]; then \
		ok "C compiler not needed (RACE=0 skips the race detector)"; \
	elif [ -n "$${CC:-}" ]; then \
		if command -v "$${CC}" >/dev/null 2>&1; then \
			ok "C compiler for 'go test -race' ($$CC)"; \
		else \
			gap "CC is set to '$$CC', which is not on PATH; unset it to use gcc or clang"; \
		fi; \
	elif command -v gcc >/dev/null 2>&1 || command -v clang >/dev/null 2>&1; then \
		ok "C compiler for 'go test -race' (gcc or clang)"; \
	else \
		gap "no C compiler (gcc or clang) for the race tests in 'make test'; pass RACE=0 to skip them"; \
	fi; \
	if command -v bun >/dev/null 2>&1; then \
		have=$$(bun --version); \
		if [ "$$have" = "$(BUN_PIN)" ]; then \
			ok "bun $$have (matches .bun-version)"; \
		else \
			gap "bun $$have on PATH, .bun-version pins $(BUN_PIN)"; \
		fi; \
	else \
		gap "bun is not on PATH (make site-check and site-lint need $(BUN_PIN))"; \
	fi; \
	if command -v uv >/dev/null 2>&1; then \
		have=$$(uv --version | awk '{print $$2}'); \
		if uv_too_old "$$have"; then \
			gap "uv $$have on PATH, make scripts-check and check-yaml need >= $(UV_MIN)"; \
		else \
			ok "uv $$have (>= $(UV_MIN))"; \
		fi; \
	else \
		gap "uv is not on PATH (make scripts-check and check-yaml need >= $(UV_MIN))"; \
	fi; \
	if [ "$$fail" != "0" ]; then \
		echo "make prereqs: install the MISSING tools above; CONTRIBUTING.md 'Prerequisites' explains each" >&2; \
		exit 1; \
	fi; \
	echo "make prereqs: every tool the merge gates need is present"

# CGO stays off so the host build matches the released artifacts exactly;
# with cgo available the net package links host-specific resolver code and
# two developer machines produce different binaries from identical source.
.PHONY: build
build: ## build the toktop binary for this host
	@$(CHECK_VERSION)
	CGO_ENABLED=0 $(GO) build $(GOTAGS) $(GO_BUILDFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD)

.PHONY: run
run: build ## build, then run against local engines
	./$(BINARY)

.PHONY: demo
demo: build ## build, then run the simulated fleet
	./$(BINARY) --demo

# -race links runtime/cgo. Honor CC when set; otherwise gcc, then clang
# (Go's search order). make build keeps CGO off; only race tests need this.
NEED_CC = cc="$${CC:-}"; \
	if [ -z "$$cc" ]; then \
		if command -v gcc >/dev/null 2>&1; then cc=gcc; \
		elif command -v clang >/dev/null 2>&1; then cc=clang; \
		fi; \
	fi; \
	if [ -z "$$cc" ] || ! command -v "$$cc" >/dev/null 2>&1; then \
		echo "make: go test -race needs a C compiler (gcc or clang) on PATH" >&2; \
		echo "  CGO stays off for make build; only the race tests need it." >&2; \
		echo "  Pass RACE=0 to skip race detection: make test RACE=0" >&2; \
		exit 1; \
	fi

.PHONY: test
test: ## run all tests shuffled (both sqlite tag halves); RACE=0 skips -race
	@if [ "$(RACE)" != "0" ]; then $(NEED_CC); fi
	CGO_ENABLED=$(if $(filter 0,$(RACE)),0,1) $(GO) test -mod=readonly $(GOTAGS_BARE) $(race_flag)-shuffle=on ./...
	CGO_ENABLED=$(if $(filter 0,$(RACE)),0,1) $(GO) test -mod=readonly $(GOTAGS) $(race_flag)-shuffle=on ./agentusage/...

# Same flags and toolchain as `make test`. PKG is required; RUN (or TEST) and
# TESTTAGS are optional. Unset TESTTAGS on ./agentusage runs both halves of the
# sqlite tag gate (matching `make test`); TESTTAGS=sqlite (or another tag) runs one.
# RACE=0 drops -race for a faster edit loop; default matches CI.
#
# RUN_TO_CHECK is RUN/TEST only when it can be checked against the test binary's
# name list: a pattern with a `/` selects subtests, and -list reports top-level
# names only. The run gets RUN_PATTERN either way.
RUN_PATTERN  := $(or $(RUN),$(TEST))
RUN_TO_CHECK := $(if $(findstring /,$(RUN_PATTERN)),,$(RUN_PATTERN))
.PHONY: test-pkg
test-pkg: ## one package/test: PKG=./internal/ui [RUN=TestName] [TESTTAGS=sqlite] [RACE=0]
	@if [ -z "$(PKG)" ]; then \
		echo "make test-pkg: set PKG (e.g. PKG=./internal/ui)" >&2; \
		echo "  optional: RUN=TestName (or TEST=TestName)  TESTTAGS=sqlite  RACE=0" >&2; \
		exit 1; \
	fi
	@if [ "$(RACE)" != "0" ]; then $(NEED_CC); fi
	@if [ -n "$(RUN_TO_CHECK)" ]; then $(CHECK_RUN_MATCHES); fi
	CGO_ENABLED=$(if $(filter 0,$(RACE)),0,1) $(GO) test -mod=readonly -tags "$(strip $(TESTTAGS) $(ZONE_TAG))" $(race_flag)-shuffle=on $(if $(RUN_PATTERN),-run "$(RUN_PATTERN)" )"$(PKG)"
	@if [ -n "$(BOTH_HALVES)" ]; then \
		echo "make test-pkg: also running the tagged half (set TESTTAGS to run one half)"; \
		CGO_ENABLED=$(if $(filter 0,$(RACE)),0,1) $(GO) test -mod=readonly $(GOTAGS) $(race_flag)-shuffle=on $(if $(RUN_PATTERN),-run "$(RUN_PATTERN)" )"$(PKG)" || exit 1; \
	fi

# The package names under which an unset TESTTAGS means "run both halves of the
# sqlite tag gate". Named once, read by test-pkg and CHECK_RUN_MATCHES, so the
# second half cannot be dropped from one and kept in the other.
BOTH_HALVES = $(if $(TESTTAGS),,$(filter \
	./agentusage ./agentusage/ ./agentusage/... agentusage \
	github.com/maci0/toktop/agentusage github.com/maci0/toktop/agentusage/ \
	github.com/maci0/toktop/agentusage/...,$(PKG)))

# `go test -run` exits 0 and prints "[no tests to run]" when the pattern
# matches nothing, so a mistyped or renamed RUN reads as a passing run. Ask
# the test binary for its names first, with the same regexp the run gets, and
# fail with the near misses. Skipped for a pattern carrying a `/`: that selects
# subtests, and -list only reports top-level names, so the check would reject a
# run that does select something.
#
# TEST_HALF is the tags of the half under test, set by the caller; the second
# line is the sqlite half, which only runs when TESTTAGS is unset.
#
# The tag list is one -tags argument, so $$tags stays quoted: unquoted it
# split "sqlite timetzdata" into two words and go read the second as a
# package pattern, the listing failed, and every RUN against a half with
# more than one tag was refused as matching no test.
define CHECK_RUN_MATCHES
matched() { tags="$(ZONE_TAG)"; [ -n "$$1" ] && tags="$$1 $(ZONE_TAG)"; $(GO) test -mod=readonly -tags "$$tags" -list "$(RUN_PATTERN)" "$(PKG)" 2>/dev/null | grep -E '^(Test|Example|Benchmark|Fuzz)' || true; }; \
	names=$$(matched "$(TESTTAGS)"); \
	if [ -z "$$names" ] && [ -n "$(BOTH_HALVES)" ]; then names=$$(matched sqlite); fi; \
	if [ -z "$$names" ]; then \
		echo "make test-pkg: no test in $(PKG) matches RUN=$(RUN_PATTERN); the run would report success without testing anything" >&2; \
		all=$$($(GO) test -mod=readonly -tags "$(strip $(TESTTAGS) $(ZONE_TAG))" -list '.*' "$(PKG)" 2>/dev/null | grep -Eo '^(Test|Example|Benchmark|Fuzz)[A-Za-z0-9_]*' || true); \
		if [ -n "$(BOTH_HALVES)" ]; then all=$${all}$$'\n'$$($(GO) test -mod=readonly $(GOTAGS) -list '.*' "$(PKG)" 2>/dev/null | grep -Eo '^(Test|Example|Benchmark|Fuzz)[A-Za-z0-9_]*' || true); fi; \
		near=$$(printf '%s\n' "$$all" | grep -F "$(RUN_PATTERN)" || true); \
		if [ -n "$$near" ]; then echo "  close: $$near" >&2; fi; \
		echo "  list the names with: $(GO) test -mod=readonly -tags \"$(strip $(TESTTAGS) $(ZONE_TAG))\" -list '.*' $(PKG)" >&2; \
		if [ -n "$(BOTH_HALVES)" ]; then echo "  (TESTTAGS is unset, so that lists the untagged half; the sqlite half can hold more: $(GO) test -mod=readonly $(GOTAGS) -list '.*' $(PKG))" >&2; fi; \
		exit 1; \
	fi
endef

.PHONY: cover
cover: ## test coverage summary per package into dist/
	@if [ "$(RACE)" != "0" ]; then $(NEED_CC); fi
	mkdir -p $(DIST)
	CGO_ENABLED=$(if $(filter 0,$(RACE)),0,1) $(GO) test -mod=readonly $(GOTAGS) $(race_flag)-shuffle=on -coverprofile=$(DIST)/coverage.out ./...
	$(GO) tool cover -func=$(DIST)/coverage.out | tail -1

.PHONY: sbom
sbom: ## generate CycloneDX SBOM of all dependencies into dist/
	@$(CHECK_VERSION)
	mkdir -p $(DIST)
	$(GO) run $(SBOM_TOOL) \
		mod -licenses -std -noserial -notimestamp -json -output $(DIST)/toktop-sbom-$(VERSION).cdx.json .

# -tests=true turns on vet's tests analyzer, which is off by default. It reads
# the _test.go files for a Test/Fuzz/Benchmark/Example whose name and signature
# do not match what `go test` runs: a mis-signed case is never executed and the
# package still reports ok. The tree passes it on both halves, so it is on
# rather than ratcheted. ci.yml spells its go vet lines out, and check-ci-tags
# is what keeps them from drifting; the flag goes there too.
VET_TESTS := -tests=true

.PHONY: vet
vet: ## run go vet (both halves of the sqlite tag gate)
	$(GO) vet -mod=readonly $(VET_TESTS) $(GOTAGS_BARE) ./...
	$(GO) vet -mod=readonly $(VET_TESTS) $(GOTAGS) ./agentusage/...

# Same per-platform gate release.yml runs before shipping; PLATFORMS is the
# single source of truth so CI and local checks cannot list different targets.
# staticcheck is built for the host once: installing with GOOS set would
# produce a tool binary for the analyzed platform instead of the analyzer.
.PHONY: vet-cross
vet-cross: ## vet + staticcheck every release platform from PLATFORMS
	@mkdir -p $(DIST)/bin
	@env GOBIN=$(CURDIR)/$(DIST)/bin $(GO) install tool || exit 1
	@for target in $(PLATFORMS); do \
		goos=$${target%/*}; goarch=$${target#*/}; \
		echo "checking $$goos/$$goarch"; \
		env GOOS=$$goos GOARCH=$$goarch $(GO) vet -mod=readonly $(VET_TESTS) $(GOTAGS_BARE) ./... || exit 1; \
		env GOOS=$$goos GOARCH=$$goarch $(GO) vet -mod=readonly $(VET_TESTS) $(GOTAGS) ./agentusage/... || exit 1; \
		env GOOS=$$goos GOARCH=$$goarch $(DIST)/bin/staticcheck $(GOTAGS_BARE) ./... || exit 1; \
		env GOOS=$$goos GOARCH=$$goarch $(DIST)/bin/staticcheck $(GOTAGS) ./agentusage/... || exit 1; \
	done
	@rm -rf $(DIST)/bin

.PHONY: lint
lint: ## run staticcheck (both halves of the sqlite tag gate)
	$(STATICCHECK) $(GOTAGS_BARE) ./...
	$(STATICCHECK) $(GOTAGS) ./agentusage/...

.PHONY: govulncheck
govulncheck: ## run govulncheck at the GOVULNCHECK pin (same pin as CI)
	$(GO) run $(GOVULNCHECK) ./...
	$(GO) run $(GOVULNCHECK) $(GOTAGS) ./...

# The dashboard captures under site/public are build outputs of
# docs/images/dashboard.png, and site/worker.js names every one of them (the
# srcset widths, the share-card path, the og:image size). A recapture that
# renames or drops one of them does not fail a build: worker.js keeps serving
# a 404 and the byte ceilings in worker.test.js still pass on the files that
# remain. This is that recipe, so the shipped set and the documented set
# cannot drift.
#
# The encoders are not the project's package manager, so their versions are
# recorded instead of pinned: -strip drops the metadata that would otherwise
# carry the encoder version and a build clock into every byte, and the record
# says which builds produced the captures now committed under site/public.
# site-assets writes $(ENCODER_RECORD) after every rebuild and require-encoders
# refuses a machine whose encoders disagree with it, so a recapture that
# silently rewrites every committed capture with a different encoder's bytes
# is a named failure rather than a diff nobody attributes. magick and avifenc
# stay out of prereqs and out of every gate: they are only needed to
# recapture, so a machine without them still passes the whole merge path.
#
# `sed -n 1p`, not `head -1`: head closes the pipe on its first line, and
# pipefail turns the encoder's SIGPIPE into a failed recipe.
ENCODER_VERSIONS = magick -version 2>&1 | sed -n 1p; avifenc --version 2>&1 | sed -n 1p
.PHONY: require-encoders
require-encoders:
	@for tool in magick avifenc; do \
		command -v $$tool >/dev/null 2>&1 || { \
			echo "make site-assets, make readme-assets: $$tool is not on PATH (ImageMagick 7 and libavif); see CONTRIBUTING.md 'Prerequisites'" >&2; \
			exit 1; \
		}; \
	done
	@if [ -f $(ENCODER_RECORD) ]; then \
		have=$$({ $(ENCODER_VERSIONS); } | tr -d '\r'); \
		want=$$(tr -d '\r' < $(ENCODER_RECORD)); \
		if [ "$$have" != "$$want" ]; then \
			echo "make site-assets, make readme-assets: the encoders on this machine are not the ones that produced the captures in $(SITE_PUBLIC) and $(DOCS_IMAGES):" >&2; \
			printf '%s\n' "$$have" | sed 's/^/  have: /' >&2; \
			printf '%s\n' "$$want" | sed 's/^/  want: /' >&2; \
			echo "  install those builds to recapture byte-identically, or delete $(ENCODER_RECORD) and" >&2; \
			echo "  run 'make site-assets' to record the encoders this machine has and commit the result." >&2; \
			exit 1; \
		fi; \
	fi

.PHONY: site-assets
site-assets: require-encoders require-bun ## rebuild the site dashboard captures from docs/images/dashboard.png
	@mkdir -p $(DIST)
	cp docs/images/dashboard.png $(SITE_PUBLIC)/dashboard.png
	magick docs/images/dashboard.png -strip -resize 1920x -quality 82 $(SITE_PUBLIC)/dashboard.webp
	magick docs/images/dashboard.png -strip -resize 1280x -quality 82 $(SITE_PUBLIC)/dashboard-1280.webp
	magick docs/images/dashboard.png -strip -resize 768x -quality 82 $(SITE_PUBLIC)/dashboard-768.webp
	magick docs/images/dashboard.png -strip -resize 1200x -colors 128 PNG8:$(SITE_PUBLIC)/dashboard-card.png
	# avifenc -q 32 is the measured floor for this capture: 10,577 bytes at
	# 768w against 13,563 at -q 40, a 22% cut of the image that is 79% of a
	# phone's visit, at 30.0 dB PSNR against the resized source. The page
	# draws that candidate into about 662 device pixels, so the browser
	# downscales it, and a 2x crop of the 768w frame shows no artifact a
	# reader would see. 4:2:0 was measured too and is not smaller here: the
	# frame is mostly flat dark background, so there is little chroma to
	# subsample, and it costs luma detail on the text that is the picture.
	@for width in 1920 1280 768; do \
		stem=$$( [ "$$width" = 1920 ] && echo dashboard || echo "dashboard-$$width" ); \
		magick docs/images/dashboard.png -strip -resize $${width}x $(DIST)/$$stem.png; \
		avifenc -q 32 -s 2 -y 444 --ignore-exif --ignore-xmp \
			$(DIST)/$$stem.png $(SITE_PUBLIC)/$$stem.avif; \
	done
	@{ $(ENCODER_VERSIONS); } | tr -d '\r' > $(ENCODER_RECORD)
	@bun test site/

# The README's capture, and only that one. GitHub's renderer drops srcset and
# picture, so the repository front page gets exactly the image the README
# names, at whatever size that file is: the 3240px PNG it used to name was
# 303,865 bytes on every visit, where the same frame at 1920px in AVIF is
# 45,559, a 85% cut of the first thing a reader downloads. 1920px is the slot
# that fills the README's width=900 at 2x; the encode is the site recipe's,
# so the two captures are the same bytes and a re-capture cannot show the
# repository and the landing page different dashboards.
.PHONY: readme-assets
readme-assets: require-encoders ## rebuild the README's dashboard capture from docs/images/dashboard.png
	@mkdir -p $(DIST)
	magick docs/images/dashboard.png -strip -resize 1920x $(DIST)/readme-dashboard.png
	avifenc -q 32 -s 2 -y 444 --ignore-exif --ignore-xmp \
		$(DIST)/readme-dashboard.png $(DOCS_IMAGES)/dashboard.avif
	@{ $(ENCODER_VERSIONS); } | tr -d '\r' > $(ENCODER_RECORD)
	@bun test site/

# Every site-* target depends on this: .bun-version is what CI installs
# (bun-version-file), so a local run cannot check or deploy the site with
# a different runtime than the merge gate did.
.PHONY: require-bun
require-bun:
	@command -v bun >/dev/null 2>&1 || { \
		echo "make: bun is not on PATH (see .bun-version)" >&2; \
		exit 1; \
	}
	@have=$$(bun --version); \
	if [ "$$have" != "$(BUN_PIN)" ]; then \
		echo "make: bun $$have on PATH, .bun-version pins $(BUN_PIN)" >&2; \
		exit 1; \
	fi

.PHONY: site-check
site-check: require-bun ## bun test the Cloudflare Worker in site/ (CI parity)
	bun test site/

# biome.jsonc names the schema of the version BIOME_VERSION pins, so the pin
# appears twice in the tree. The guard is what keeps the two from disagreeing
# after a bump: an editor would check the old rules while the gate enforced the
# new ones, and nothing else reads the schema URL.
.PHONY: site-lint
site-lint: require-bun ## biome format-check and lint the files biome.jsonc includes, at the BIOME pin (CI parity)
	@grep -Fq '"$$schema": "https://biomejs.dev/schemas/$(BIOME_VERSION)/schema.json"' biome.jsonc || { \
		echo "make site-lint: the biome.jsonc schema URL does not name biome $(BIOME_VERSION); the pin and the schema must move together" >&2; \
		exit 1; \
	}
	bunx $(BIOME) check

.PHONY: site-fmt
site-fmt: require-bun ## rewrite the included files with the BIOME formatter, then re-lint
	bunx $(BIOME) check --write

# The site's only deployment step, and its undo. The /health poll proves the
# site is answering, not that this tree is the version answering, so it is an
# availability check: a deploy that uploaded cleanly but never took effect
# still passes here and is found by a visitor.
#
# site-deploy runs the two site gates first, so what reaches production is the
# tree the merge gate accepted; nothing else stands between a local edit and
# the live site. site-rollback deliberately does not: the point of a rollback
# is to work on a tree that does not pass.
#
# Both targets take $(SITE_LOCK) for the whole recipe, deploy and rollback
# alike: two of these running at once would leave whichever upload reached the
# platform last, and a rollback racing a deploy restores whichever version
# the platform happened to serve. mkdir is the portable lock (flock is not on
# macOS); it lives under dist/, so `make clean` releases a stale one.
#
# $(SITE_GUARD) opens the recipe: one shell takes the lock, arms the trap that
# releases it, and defines wait_for_site for the recipe to call once the
# platform call is done. Every recipe that calls wrangler must open with it
# and carry the rest of its work on the same recipe line: a second line is a
# second shell, so the trap would fire and free the lock before the deploy ran.
define SITE_GUARD
mkdir -p $(DIST); \
if ! mkdir $(SITE_LOCK) 2>/dev/null; then \
	echo "another site deploy or rollback holds $(SITE_LOCK); wait for it, or remove the directory if that process is gone" >&2; \
	exit 1; \
fi; \
trap 'rmdir $(SITE_LOCK) 2>/dev/null || true' EXIT; \
wait_for_site() { \
	for i in $$(seq 1 $(SITE_HEALTH_TRIES)); do \
		if [ "$$(curl -fsS --max-time 10 $(SITE_HEALTH_URL))" = "ok" ]; then \
			echo "$(SITE_HEALTH_URL) answered ok after $${i} attempt(s)"; \
			return 0; \
		fi; \
		echo "attempt $$i/$(SITE_HEALTH_TRIES): no ok from $(SITE_HEALTH_URL)"; \
		sleep $(SITE_HEALTH_WAIT); \
	done; \
	echo "$(SITE_HEALTH_URL) never answered ok" >&2; \
	return 1; \
};
endef

# CONTRIBUTING.md spells the wrangler version out in the login command an
# operator copies, and docs/THREAT_MODEL.md names it in the deployment paths it
# documents, so the pin lives in three files. This is that guard, the same
# shape as the biome schema check in site-lint: a bump of WRANGLER that does
# not move the documented command leaves an operator logging in with, and then
# deploying, a version the tree does not test against. Deploy-only, so a
# rollback keeps working on a tree whose docs have moved.
# ci.yml spells out go test / go vet / staticcheck instead of calling the
# targets above, so every tag those targets pass has to be written on the
# workflow line too. The zone tag is the one that bites: without it a test
# binary resolves time.Local against the host's zone files, which is the
# fallback the released binaries no longer have, so a test that passes under
# `make test` can fail in CI on a runner without them, and neither run is the
# artifact. Same shape as the biome schema and wrangler checks: a workflow
# line that has drifted fails here, locally, before a push.
#
# Comment lines and step names are skipped: a comment or a `name:` may
# mention a command it is not running. Every other line naming a go
# toolchain command must carry the tag, and every go vet line must carry
# VET_TESTS as well, for the reason given at the `vet` target.
WORKFLOWS := $(wildcard .github/workflows/*.yml)

# The same drift, inside this file. `test-pkg` builds its own `-tags` value
# rather than reusing GOTAGS, so a line here can lose the tag or the shuffle
# without any workflow changing, and nothing else in the tree notices: the
# go command is handed a *different* argument, not a missing one. With the
# tag value quoted against `$(race_flag)` with no space between them, the
# shell joins them, so RACE=0 (where race_flag expands to nothing) hands go
# a single build tag named "timetzdata-shuffle=on": the zone database drops
# out of the test binary and the shuffle flag is never seen at all, and go
# accepts the unknown tag without complaint. RACE=1 leaves race_flag as
# " -race ", so the space is there and that run stays correct. The divergence
# is therefore invisible on the default loop and lands on the RACE=0 loop,
# the one contributors are told to use for a faster cycle.
#
# Four rules, all read off the recipe source with index() rather than a
# regexp: the pattern text here is `$(race_flag)`, whose parentheses and
# dollar sign are regexp metacharacters, and a mangled pattern is a check
# that fires on every line or none. Every `$(GO) test` line must name the
# zone tag (directly or through GOTAGS); none may put `$(race_flag)`
# against a preceding non-space character; none may hand -tags an unquoted
# shell variable where the shell splits a two-tag list into a tag and a
# package pattern; and every one that runs tests rather than listing them
# must carry -shuffle=on.
#
# The guard skips any line containing awk, because its own recipe lines
# quote the very text being searched for and a check that matches itself
# reports every line it was meant to skip.
.PHONY: check-test-flags
check-test-flags: ## fail if a Makefile go test line lost the zone tag, glued a flag onto it, split -tags, or dropped -shuffle=on
	@untagged=$$(awk '/^[[:space:]]*#/ || /awk/ { next } index($$0, "$$(GO) test") && !index($$0, "$$(ZONE_TAG)") && !index($$0, "$$(GOTAGS") { print "  Makefile:" FNR ": " $$0 }' $(MAKEFILE_LIST)); \
	if [ -n "$$untagged" ]; then \
		echo "make check-test-flags: these lines run go test without -tags $(ZONE_TAG), so they test a binary without the embedded zone database, which is the fallback the released binaries no longer have:" >&2; \
		echo "$$untagged" >&2; \
		exit 1; \
	fi
	@glued=$$(awk '/^[[:space:]]*#/ || /awk/ { next } index($$0, "$$(GO) test") { p = index($$0, "$$(race_flag)"); if (p > 1 && substr($$0, p - 1, 1) != " ") print "  Makefile:" FNR ": " $$0 }' $(MAKEFILE_LIST)); \
	if [ -n "$$glued" ]; then \
		echo "make check-test-flags: these lines put \$$(race_flag) directly against the tag value, so RACE=0 joins them into one argument: the zone tag stops applying and -shuffle=on is dropped. Put a space between them:" >&2; \
		echo "$$glued" >&2; \
		exit 1; \
	fi
	@unshuffled=$$(awk '/^[[:space:]]*#/ || /awk/ { next } index($$0, "$$(GO) test") && !index($$0, "-list") && !index($$0, "-shuffle=on") { print "  Makefile:" FNR ": " $$0 }' $(MAKEFILE_LIST)); \
	if [ -n "$$unshuffled" ]; then \
		echo "make check-test-flags: these lines run tests without -shuffle=on, so an order-dependent test passes locally and fails under the shuffled order CI uses:" >&2; \
		echo "$$unshuffled" >&2; \
		exit 1; \
	fi
	@split=$$(awk '/^[[:space:]]*#/ || /awk/ { next } index($$0, "$$(GO) test") { p = index($$0, "-tags $$"); if (p > 0 && substr($$0, p + 7, 1) ~ /^[A-Za-z_]$$/) print "  Makefile:" FNR ": " $$0 }' $(MAKEFILE_LIST)); \
	if [ -n "$$split" ]; then \
		echo "make check-test-flags: these lines hand -tags an unquoted shell variable holding more than one tag, and the shell splits it: go takes the first word as the tag list and the rest as package patterns, the run fails with 'package <tag> is not in std', and the line it was meant to run is never reached:" >&2; \
		echo "$$split" >&2; \
		exit 1; \
	fi

.PHONY: check-ci-tags
check-ci-tags: ## fail if a workflow's go test/vet/staticcheck line does not carry the zone tag, or a go vet line lost -tests=true
	@missing=$$(awk '/^[[:space:]]*#/ || /^[[:space:]]*-?[[:space:]]*name:/ { next } /(^|[[:space:]])(go (test|vet|tool staticcheck)|staticcheck)[[:space:]]/ && $$0 !~ /$(ZONE_TAG)/ { print "  " FILENAME ":" FNR ": " $$0 }' $(WORKFLOWS)); \
	if [ -n "$$missing" ]; then \
		echo "make check-ci-tags: these workflow lines run go without -tags $(ZONE_TAG), so they analyze and test a different binary than 'make check' and 'make test':" >&2; \
		echo "$$missing" >&2; \
		exit 1; \
	fi
	@unvet=$$(awk '/^[[:space:]]*#/ || /^[[:space:]]*-?[[:space:]]*name:/ { next } /(^|[[:space:]])go vet[[:space:]]/ && $$0 !~ /$(VET_TESTS)[[:space:]]/ { print "  " FILENAME ":" FNR ": " $$0 }' $(WORKFLOWS)); \
	if [ -n "$$unvet" ]; then \
		echo "make check-ci-tags: these workflow lines run go vet without $(VET_TESTS), so they skip the test files that 'make vet' analyzes:" >&2; \
		echo "$$unvet" >&2; \
		exit 1; \
	fi

# The build job in ci.yml carries its own copy of PLATFORMS so each platform
# gets its own runner. A copy is a second place to forget: adding a platform
# here leaves the CI matrix vetting and compiling the old set while the release
# job builds and publishes the new one, and nothing fails. Same shape as
# check-ci-tags: a workflow that has drifted fails here, locally, first.
# The matrix entries only, so the `matrix.goos` uses in the step bodies and the
# uppercase GOOS/GOARCH in their env blocks do not read as platforms.
CI_WORKFLOW := .github/workflows/ci.yml
.PHONY: check-ci-platforms
check-ci-platforms: ## fail if the ci.yml build matrix does not match PLATFORMS
	@in_workflow=$$(sed -n 's/.*goos:[[:space:]]*\([a-z0-9]\{1,\}\)[^a-z0-9]*goarch:[[:space:]]*\([a-z0-9]\{1,\}\).*/\1\/\2/p' $(CI_WORKFLOW) | sort); \
	in_make=$$(printf '%s\n' $(PLATFORMS) | sort); \
	if [ "$$in_workflow" != "$$in_make" ]; then \
		echo "make check-ci-platforms: the build matrix in $(CI_WORKFLOW) and PLATFORMS here are different platform sets, so CI vets and compiles a different set than the release builds and publishes:" >&2; \
		printf '  only in %s:\n' "$(CI_WORKFLOW)" >&2; \
		comm -23 <(printf '%s\n' "$$in_workflow") <(printf '%s\n' "$$in_make") | sed 's/^/    /' >&2; \
		printf '  only in PLATFORMS:\n' >&2; \
		comm -13 <(printf '%s\n' "$$in_workflow") <(printf '%s\n' "$$in_make") | sed 's/^/    /' >&2; \
		exit 1; \
	fi

# Every merge gate in this repo is a step in a workflow, and a workflow is the
# one file here no analyzer reads: gofmt, staticcheck, vet, biome, ruff, mypy
# and black all look elsewhere. A YAML error there is worse than an unused
# import, because the step it breaks is a gate, and a gate that does not parse
# is a gate that does not run. The rule set and its three deviations are in
# .yamllint. Only .github/workflows: site/wrangler.jsonc is jsonc, and biome
# owns it.
#
# yamllint runs from the scripts env, the one black, ruff and mypy run from, so
# its pin is a line in scripts/requirements-dev.txt with a hash on the wheel
# like theirs. `uvx yamllint@1.38.0` pinned the linter and nothing else: uvx
# resolves the linter's own requirements out of the index every run, so pyyaml
# and pathspec arrived at whatever the registry served that minute, unhashed
# and unreviewed, on the one gate in the tree with no fixed closure. A gate
# whose linter can change under it is a gate nobody can reproduce a failure of.
.PHONY: check-yaml
check-yaml: ## fail if a workflow is invalid YAML or breaks the .yamllint rule set
	@$(MAKE) --no-print-directory scripts-env
	@$(SCRIPTS_BIN)/yamllint --config-file .yamllint $(WORKFLOWS)

# `make help` and the CONTRIBUTING.md target table both enumerate what a
# contributor runs, and a target that reaches them through one and not the
# other is the discovery gap: documented in the table but absent from the
# listing everybody reads first, so the only way to learn it exists is to
# read the table. One direction, table to help: every name the table
# documents must carry the '## ' comment the help recipe greps for. The
# reverse is not a gap, since help also lists the internal guards and the
# release-only targets a contributor never runs by hand.
.PHONY: check-help-docs
check-help-docs: ## fail if a target in CONTRIBUTING.md's table is missing from make help
	@documented=$$(awk -F'`' '/^\| `make / { for (i = 2; i < NF; i += 2) { t = $$i; if (t ~ /^make /) { sub(/^make /, "", t); split(t, w, /[ \/]/); print w[1] } } }' CONTRIBUTING.md | sort -u); \
	described=$$(grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{ print $$1 }' | sort -u); \
	missing=$$(comm -23 <(printf '%s\n' "$$documented") <(printf '%s\n' "$$described")); \
	if [ -n "$$missing" ]; then \
		echo "make check-help-docs: these targets are in CONTRIBUTING.md's target table but carry no '## ' description, so 'make help' does not list them:" >&2; \
		printf '  %s\n' $$missing >&2; \
		echo "  add the '## ...' comment to the target in the Makefile, the same one the help recipe reads" >&2; \
		exit 1; \
	fi

.PHONY: check-wrangler-doc
check-wrangler-doc: ## fail unless CONTRIBUTING.md's login command and docs/THREAT_MODEL.md's deploy path name the WRANGLER pin
	@grep -Fq 'wrangler@$(WRANGLER) login' CONTRIBUTING.md || { \
		echo "make: CONTRIBUTING.md does not name wrangler $(WRANGLER) in its login command; the pin and the documented command must move together" >&2; \
		exit 1; \
	}
	@grep -Fq 'wrangler@$(WRANGLER) deploy' docs/THREAT_MODEL.md || { \
		echo "make: docs/THREAT_MODEL.md does not name wrangler $(WRANGLER) in the deploy path it documents; the pin and the documented command must move together" >&2; \
		exit 1; \
	}

.PHONY: site-deploy
site-deploy: require-bun site-lint site-check check-wrangler-doc ## gate with site-lint/site-check, deploy the site Worker at the WRANGLER pin, then wait for /health
	@$(SITE_GUARD) \
	(cd site && bunx wrangler@$(WRANGLER) deploy) || exit 1; \
	rmdir $(SITE_ROLLED_BACK) 2>/dev/null || true; \
	mkdir -p $(SITE_DEPLOYED) || { echo "deployed, but cannot record $(SITE_DEPLOYED); the next 'make site-rollback' would find nothing to undo" >&2; exit 1; }; \
	wait_for_site || { echo "deploy finished but the site is not serving; roll back with 'make site-rollback'" >&2; exit 1; }

.PHONY: site-rollback
site-rollback: require-bun ## roll the site Worker back to the version before the last deploy, then wait for /health
	@$(SITE_GUARD) \
	if [ ! -d $(SITE_DEPLOYED) ]; then \
		echo "nothing to roll back: no deploy from this tree is waiting to be undone ($(SITE_DEPLOYED) is absent)"; \
		echo "'wrangler rollback' with no version undoes the most recent deployment whoever shipped it, so a second run here would roll back a rollback and put the version you just undid back on the site"; \
		exit 0; \
	fi; \
	(cd site && bunx wrangler@$(WRANGLER) rollback) || exit 1; \
	rmdir $(SITE_ROLLED_BACK) 2>/dev/null || true; \
	mv $(SITE_DEPLOYED) $(SITE_ROLLED_BACK) || { echo "rolled back, but cannot move $(SITE_DEPLOYED) aside; the next 'make site-rollback' would undo this one as well" >&2; exit 1; }; \
	wait_for_site || { echo "rollback finished but the site is not serving; retry, or read the deployment log in the Cloudflare dashboard" >&2; exit 1; }

.PHONY: fmt
fmt: ## rewrite all Go files with gofmt (including simplifications)
	$(GOFMT) -s -w .

.PHONY: fix
fix: ## apply go fix modernization autofixes, then gofmt
	$(GO) fix ./...
	GOOS=darwin $(GO) fix ./...
	GOOS=windows $(GO) fix ./...
	$(GOFMT) -s -w .

.PHONY: tidy
tidy: ## tidy go.mod and go.sum
	$(GO) mod tidy

.PHONY: tidy-check
tidy-check: ## fail if go.mod or go.sum would change
	$(GO) mod tidy -diff

.PHONY: require-uv
require-uv: ## fail unless uv is on PATH at or above UV_MIN
	@command -v uv >/dev/null 2>&1 || { \
		echo "make: uv is not on PATH (need >= $(UV_MIN); pins are scripts/requirements-dev.txt)" >&2; \
		exit 1; \
	}
# Definition and call in one shell: a second recipe line is a second shell,
# uv_too_old is undefined there, "command not found" is a false condition, and
# the version floor then passed anything.
	@$(UV_TOO_OLD); \
	have=$$(uv --version | awk '{print $$2}'); \
	if uv_too_old "$$have"; then \
		echo "make scripts-env: uv $$have on PATH, need >= $(UV_MIN) (CI installs $(UV_MIN))" >&2; \
		exit 1; \
	fi

# Rebuilds the env when either requirements file moves, or when the pinned
# interpreter moves, so an edited pin, a corrected hash or a Python bump is
# picked up without `make clean`. Missing .python-version from the
# prerequisites would leave a venv built on the old interpreter in place under
# a new pin, and every later `make scripts-check` or `make check-yaml` would
# answer from it.
.PHONY: scripts-env
scripts-env: $(SCRIPTS_BIN)/.stamp ## Python tool env under dist/, hashes verified

$(SCRIPTS_BIN)/.stamp: scripts/requirements-dev.txt scripts/requirements.txt .python-version
	@$(MAKE) --no-print-directory require-uv
	@mkdir -p $(DIST)
	@uv venv --quiet --clear --python $(PY_PIN) $(SCRIPTS_ENV)
	@VIRTUAL_ENV=$(SCRIPTS_ENV) uv pip install --quiet --no-deps -r scripts/requirements-dev.txt
	@touch $@

.PHONY: scripts-check
scripts-check: ## black, ruff and mypy over scripts/ (same pins as CI)
	@pin=$$(awk -F'"' '/^\[tool\.uv\]$$/ { u = 1; next } /^\[/ { u = 0 } u && /^required-version/ { print $$2 }' pyproject.toml | tr -d ' \t'); \
		if [ "$$pin" != ">=$(UV_MIN)" ]; then \
			echo "make scripts-check: pyproject.toml required-version '$$pin' disagrees with the uv line of .tool-versions '$(UV_MIN)'" >&2; \
			exit 1; \
		fi
	@$(MAKE) --no-print-directory scripts-env
	$(SCRIPTS_BIN)/black --check scripts/
	$(SCRIPTS_BIN)/ruff check scripts/
	# The type checker runs over the same target ruff does, from the same
	# [tool.mypy] files list, so a Python file ruff passes and mypy cannot
	# resolve is a failure here rather than a silent pass. --strict is
	# redundant with pyproject.toml's strict = true and is left off on
	# purpose: one place decides how strict this is.
	$(SCRIPTS_BIN)/mypy --no-incremental

.PHONY: screenshot
screenshot: ## render a tmux capture: make screenshot CAPTURE=.scratch/capture.txt OUT=docs/images/dashboard.png [SCALE COLS ROWS]
	@$(MAKE) --no-print-directory scripts-env
	$(SCRIPTS_BIN)/python scripts/screenshot.py $(CAPTURE) $(OUT) $(SCALE) $(COLS) $(ROWS)

.PHONY: check
check: ## verify go.mod, gofmt -s formatting, vet, staticcheck, the workflow YAML and the doc guards (CI parity)
	@$(MAKE) --no-print-directory check-test-flags
	@$(MAKE) --no-print-directory check-ci-tags
	@$(MAKE) --no-print-directory check-ci-platforms
	@$(MAKE) --no-print-directory check-yaml
	@$(MAKE) --no-print-directory check-help-docs
	@unformatted=$$($(GOFMT) -s -l .); \
		if [ -n "$$unformatted" ]; then \
			echo "needs gofmt (run 'make fmt'):" >&2; echo "$$unformatted" >&2; exit 1; \
		fi
	@$(MAKE) tidy-check
	@$(MAKE) lint
	@$(MAKE) vet

.PHONY: ci
ci: ## Go merge gates: tidy-diff, fmt, lint, vet, govulncheck, race tests
	@$(MAKE) check
	@$(MAKE) govulncheck
	@$(MAKE) test RACE=1

.PHONY: pr
pr: ## every PR merge gate except the OS matrix: ci + site-lint + site-check + check-wrangler-doc + scripts-check + repro-check-pair
	@$(MAKE) ci
	@$(MAKE) site-lint
	@$(MAKE) site-check
	@$(MAKE) check-wrangler-doc
	@$(MAKE) scripts-check
	@$(MAKE) repro-check-pair

.PHONY: clean
clean: ## remove build artifacts
	rm -rf $(DIST) $(BINARY) coverage.out *.test

.PHONY: check-changelog
check-changelog: ## verify CHANGELOG.md contains release section and link for VERSION
	@$(CHECK_VERSION)
	@$(CHECK_CHANGELOG)

# A release packages the working tree, so the two states that leave the bytes
# unreproducible have to be refused before they are packaged rather than
# recorded after: an uncommitted change, which buildinfo would name a commit
# for while the binaries carry something else, and no git at all, where
# SOURCE_DATE_EPOCH falls back to 0 and every archive member is dated to the
# epoch. A dev build is exempt: it is a local artifact, and its manifest
# records the tree honestly. ALLOW_DIRTY=1 is the named override for a release
# cut from a tree that cannot be committed first.
.PHONY: check-release-source
check-release-source: ## fail unless a non-dev VERSION builds from a clean, git-backed tree
	@if [ "$(VERSION)" = "dev" ]; then exit 0; fi; \
	if ! git rev-parse HEAD >/dev/null 2>&1; then \
		echo "make release: VERSION=$(VERSION) needs a git checkout; a source export has no commit for buildinfo to record and no commit time for SOURCE_DATE_EPOCH" >&2; \
		echo "  cut the release from the repository, or build VERSION=dev from the export" >&2; \
		exit 1; \
	fi; \
	if [ "$(ALLOW_DIRTY)" != "1" ]; then \
		dirty=$$(git status --porcelain); \
		if [ -n "$$dirty" ]; then \
			echo "make release: the working tree has uncommitted changes, so the binaries do not match the commit buildinfo records:" >&2; \
			printf '%s\n' "$$dirty" | sed 's/^/  /' >&2; \
			echo "  commit them, or pass ALLOW_DIRTY=1 to ship the tree as it stands" >&2; \
			exit 1; \
		fi; \
	fi; \
	case '$(SOURCE_DATE_EPOCH)' in \
		''|*[!0-9]*) echo "make release: SOURCE_DATE_EPOCH '$(SOURCE_DATE_EPOCH)' is not an epoch, so archive timestamps would follow the clock" >&2; exit 1;; \
	esac; \
	if [ "$(SOURCE_DATE_EPOCH)" = 0 ]; then \
		echo "make release: SOURCE_DATE_EPOCH is 0, so the checksums tarball dates every member to the epoch" >&2; \
		echo "  export SOURCE_DATE_EPOCH=\$$(git log -1 --pretty=%ct) before 'make release'" >&2; \
		exit 1; \
	fi

# The guards run before anything writes a byte: a version whose changelog or
# source tree fails must not spend a full cross-build first. sbom before
# checksums, because checksums.txt has to cover the SBOM or a downloaded SBOM
# is the one release asset with nothing to verify it against. Under -j none of
# that order is guaranteed by listing prerequisites, so .NOTPARALLEL below
# inserts a .WAIT between them.
.PHONY: release
release: check-changelog check-release-source sbom checksums ## build every release platform and SBOM into dist/ with reproducible checksums

# Without this, `make -j release` runs the three prerequisites above at once:
# dist-clean would delete the binaries test-dist is writing, and the checksums
# glob would miss an SBOM that has not been written yet.
.NOTPARALLEL: release

# dist/ is shared: cover writes coverage.out, vet-cross and repro-check write
# subdirectories, and the release build writes binaries. release.yml publishes
# every top-level file it finds there, so a coverage profile or an old note left
# by an earlier local target would ride along as a release asset. test-dist
# already drops this version's binaries; this drops the rest. It is a
# prerequisite of buildinfo rather than a sibling of it, so the sweep cannot
# run beside the build that fills dist/ again. The keep patterns name
# $(VERSION) rather than the bare prefix, so a `make release` of one version
# cannot checksum and publish another version's leftover, and both separators
# are kept so the SBOM (toktop-sbom-*) survives the sweep that runs after it.
# The tarball is a packaging output, not a release input: keeping it would fold
# yesterday's archive into today's checksums.txt, which the tar step is about to
# overwrite. Only regular files at depth 1 are touched, so the site deploy lock
# (a directory) and any nested build output are left alone.
.PHONY: dist-clean
dist-clean: ## drop files in dist/ that this $(VERSION) does not publish
	@mkdir -p $(DIST)
	@find $(DIST) -maxdepth 1 -type f ! -name '$(BINARY)_$(VERSION)_*' ! -name '$(BINARY)-sbom-$(VERSION)*' -delete
	@rm -f $(DIST)/$(BINARY)_$(VERSION)_checksums.tar.gz

# The SBOM is named with a `-` where the binaries use `_`, so it needs its own
# glob: the `$(BINARY)_*` list below would otherwise skip it. `make release`
# runs `sbom` first; run on its own, the glob matches nothing and the list is
# the binaries alone.
.PHONY: checksums
checksums: sbom buildinfo ## checksum the dist/ binaries into a byte-reproducible tarball
	@$(TAR) --sort=name --version >/dev/null 2>&1 || \
		{ echo "$(TAR) rejects --sort: deterministic packaging needs GNU tar (install it as gtar)" >&2; exit 1; }
	@cd $(DIST) && \
		set -- $(BINARY)_* && \
		if [ "$$1" = "$(BINARY)_*" ]; then \
			echo "make: no $(BINARY)_* binaries in $(DIST)" >&2; exit 1; \
		fi && \
		set -- $$(printf '%s\n' "$$@" $$(ls -1 $(BINARY)-sbom-* 2>/dev/null || true) | sort) && \
		if command -v sha256sum >/dev/null 2>&1; then \
			sha256sum "$$@" > checksums.txt && sha256sum -c checksums.txt; \
		else \
			shasum -a 256 "$$@" > checksums.txt && shasum -a 256 -c checksums.txt; \
		fi
	@cd $(DIST) && $(TAR) $(TAR_REPRO) -c checksums.txt | gzip -n -6 > toktop_$(VERSION)_checksums.tar.gz && rm checksums.txt

# Binaries of any earlier version are dropped first: leftovers would
# otherwise ride the toktop_* glob into checksums.txt and the release.
.PHONY: test-dist
test-dist: ## build every release platform without packaging
	@$(CHECK_VERSION)
	@mkdir -p $(DIST)
	@rm -f $(DIST)/$(BINARY)_*
	@for target in $(PLATFORMS); do \
		goos=$${target%/*}; goarch=$${target#*/}; ext=""; \
		if [ "$$goos" = "windows" ]; then ext=".exe"; fi; \
		name="$(BINARY)_$(VERSION)_$${goos}_$${goarch}$${ext}"; \
		echo "building $$name"; \
		CGO_ENABLED=0 GOOS=$$goos GOARCH=$$goarch \
			$(GO) build $(GOTAGS) $(GO_BUILDFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/$$name $(CMD) || exit 1; \
	done

# What produced the bytes, recorded next to them. A checksum list proves the
# download arrived intact, not which toolchain made it; without the commit,
# the toolchain, and the flags there is nothing faithful to rebuild against.
# Named to match the toktop_* glob, so it lands in checksums.txt too.
# dist-clean is a prerequisite, not a sibling: as a sibling it raced
# test-dist under `make -j release`, deleting binaries while they were being
# written and leaving checksums.txt covering fewer files than PLATFORMS.
.PHONY: buildinfo
buildinfo: dist-clean test-dist ## record the toolchain, commit, and flags behind dist/ into a manifest
	@mkdir -p $(DIST)
	@{ \
		echo "name: $(BINARY)"; \
		echo "version: $(VERSION)"; \
		echo "module: $$($(GO) list -m)"; \
		echo "commit: $(shell git rev-parse HEAD 2>/dev/null || echo unknown)"; \
		echo "dirty: $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo true || echo false)"; \
		echo "source_date_epoch: $(SOURCE_DATE_EPOCH)"; \
		echo "go: $$($(GO) env GOVERSION)"; \
		echo "gotoolchain: $(GOTOOLCHAIN)"; \
		echo "tags: $(TAGS)"; \
		echo "buildflags: $(GO_BUILDFLAGS)"; \
		echo "ldflags: $(LDFLAGS)"; \
		echo "cgo_enabled: $(CGO_ENABLED)"; \
		echo "goamd64: $(GOAMD64)"; \
		echo "goarm64: $(GOARM64)"; \
	} > $(DIST)/$(BINARY)_$(VERSION)_buildinfo.txt

# A published release is only a backup once something has fetched from it and
# the bytes came back whole. The publish step's exit status is not that
# evidence: fail_on_unmatched_files catches a glob that matched nothing, not an
# asset that never arrived, and a job killed after the release exists leaves
# the already-published guard refusing every retry. So this is the restore
# drill, and it runs the way a client does: read the published asset list,
# compare it against what PLATFORMS says a VERSION holds, pull every asset
# back down, and re-verify each digest against the release's own
# checksums.txt. Everything it downloads lands under $(DIST), which
# .gitignore covers and `make clean` takes.
#
# gh reads the release with GH_TOKEN from the environment, the same way
# release.yml's guard does, and `--jq` keeps a JSON parser off PATH: gh carries
# one. Both calls refuse to read an empty answer as success, because a list
# that failed to parse is the same shape as a release with nothing on it.
.PHONY: release-verify
release-verify: ## fetch every published asset for VERSION and re-verify its checksum (needs gh)
	@$(CHECK_VERSION)
	@command -v gh >/dev/null 2>&1 || { \
		echo "make release-verify: gh is not on PATH; it is what reads the published asset list" >&2; \
		exit 1; \
	}
	@tag=v$(VERSION); \
	dir=$(CURDIR)/$(DIST)/release-verify/$(VERSION); \
	rm -rf "$$dir" && mkdir -p "$$dir/sums"; \
	if ! assets=$$(gh release view "$$tag" --repo $(RELEASE_REPO) --json assets --jq '.assets[] | "\(.size) \(.name)"' 2>&1); then \
		echo "make release-verify: cannot read release $$tag on $(RELEASE_REPO):" >&2; printf '%s\n' "$$assets" >&2; \
		echo "  an unpublished version has nothing to restore; this runs after the tag push" >&2; exit 1; \
	fi; \
	if [ -z "$$assets" ]; then \
		echo "make release-verify: $$tag publishes no assets on $(RELEASE_REPO), or the list could not be read" >&2; exit 1; \
	fi; \
	printf '%s\n' "$$assets" | sort -k2 > "$$dir/published.txt"; \
	{ for target in $(PLATFORMS); do \
		goos=$${target%/*}; goarch=$${target##*/}; ext=""; \
		if [ "$$goos" = "windows" ]; then ext=".exe"; fi; \
		printf '%s_%s_%s_%s%s\n' "$(BINARY)" "$(VERSION)" "$$goos" "$$goarch" "$$ext"; \
	done; \
	printf '%s-sbom-%s.cdx.json\n' "$(BINARY)" "$(VERSION)"; \
	printf '%s_%s_buildinfo.txt\n' "$(BINARY)" "$(VERSION)"; \
	printf '%s_%s_checksums.tar.gz\n' "$(BINARY)" "$(VERSION)"; } | sort > "$$dir/expected.txt"; \
	awk '{ print $$2 }' "$$dir/published.txt" > "$$dir/names.txt"; \
	missing=$$(grep -vxF -f "$$dir/names.txt" "$$dir/expected.txt" || true); \
	extra=$$(grep -vxF -f "$$dir/expected.txt" "$$dir/names.txt" || true); \
	if [ -n "$$missing" ]; then \
		echo "make release-verify: $$tag does not publish:" >&2; sed 's/^/  MISSING     /' <<< "$$missing" >&2; \
		echo "  a published release is not re-runnable (release.yml refuses an already-published tag)," >&2; \
		echo "  so a missing asset is repaired by uploading it from a local 'make release VERSION=$(VERSION)'," >&2; \
		echo "  which builds the same bytes, or by cutting a new version." >&2; exit 1; \
	fi; \
	if [ -n "$$extra" ]; then \
		echo "make release-verify: $$tag publishes assets a VERSION=$(VERSION) release does not produce:" >&2; \
		sed 's/^/  UNEXPECTED  /' <<< "$$extra" >&2; exit 1; \
	fi; \
	awk '{ if ($$1 <= 0) { printf "  EMPTY      %s\n", $$2; exit 1 } }' "$$dir/published.txt" >&2 || exit 1; \
	gh release download "$$tag" --repo $(RELEASE_REPO) --dir "$$dir" --pattern '$(BINARY)*$(VERSION)*' || exit 1; \
	tar -xzf "$$dir/$(BINARY)_$(VERSION)_checksums.tar.gz" -C "$$dir/sums" || exit 1; \
	listed=$$(cut -c67- "$$dir/sums/checksums.txt" 2>/dev/null | tr -d '*' | sort || true); \
	unlisted=$$(grep -vxF -e "$$listed" -e "$(BINARY)_$(VERSION)_checksums.tar.gz" "$$dir/expected.txt" || true); \
	if [ -z "$$listed" ] || [ -n "$$unlisted" ]; then \
		echo "make release-verify: $$tag's checksums.txt does not cover every artifact it should:" >&2; \
		if [ -n "$$unlisted" ]; then sed 's/^/  UNLISTED   /' <<< "$$unlisted" >&2; fi; \
		echo "  an artifact missing from checksums.txt is one a client cannot verify, so it is not installed." >&2; exit 1; \
	fi; \
	cd "$$dir" && \
		if command -v sha256sum >/dev/null 2>&1; then sha256sum -c "$$dir/sums/checksums.txt"; \
		else shasum -a 256 -c "$$dir/sums/checksums.txt"; fi

# The flags above promise byte-identical output; nothing tested that promise.
# Build each platform twice and diff. The one input still free to leak is the
# directory the compiler reads the source from, so pass b builds a copy of the
# tree at a different absolute path; a surviving source path shows up as a
# diff, not as a coincidence of one machine. Each pass also gets its own
# GOCACHE, so neither can be answered out of the other's build cache. LC_ALL,
# TZ, GOTOOLCHAIN, GOAMD64 and GOARM64 are already pinned globally above, so
# re-setting them inside a pass would vary nothing. diffoscope explains a
# failure when it is installed; the diff itself is the verdict either way.
#
# The copy is the working tree as it stands, not HEAD, so a dirty checkout is
# compared with itself. $(DIST) is excluded so tar does not walk into the tree
# it is writing; .scratch and .gauntlet are gitignored developer state that no
# build step reads.
.PHONY: repro-check
repro-check: ## build every release platform twice, from two different source paths, then diff
	@$(CHECK_VERSION)
	@rm -rf $(DIST)/repro
	@mkdir -p $(DIST)/repro/src-b
	@$(TAR) --exclude=./$(DIST) --exclude=./.git --exclude=./.scratch --exclude=./.gauntlet -cf - . | \
		$(TAR) -C $(DIST)/repro/src-b -xf -
	@test -f $(DIST)/repro/src-b/go.mod || { \
		echo "make: repro-check could not stage a source copy under $(DIST)/repro/src-b" >&2; \
		exit 1; \
	}
	@for target in $(PLATFORMS); do \
		goos=$${target%/*}; goarch=$${target#*/}; ext=""; \
		if [ "$$goos" = "windows" ]; then ext=".exe"; fi; \
		name="$(BINARY)_$(VERSION)_$${goos}_$${goarch}$${ext}"; \
		for pass in a b; do \
			mkdir -p $(DIST)/repro/$$pass/$${goos}_$${goarch} $(DIST)/repro/cache-$$pass; \
			if [ "$$pass" = a ]; then src=$(CURDIR); else src=$(CURDIR)/$(DIST)/repro/src-b; fi; \
			( cd "$$src" && \
				env GOCACHE=$(CURDIR)/$(DIST)/repro/cache-$$pass \
				GOOS=$$goos GOARCH=$$goarch CGO_ENABLED=0 \
				$(GO) build $(GOTAGS) $(GO_BUILDFLAGS) -ldflags "$(LDFLAGS)" \
				-o $(CURDIR)/$(DIST)/repro/$$pass/$${goos}_$${goarch}/$$name $(CMD) ) || exit 1; \
		done; \
		a=$(DIST)/repro/a/$${goos}_$${goarch}/$$name; \
		b=$(DIST)/repro/b/$${goos}_$${goarch}/$$name; \
		if cmp -s "$$a" "$$b"; then \
			echo "reproducible: $$name"; \
		else \
			echo "NOT reproducible: $$name" >&2; \
			if command -v diffoscope >/dev/null 2>&1; then \
				diffoscope "$$a" "$$b" >&2 || true; \
			else \
				echo "  install diffoscope to see what differs" >&2; \
			fi; \
			exit 1; \
		fi; \
	done
	@rm -rf $(DIST)/repro

# The same gate over REPRO_PLATFORMS, so a contributor can reproduce either
# the merge-gate repro job or the release job locally without paying for all
# of PLATFORMS. `make repro-check` is the full sweep, local only.
.PHONY: repro-check-pair
repro-check-pair: ## repro-check over REPRO_PLATFORMS (what the PR gate runs)
	@$(MAKE) repro-check PLATFORMS="$(REPRO_PLATFORMS)"

# XDG user bin on Linux; override on macOS so the binary lands on PATH
# (PREFIX=/usr/local or PREFIX=$(brew --prefix)).
PREFIX ?= $(HOME)/.local

.PHONY: install
install: build ## install into PREFIX/bin (default ~/.local/bin)
	mkdir -p "$(PREFIX)/bin"
	install -m 0755 $(BINARY) "$(PREFIX)/bin/$(BINARY)"
	@case ":$$PATH:" in \
		*":$(PREFIX)/bin:"*) ;; \
		*) echo "make install: installed to $(PREFIX)/bin, which is not on PATH; add it before '$(BINARY)' resolves" >&2; \
		   echo "  export PATH=\"$(PREFIX)/bin:$$PATH\"" >&2 ;; \
	esac
