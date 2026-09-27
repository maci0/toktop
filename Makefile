BINARY  := toktop
CMD     := ./cmd/toktop
DIST    := dist
VERSION ?= dev
# VERSION is interpolated into -ldflags and dist filenames. Refuse values
# that would break the shell, the linker flag, or the artifact name.
CHECK_VERSION = printf '%s' '$(VERSION)' | grep -qE '^[A-Za-z0-9._+-]+$$' || { echo "make: VERSION must match [A-Za-z0-9._+-]+ (got '$(VERSION)')" >&2; exit 1; }
CHECK_CHANGELOG = if [ '$(VERSION)' != 'dev' ]; then \
	awk -v v='$(VERSION)' 'index($$0, "\#\# [" v "] ") == 1 || $$0 == "\#\# [" v "]" {f=1} END{exit !f}' CHANGELOG.md || { echo "make: CHANGELOG.md missing '\#\# [$(VERSION)]' section" >&2; exit 1; }; \
	awk -v v='$(VERSION)' 'index($$0, "[" v "]: ") == 1 {f=1} END{exit !f}' CHANGELOG.md || { echo "make: CHANGELOG.md missing '[$(VERSION)]:' link reference" >&2; exit 1; }; \
	awk -v v='$(VERSION)' 'index($$0, "[" v "]: ") == 1 && $$0 !~ "compare/.*v" v "$$" {f=1} END{exit f}' CHANGELOG.md || { echo "make: CHANGELOG.md '[$(VERSION)]:' link does not end at tag v$(VERSION)" >&2; exit 1; }; \
	awk '/^\#\# \[Unreleased\]/{f=1;next} /^\#\# \[/{f=0} f && /^- /{n++} END{exit (n>0)}' CHANGELOG.md || { echo "make: CHANGELOG.md still has entries under [Unreleased]; move them under [$(VERSION)] first" >&2; exit 1; }; \
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
# go.sum, produce a PIE. Empty -buildid= so the GNU build-id note is not a
# second, toolchain-hash-shaped input to the bytes.
GO_BUILDFLAGS := -trimpath -buildvcs=false -mod=readonly -buildmode=pie
STATICCHECK := $(GO) tool staticcheck
# go run @version, not a go.mod tool: govulncheck's module graph is newer than
# staticcheck's (x/tools, x/mod) and would force those up if it joined the
# tool block. One string so Makefile and CI cannot drift.
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.7.0
SBOM_TOOL   := github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.12.0
# bunx, not a package.json: the Worker ships with no npm dependencies, and a
# manifest plus lockfile would exist only to pin this one linter.
BIOME       := @biomejs/biome@2.5.14
# Cloudflare deploy tool for site/. Deploying with whatever `wrangler` a
# machine happens to have installed (or a bare `cf deploy`) makes the upload
# depend on PATH, so the pin is named here and every deploy path reads it.
WRANGLER    := 4.126.0
# The Worker answers /health with `ok`; site-deploy and site-rollback poll it
# until the expected version is serving or give up.
SITE_HEALTH_URL   := https://toktop.ai/health
SITE_HEALTH_TRIES := 6
SITE_HEALTH_WAIT  := 10
# Serializes site-deploy against site-deploy and site-rollback. Under dist/,
# which .gitignore already covers, so a released lock is never committed.
SITE_LOCK         := $(DIST)/site.lock
LDFLAGS     := -s -w -buildid= -X main.version=$(VERSION)
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
GOTAGS  := $(if $(TAGS),-tags $(TAGS),)
# Race detector is on by default so the loop matches `make test` / CI.
# RACE=0 skips it (and the C compiler) for a faster edit cycle.
RACE    ?= 1
race_flag = $(if $(filter 0,$(RACE)),,-race )
# Lower bound for `make scripts-check`. CI installs this exact version
# (.github/workflows/ci.yml setup-uv); a newer uv on PATH is fine.
UV_MIN := 0.12.6

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

.DEFAULT_GOAL := help

.PHONY: help
help: ## show available targets
	@if [ -t 1 ] && [ -z "$${NO_COLOR:-}" ] && [ "$${TERM:-}" != "dumb" ]; then \
		color=1; \
	else \
		color=; \
	fi; \
	grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | \
		awk -v color="$$color" 'BEGIN {FS = ":.*## "} { if (color) printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2; else printf "  %-14s %s\n", $$1, $$2 }'

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
	CGO_ENABLED=$(if $(filter 0,$(RACE)),0,1) $(GO) test -mod=readonly $(race_flag)-shuffle=on ./...
	CGO_ENABLED=$(if $(filter 0,$(RACE)),0,1) $(GO) test -mod=readonly -tags sqlite $(race_flag)-shuffle=on ./agentusage/...

# Same flags and toolchain as `make test`. PKG is required; RUN (or TEST) and
# TESTTAGS are optional. Unset TESTTAGS on ./agentusage runs both halves of the
# sqlite tag gate (matching `make test`); TESTTAGS=sqlite (or another tag) runs one.
# RACE=0 drops -race for a faster edit loop; default matches CI.
.PHONY: test-pkg
test-pkg: ## one package/test: PKG=./internal/ui [RUN=TestName] [TESTTAGS=sqlite] [RACE=0]
	@if [ -z "$(PKG)" ]; then \
		echo "make test-pkg: set PKG (e.g. PKG=./internal/ui)" >&2; \
		echo "  optional: RUN=TestName (or TEST=TestName)  TESTTAGS=sqlite  RACE=0" >&2; \
		exit 1; \
	fi
	@if [ "$(RACE)" != "0" ]; then $(NEED_CC); fi
	CGO_ENABLED=$(if $(filter 0,$(RACE)),0,1) $(GO) test -mod=readonly $(if $(TESTTAGS),-tags $(TESTTAGS) )$(race_flag)-shuffle=on $(if $(or $(RUN),$(TEST)),-run "$(or $(RUN),$(TEST))" )"$(PKG)"
	@if [ -z "$(TESTTAGS)" ]; then \
		case "$(PKG)" in \
		./agentusage|./agentusage/|./agentusage/...|agentusage|github.com/maci0/toktop/agentusage|github.com/maci0/toktop/agentusage/|github.com/maci0/toktop/agentusage/...) \
			echo "make test-pkg: also running -tags sqlite (set TESTTAGS to run one half)"; \
			CGO_ENABLED=$(if $(filter 0,$(RACE)),0,1) $(GO) test -mod=readonly -tags sqlite $(race_flag)-shuffle=on $(if $(or $(RUN),$(TEST)),-run "$(or $(RUN),$(TEST))" )"$(PKG)" || exit 1; \
			;; \
		esac; \
	fi

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

.PHONY: vet
vet: ## run go vet (both halves of the sqlite tag gate)
	$(GO) vet -mod=readonly ./...
	$(GO) vet -mod=readonly -tags sqlite ./agentusage/...

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
		env GOOS=$$goos GOARCH=$$goarch $(GO) vet -mod=readonly ./... || exit 1; \
		env GOOS=$$goos GOARCH=$$goarch $(GO) vet -mod=readonly -tags sqlite ./agentusage/... || exit 1; \
		env GOOS=$$goos GOARCH=$$goarch $(DIST)/bin/staticcheck ./... || exit 1; \
		env GOOS=$$goos GOARCH=$$goarch $(DIST)/bin/staticcheck -tags sqlite ./agentusage/... || exit 1; \
	done
	@rm -rf $(DIST)/bin

.PHONY: lint
lint: ## run staticcheck (both halves of the sqlite tag gate)
	$(STATICCHECK) ./...
	$(STATICCHECK) -tags sqlite ./agentusage/...

.PHONY: govulncheck
govulncheck: ## run govulncheck at the GOVULNCHECK pin (same pin as CI)
	$(GO) run $(GOVULNCHECK) ./...
	$(GO) run $(GOVULNCHECK) -tags sqlite ./...

# Every site-* target depends on this: .bun-version is what CI installs
# (bun-version-file), so a local run cannot check or deploy the site with
# a different runtime than the merge gate did.
.PHONY: require-bun
require-bun:
	@command -v bun >/dev/null 2>&1 || { \
		echo "make: bun is not on PATH (see .bun-version)" >&2; \
		exit 1; \
	}
	@want=$$(tr -d ' \t\r\n' < .bun-version); have=$$(bun --version); \
	if [ "$$have" != "$$want" ]; then \
		echo "make: bun $$have on PATH, .bun-version pins $$want" >&2; \
		exit 1; \
	fi

.PHONY: site-check
site-check: require-bun ## bun test the Cloudflare Worker in site/ (CI parity)
	bun test site/

.PHONY: site-lint
site-lint: require-bun ## biome-lint site/ at the BIOME pin (CI parity)
	bunx $(BIOME) lint site/

# The site's only deployment step, and its undo. The /health poll proves the
# site is answering, not that this tree is the version answering, so it is an
# availability check: a deploy that uploaded cleanly but never took effect
# still passes here and is found by a visitor.
#
# Both targets take $(SITE_LOCK) for the whole recipe, deploy and rollback
# alike: two of these running at once would leave whichever upload reached the
# platform last, and a rollback racing a deploy restores whichever version
# the platform happened to serve. mkdir is the portable lock (flock is not on
# macOS); it lives under dist/, so `make clean` releases a stale one.
#
# $(SITE_GUARD) opens the recipe: one shell takes the lock, arms the trap that
# releases it, and defines wait_for_site for the recipe to call once the
# platform call is done. Every site-* recipe must open with it and carry the
# rest of its work on the same recipe line: a second line is a second shell,
# so the trap would fire and free the lock before the deploy ran.
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

.PHONY: site-deploy
site-deploy: require-bun ## deploy the site Worker at the WRANGLER pin, then wait for /health
	@$(SITE_GUARD) \
	(cd site && bunx wrangler@$(WRANGLER) deploy) || exit 1; \
	wait_for_site || { echo "deploy finished but the site is not serving; roll back with 'make site-rollback'" >&2; exit 1; }

.PHONY: site-rollback
site-rollback: require-bun ## roll the site Worker back to the version before the last deploy, then wait for /health
	@$(SITE_GUARD) \
	(cd site && bunx wrangler@$(WRANGLER) rollback) || exit 1; \
	wait_for_site || { echo "rollback finished but the site is not serving; retry, or read the deployment log in the Cloudflare dashboard" >&2; exit 1; }

.PHONY: fmt
fmt: ## rewrite all Go files with gofmt (including simplifications)
	$(GOFMT) -s -w .

.PHONY: format
format: fmt ## alias for fmt

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

.PHONY: scripts-check
scripts-check: ## black and ruff over scripts/ (same pins as CI)
	@command -v uv >/dev/null 2>&1 || { \
		echo "make scripts-check: uv is not on PATH (need >= $(UV_MIN); pins are scripts/requirements-dev.txt)" >&2; \
		exit 1; \
	}
	@have=$$(uv --version | awk '{print $$2}'); \
		if [ "$$(printf '%s\n%s\n' "$(UV_MIN)" "$$have" | sort -V | head -1)" != "$(UV_MIN)" ]; then \
			echo "make scripts-check: uv $$have on PATH, need >= $(UV_MIN) (CI installs $(UV_MIN))" >&2; \
			exit 1; \
		fi
	uv run --isolated --no-project --with-requirements scripts/requirements-dev.txt black --check scripts/
	uv run --isolated --no-project --with-requirements scripts/requirements-dev.txt ruff check scripts/

.PHONY: check
check: ## verify go.mod, gofmt -s formatting, vet and staticcheck (CI parity)
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
pr: ## every PR merge gate except the OS matrix: ci + site-lint + site-check + scripts-check + repro-check-pair
	@$(MAKE) ci
	@$(MAKE) site-lint
	@$(MAKE) site-check
	@$(MAKE) scripts-check
	@$(MAKE) repro-check-pair

.PHONY: clean
clean: ## remove build artifacts
	rm -rf $(DIST) $(BINARY) coverage.out *.test

.PHONY: check-changelog
check-changelog: ## verify CHANGELOG.md contains release section and link for VERSION
	@$(CHECK_VERSION)
	@$(CHECK_CHANGELOG)

.PHONY: release
release: check-changelog checksums sbom ## build every release platform and SBOM into dist/ with reproducible checksums

# dist/ is shared: cover writes coverage.out, vet-cross and repro-check write
# subdirectories, and the release build writes binaries. release.yml publishes
# every top-level file it finds there, so a coverage profile or an old note left
# by an earlier local target would ride along as a release asset. test-dist
# already drops $(BINARY)_*; this drops the rest. Both separators are kept so
# the SBOM (toktop-sbom-*) survives whichever order a `make -j release` runs
# the prerequisites in. Only regular files at depth 1 are touched, so the site
# deploy lock (a directory) and any nested build output are left alone.
.PHONY: dist-clean
dist-clean: ## drop non-release files left in dist/ by earlier targets
	@mkdir -p $(DIST)
	@find $(DIST) -maxdepth 1 -type f ! -name '$(BINARY)[_-]*' -delete

.PHONY: checksums
checksums: dist-clean buildinfo ## checksum the dist/ binaries into a byte-reproducible tarball
	@$(TAR) --sort=name --version >/dev/null 2>&1 || \
		{ echo "$(TAR) rejects --sort: deterministic packaging needs GNU tar (install it as gtar)" >&2; exit 1; }
	@cd $(DIST) && \
		set -- $(BINARY)_* && \
		if [ "$$1" = "$(BINARY)_*" ]; then \
			echo "make: no $(BINARY)_* binaries in $(DIST)" >&2; exit 1; \
		fi && \
		set -- $$(printf '%s\n' "$$@" | sort) && \
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
.PHONY: buildinfo
buildinfo: test-dist ## record the toolchain, commit, and flags behind dist/ into a manifest
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

# The flags above promise byte-identical output; nothing tested that promise.
# Build each platform twice with everything the Makefile neutralizes varied
# between the passes: output path, build cache, locale, and timezone. A
# timestamp leak or a surviving absolute path shows up as a diff, not as a
# coincidence of one machine. diffoscope explains a failure when it is
# installed; the diff itself is the verdict either way.
.PHONY: repro-check
repro-check: ## build every release platform twice under different path, cache, locale, and TZ, then diff
	@$(CHECK_VERSION)
	@rm -rf $(DIST)/repro
	@for target in $(PLATFORMS); do \
		goos=$${target%/*}; goarch=$${target#*/}; ext=""; \
		if [ "$$goos" = "windows" ]; then ext=".exe"; fi; \
		name="$(BINARY)_$(VERSION)_$${goos}_$${goarch}$${ext}"; \
		for pass in a b; do \
			mkdir -p $(DIST)/repro/$$pass/$${goos}_$${goarch} $(DIST)/repro/cache-$$pass; \
			env LC_ALL=C TZ=UTC GOCACHE=$(CURDIR)/$(DIST)/repro/cache-$$pass \
				GOOS=$$goos GOARCH=$$goarch CGO_ENABLED=0 \
				$(GO) build $(GOTAGS) $(GO_BUILDFLAGS) -ldflags "$(LDFLAGS)" \
				-o $(DIST)/repro/$$pass/$${goos}_$${goarch}/$$name $(CMD) || exit 1; \
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

# The same gate over REPRO_PLATFORMS, so a contributor can reproduce the
# merge-gate repro job locally instead of only the full pre-release sweep.
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
