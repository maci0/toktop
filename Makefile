BINARY  := toktop
CMD     := ./cmd/toktop
DIST    := dist
VERSION ?= dev
# VERSION is interpolated into -ldflags and dist filenames. Refuse values
# that would break the shell, the linker flag, or the artifact name.
CHECK_VERSION = printf '%s' '$(VERSION)' | grep -qE '^[A-Za-z0-9._+-]+$$' || { echo "make: VERSION must match [A-Za-z0-9._+-]+ (got '$(VERSION)')" >&2; exit 1; }
# The files a release publishes that are not per-platform binaries, named once.
# Every recipe that writes one spells the name from here, and release-verify
# reads the same five names when it compares the published asset list against
# what a VERSION produces: a name spelled in both places is a release that
# uploads a file the restore drill then reports as unexpected.
SBOM_ASSET      = $(BINARY)-sbom-$(VERSION).cdx.json
BUILDINFO_ASSET = $(BINARY)_$(VERSION)_buildinfo.txt
LICENSES_ASSET  = $(BINARY)_$(VERSION)_licenses.txt
LICENSE_ASSET   = $(BINARY)_$(VERSION)_LICENSE.txt
CHECKSUMS_ASSET = $(BINARY)_$(VERSION)_checksums.tar.gz
RELEASE_ASSETS  = $(SBOM_ASSET) $(BUILDINFO_ASSET) $(LICENSES_ASSET) $(LICENSE_ASSET) $(CHECKSUMS_ASSET)
# A cut leaves an empty '## [Unreleased]' stub, and the stub is a heading
# without a version. Only a versioned heading closes the section and names the
# version the bump is compared against, so a stub anywhere below the new
# section cannot pass for the release that preceded it.
#
# The bump rule that closes the assignment reads the version being cut with
# any prerelease or build suffix stripped: 0.22.1-rc.1 breaks a caller on
# 0.22.0 the way 0.22.1 does, and a release the rule skipped on the strength
# of a suffix it never looked past is the one cut that ships the break
# unannounced.
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
	awk -v v='$(VERSION)' 'BEGIN{ n=v; sub(/[-+].*$$/, "", n); if (n !~ /^[0-9]+\.[0-9]+\.[0-9]+$$/) exit 0 } \
		index($$0, "\#\# [" v "]") == 1 {f=1; next} \
		/^\#\# \[/ {if (f==1 && $$0 != "\#\# [Unreleased]") {f=0; if (match($$0, /\#\# \[[0-9]+\.[0-9]+\.[0-9]+\]/)) prev=substr($$0, RSTART+4, RLENGTH-5)} next} \
		f==1 && $$0 == "\#\#\# Breaking" {breaking=1} \
		END { \
			if (!breaking || prev == "") exit 0; \
			split(n, a, "."); split(prev, b, "."); \
			exit (a[1] == b[1] && a[2] == b[2]) \
		}' CHANGELOG.md || { echo "make: CHANGELOG.md section for $(VERSION) carries a 'Breaking' entry but $(VERSION) is a patch bump; the project is 0.x, so a breaking change rides a minor bump" >&2; exit 1; }; \
fi

# The module's public packages: the importable ones a Go program can name.
# `cmd` and `internal` are not on that list, so a change to either cannot move
# this contract.
PUBLIC_PKGS = ./agentusage

# The surfaces whose content a reader or a caller reads, and so a change to
# any of them is a change someone has to be told about. The rest of docs/ is
# deliberately absent: ARCHITECTURE.md is a map of the tree and RECOVERY.md a
# set of operator procedures, both correctable without a change anyone
# upgrading can observe, and a gate that asked for an entry for every one of
# them would train the entry to be boilerplate. internal/ui/json.go is here
# for the same reason help.go is: it holds the keys of the --json report, and
# a key renamed or a field dropped is a report a script decodes into a zero
# with no error to notice it by.
CHANGELOG_WATCHED = README.md cmd/toktop/help.go docs/openapi.yaml agentusage internal/ui/json.go site/worker.js site/README.md

# The declarations each public package exports, taken from `go doc` with the
# prose dropped by scripts/api-surface.awk. A removed or reshaped declaration
# is a breaking change a Go caller meets as a compile error in their tree, and
# nothing here can see it: this tree still builds, the tests still pass, and the
# CI platforms still cross-compile. The rule matches the changelog gate's, since
# both answer the same question: the project is 0.x, so a breaking change rides
# a minor bump and a patch is refused. A minor bump is let through, but only
# against a 'Breaking' heading under the version being cut: a removal the
# changelog does not name reaches the caller who upgrades as a compile error
# they were never told to expect. A checkout with no released tag before
# HEAD^ has no base to diff and is let past.
#
# A repository's first commit has no HEAD^ either, and there is nothing to
# compare it against, so it is let past the same way. A shallow checkout is
# not that: it has a parent in the real history and the clone simply does not
# carry it, and the gate cannot tell the two apart by looking at HEAD^ alone.
# It asks whether the clone is shallow, and a shallow one is refused with a
# message naming the fix rather than passing. The release runner checks out at
# fetch-depth: 1 by default, which is how an exported removal once reached a
# published release with every gate reporting green.
CHECK_API = if [ '$(VERSION)' = 'dev' ]; then exit 0; fi; \
	if ! git rev-parse HEAD >/dev/null 2>&1; then \
		echo "make: check-api needs a git checkout; the exported surface is read from the tree" >&2; \
		exit 1; \
	fi; \
	if [ "$$(git rev-parse --is-shallow-repository 2>/dev/null)" = "true" ]; then \
		echo "make: check-api compares the exported surface against the last release, read from the commit before HEAD" >&2; \
		echo "  this checkout is shallow and does not carry that history, so the comparison would find nothing and pass." >&2; \
		echo "  actions/checkout defaults to fetch-depth: 1; the release workflow sets fetch-depth: 0 for this reason." >&2; \
		echo "  unshallow the clone (git fetch --unshallow), or pass ALLOW_SHALLOW=1 to accept that this cut is unchecked." >&2; \
		if [ "$(ALLOW_SHALLOW)" != "1" ]; then exit 1; fi; \
	fi; \
	if ! git rev-parse --verify --quiet 'HEAD^{commit}' >/dev/null 2>&1; then \
		echo "make: check-api needs a git checkout; the exported surface is read from the tree" >&2; \
		exit 1; \
	fi; \
	base=$$(git describe --tags --abbrev=0 HEAD^ 2>/dev/null) || exit 0; \
	if [ -z "$$base" ]; then echo "make: check-api found no released tag before HEAD^; nothing to compare the surface to" >&2; exit 0; fi; \
	work=$$(mktemp -d); trap 'rm -rf "$$work"' EXIT; \
	git archive "$$base" | $(TAR) -xf - -C "$$work" || exit 1; \
	awk=$$(pwd)/scripts/api-surface.awk; \
	[ -f "$$awk" ] || awk="$$work/scripts/api-surface.awk"; \
	fail=0; \
	for pkg in $(PUBLIC_PKGS); do \
		( cd "$$work" && $(GO) doc -all "$$pkg" ) | awk -f "$$awk" | sort -u > "$$work/base.txt" || exit 1; \
		$(GO) doc -all "$$pkg" | awk -f "$$awk" | sort -u > "$$work/head.txt" || exit 1; \
		gone=$$(comm -23 "$$work/base.txt" "$$work/head.txt"); \
		if [ -n "$$gone" ]; then \
			prev=$$(printf '%s' "$$base" | sed 's/^v//'); \
			major=$$(printf '%s' "$(VERSION)" | cut -d. -f1); \
			minor=$$(printf '%s' "$(VERSION)" | cut -d. -f2); \
			pmajor=$$(printf '%s' "$$prev" | cut -d. -f1); \
			pminor=$$(printf '%s' "$$prev" | cut -d. -f2); \
			if [ "$$major" = "$$pmajor" ] && [ "$$minor" = "$$pminor" ]; then \
				echo "make: $(VERSION) removes from $$pkg, what $$base exported:" >&2; \
				printf '%s\n' "$$gone" | sed 's/^/  /' >&2; \
				echo "  a Go caller compiled against these is broken; ride a minor bump, or restore them" >&2; \
				fail=1; \
			else \
				echo "make: $(VERSION) removes from $$pkg, what $$base exported (a minor bump, so this is allowed):" >&2; \
				printf '%s\n' "$$gone" | sed 's/^/  /' >&2; \
				if awk -v v='$(VERSION)' 'BEGIN{b=0} index($$0, "\#\# [" v "]") == 1 {f=1; next} /^\#\# \[/ {f=0} f && /^\#\#\# Breaking$$/ {b=1} END{exit (b==0)}' CHANGELOG.md; then \
					echo "  recorded under the 'Breaking' heading in CHANGELOG.md" >&2; \
				else \
					echo "  and CHANGELOG.md has no 'Breaking' heading under its $(VERSION) section" >&2; \
					echo "  a caller upgrading reads the changelog, not this message: a removal nothing there names" >&2; \
					echo "  is a break the release notes do not admit" >&2; \
					fail=1; \
				fi; \
			fi; \
		fi; \
	done; \
	exit $$fail

GO          ?= go
# go.mod's go line is the compiler pin. GOTOOLCHAIN=auto would keep a newer
# host toolchain (and its GOEXPERIMENT defaults), so two machines would emit
# different binaries from the same source.
GO_VERSION  := $(shell awk '/^go / { print $$2; exit }' go.mod)
ifeq ($(GO_VERSION),)
$(error go.mod has no 'go' line; cannot pin GOTOOLCHAIN)
endif
export GOTOOLCHAIN := go$(GO_VERSION)
# A host on another toolchain gets $(GOTOOLCHAIN) downloaded on the first go
# command, and that download is the first thing to fail on a machine without
# network: behind a proxy, offline, or with an unreachable GOPROXY, the go
# command prints "go: downloading goX ... verifying module ... permission
# denied" or "... 404 Not Found" and the recipe ends there. Those read as a
# broken module cache or a corrupt go.sum, which neither is, and neither of
# which the fix involves. Ask once at parse time instead, so the answer names
# the pinned toolchain and the two ways out. Nothing is fetched when the host
# already runs the pin, so a fully cached checkout pays only the env read.
GO_HAS_TOOLCHAIN := $(shell $(GO) env GOVERSION 2>/dev/null)
ifneq ($(GO_HAS_TOOLCHAIN),)
ifneq ($(GO_HAS_TOOLCHAIN),$(GOTOOLCHAIN))
GO_FETCH_TOOLCHAIN := $(shell $(GO) version 2>&1 >/dev/null)
ifneq ($(GO_FETCH_TOOLCHAIN),)
$(error make: go.mod pins $(GOTOOLCHAIN), which GOTOOLCHAIN selects, and the host go ($(GO_HAS_TOOLCHAIN)) cannot fetch it: $(GO_FETCH_TOOLCHAIN) Install that toolchain, or point GOPROXY at a module proxy this machine can reach)
endif
endif
endif
# A go.work in this directory or any parent puts the build in workspace mode
# and resolves the module graph through it, so the same source builds a
# different binary depending on what sits above the checkout. Off is the
# one-tree answer, and check-ci-env holds the workflows to it too.
export GOWORK := off
# Instruction-set baselines: an ambient GOAMD64=v3 would change amd64 artifacts.
export GOAMD64 := v1
export GOARM64 := v8.0
# The crypto module, the same class of hole. GOFIPS140 is read by the
# compiler, so an ambient GOFIPS140=latest on a developer machine links a
# different crypto implementation than the released binary carries, from
# identical source. off is the toolchain default and what CI has no value
# for, so naming it here is what keeps the two in step.
export GOFIPS140 := off
# Race tests turn cgo on in their recipes. Everything else matches the
# released artifacts, including vet/staticcheck so they analyze the same
# net resolver the binaries ship.
export CGO_ENABLED := 0
# Drop ambient GOFLAGS/GOEXPERIMENT so a developer's shell cannot change
# the artifact (GOFLAGS=-race on make build, extra experiments on codegen).
export GOFLAGS :=
export GOEXPERIMENT :=
# Drop ambient GODEBUG for the same reason GOEXPERIMENT is dropped: it is a
# build input the compiler reads (gotypesalias=1 changes type identity), so a
# developer debugging with GODEBUG=httplaxcontentlength=1 would get a binary
# the merge gate never saw. GOEXPERIMENT's defaults come from go.mod's go
# line, so an empty GODEBUG keeps those and loses nothing.
export GODEBUG :=
# Ignore the go command's own persistent config file (~/.config/go/env, or
# whatever `go env -w` wrote). It is the one ambient state that can still
# change these bytes: `go env -w GOAMD64=v3` or `GOEXPERIMENT=loopvar` beats
# nothing here, it is read before the exports above reach the go command, and
# a value set there wins over the env. Everything the build needs is stated
# in this file or in go.mod, so the config file is only ever drift. CI has
# none, which is why the gap only shows on a developer machine and only after
# someone has run `go env -w`.
export GOENV := off
# Same ambient-state hole, from the environment: any of these makes the go
# command skip the checksum database and accept a module the go.sum lines do
# not cover, so a tampered or repackaged module builds here and nowhere else.
# -mod=readonly governs which versions resolve, not whether their bytes were
# checked.
export GOPRIVATE :=
export GONOSUMDB :=
export GOINSECURE :=
# The other two halves of the same switch. GONOPROXY and GONOSUMDB are set
# independently of GOPRIVATE rather than inherited from it, so clearing
# GOPRIVATE alone leaves the bypass reachable: a pattern in either names the
# modules fetched straight from their VCS and never looked up in the
# checksum database.
#
# GOSUMDB is the one that has to be named rather than cleared, because it is
# the only thing standing between a build and an unverified tool binary. The
# module pins in go.mod are covered by go.sum, but govulncheck and
# cyclonedx-gomod are run as `go run <tool>@<version>` and are in no go.sum:
# with GOSUMDB off, or a GONOSUMDB pattern covering golang.org/x/vuln, the
# build downloads and executes whatever the network hands back and calls it a
# pinned tool. sum.golang.org is the toolchain default, so naming it changes
# nothing on a clean machine and refuses the override on a dirty one.
export GONOPROXY :=
export GOSUMDB := sum.golang.org
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
# The completion scripts `toktop completion <shell>` prints, the order
# completionShells in cmd/toktop/completion.go publishes them, and the analyzer
# each one is read by. shellcheck has no zsh or fish mode, so those two are
# parsed by the shell that will source them, which is the only reader that
# knows the grammar: `zsh -n` and `fish --no-execute` read a file and report a
# parse error without running it, so a generated script that would fail to load
# in a user's shell fails the gate instead. One list, so a shell added to the
# Go side cannot reach a release with no analyzer and no prereq behind it.
COMPLETION_SHELLS := bash zsh fish
# shellcheck for the bash completion script. The floor is the oldest release
# whose checks this script has to clear under, so a machine with an older
# shellcheck is told rather than reporting a pass from a rule set that predates
# the script's own `# shellcheck disable`. The two parser floors are the oldest
# releases carrying the flags above, for the same reason.
SHELLCHECK_MIN := 0.10.0
SHELLCHECK     := shellcheck
ZSH_MIN        := 5.0.0
ZSH            := zsh
FISH_MIN       := 3.0.0
FISH           := fish
# Defines version_too_old ($1 is the floor, $2 the version on PATH; it returns
# true when that version is below the floor), the same shape as UV_TOO_OLD below
# and for the same reason: a definition and its call in one shell, or the
# function is undefined at the call and every version passes. The floor is an
# argument rather than baked in, so shellcheck, zsh and fish share one compare.
VERSION_OLD = version_too_old() { awk -v min="$$1" -v have="$$2" 'BEGIN { n = split(min, a, "."); m = split(have, b, "."); for (i = 1; i <= (n > m ? n : m); i++) { x = a[i] + 0; y = b[i] + 0; if (y < x) exit 0; if (y > x) exit 1 } exit 1 }'; }
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
# must not ship and leaves these alone, and `make clean` moves them out of the
# way and puts them back, because a marker that says a deploy of this tree is
# live is the record a rollback needs and a build sweep is not entitled to
# take. The one a deploy leaves behind holds a manifest of what it uploaded
# (SITE_DEPLOYINFO), and a rollback moves it with the directory, so the
# version that was undone stays on record.
#
# A deploy writes this marker BEFORE it calls the platform, not after. Written
# after, the record exists only for a run that survived to the end: a deploy
# killed by Ctrl-C, by a closed terminal, or by the health poll never finishing
# leaves the new version live with no marker, and the next `make site-rollback`
# reads "nothing to roll back" and exits. The run that has to be undone is
# exactly the one that did not finish, so writing it after names the only
# deploys that need no undo. Written first it is the opposite: a marker with no
# upload behind it rolls back to the version the previous deploy left serving,
# which is the version that was serving a moment ago, and a deploy that never
# reached the platform leaves nothing to undo at all. The marker is a claim, not
# a receipt, and the claim is cheap to make and cheap to keep.
SITE_DEPLOYED     := $(DIST)/site.deployed
SITE_ROLLED_BACK  := $(DIST)/site.rolled-back
# -bindnow is the Go spelling of -Wl,-z,now: without it the linux ELF ships
# partial RELRO, because the internal linker (CGO stays off) emits DT_BIND_NOW
# for nothing. A no-op on darwin and windows, so one LDFLAGS covers PLATFORMS.
# Empty -buildid= so the GNU build-id note is not a second, toolchain-hash-shaped
# input to the bytes.
LDFLAGS     := -s -w -buildid= -bindnow -X main.version=$(VERSION)
# gofmt from the selected toolchain, not a different major on PATH. Quoted
# because the default Windows install lives under "C:\Program Files\Go", and
# an unquoted $(GOFMT) word-splits there: `make fmt` reports the first half as
# a missing command, and the gate in `check` captures an empty file list and
# passes with nothing checked.
GOFMT = "$$($(GO) env GOROOT)/bin/gofmt"

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
# uv lays a venv out as bin/ on POSIX and Scripts\ on Windows, and $(OS) is
# the one variable make itself sets per host. Without this every tool in the
# env resolved to a path that does not exist on a Windows developer, which
# CONTRIBUTING.md asks for support.
SCRIPTS_BIN := $(SCRIPTS_ENV)/$(if $(OS),Scripts,bin)
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
# GNU tar does the same with $TAR_OPTIONS. $TAPE names the device `tar -c`
# writes to when no -f is given, and POSIXLY_CORRECT switches tar, sort, grep
# and ls to their POSIX option parsing, which changes how every recipe above
# and below reads its arguments.
export GZIP :=
export TAR_OPTIONS :=
export TAPE :=
export POSIXLY_CORRECT :=

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
#
# The go check runs the compiler with the same GOTOOLCHAIN every recipe gets,
# so the version it reports is the one the first `make build` will actually
# run. Asking `go env` with no override answered from the host toolchain and
# printed "ok" beside a version the pin would go download instead, which is
# exactly the check a new contributor needs: whether the compiler this build
# wants is already here, or whether the first build pays for it.
.PHONY: prereqs
prereqs: ## check every tool the merge gates need, naming all gaps at once
	@fail=0; \
	ok() { printf '  ok       %s\n' "$$1"; }; \
	gap() { printf '  MISSING  %s\n' "$$1" >&2; fail=1; }; \
	$(UV_TOO_OLD); \
	$(VERSION_OLD); \
	if have=$$($(GO) env GOVERSION 2>/dev/null) && [ -n "$$have" ]; then \
		if [ "$$have" = "$(GOTOOLCHAIN)" ]; then \
			ok "go $$have (the exact version go.mod pins; every recipe selects it)"; \
		else \
			ok "go $$have on PATH, $(GOTOOLCHAIN) selected by GOTOOLCHAIN"; \
			printf '  note     the first make build downloads that toolchain over the network\n' >&2; \
		fi; \
	else \
		gap "go is not on PATH, or $(GO) env failed; install Go and the version go.mod pins ($(GO_VERSION))"; \
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
	if command -v $(SHELLCHECK) >/dev/null 2>&1; then \
		have=$$($(SHELLCHECK) --version | awk '/^version:/ { sub(/^version: */, ""); print; exit }'); \
		if version_too_old "$(SHELLCHECK_MIN)" "$$have"; then \
			gap "shellcheck $$have on PATH, make check-shell and check-workflow-shell need >= $(SHELLCHECK_MIN)"; \
		else \
			ok "shellcheck $$have (>= $(SHELLCHECK_MIN))"; \
		fi; \
	else \
		gap "shellcheck is not on PATH (make check-shell analyzes the bash completion script, check-workflow-shell the workflows' run: blocks)"; \
	fi; \
	if command -v $(ZSH) >/dev/null 2>&1; then \
		have=$$($(ZSH) --version | awk '{ print $$2; exit }'); \
		if version_too_old "$(ZSH_MIN)" "$$have"; then \
			gap "$(ZSH) $$have on PATH, make check-shell needs >= $(ZSH_MIN)"; \
		else \
			ok "$(ZSH) $$have (>= $(ZSH_MIN))"; \
		fi; \
	else \
		gap "$(ZSH) is not on PATH (make check-shell parses the zsh completion script)"; \
	fi; \
	if command -v $(FISH) >/dev/null 2>&1; then \
		have=$$($(FISH) --version | sed -n 's/.*version //p'); \
		if version_too_old "$(FISH_MIN)" "$$have"; then \
			gap "$(FISH) $$have on PATH, make check-shell needs >= $(FISH_MIN)"; \
		else \
			ok "$(FISH) $$have (>= $(FISH_MIN))"; \
		fi; \
	else \
		gap "$(FISH) is not on PATH (make check-shell parses the fish completion script)"; \
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

# -race and -asan both link runtime/cgo. Honor CC when set; otherwise gcc,
# then clang (Go's search order). make build keeps CGO off; only the test
# builds turn it on. Called, not expanded: $1 names the command that needs the
# compiler and $2 the way to run without it, so `make test-asan` is not told to
# pass RACE=0, which is a flag to `make test` and does nothing here.
NEED_CC = cc="$${CC:-}"; \
	if [ -z "$$cc" ]; then \
		if command -v gcc >/dev/null 2>&1; then cc=gcc; \
		elif command -v clang >/dev/null 2>&1; then cc=clang; \
		fi; \
	fi; \
	if [ -z "$$cc" ] || ! command -v "$$cc" >/dev/null 2>&1; then \
		echo "make: $(1) needs a C compiler (gcc or clang) on PATH" >&2; \
		echo "  CGO stays off for make build; only the test builds turn it on." >&2; \
		[ -z "$(2)" ] || echo "  $(2)" >&2; \
		exit 1; \
	fi
RACE_CC_HINT := Pass RACE=0 to skip race detection: make test RACE=0

.PHONY: test
test: ## run all tests shuffled (both sqlite tag halves); RACE=0 skips -race
	@if [ "$(RACE)" != "0" ]; then $(call NEED_CC,go test -race,$(RACE_CC_HINT)); fi
	CGO_ENABLED=$(if $(filter 0,$(RACE)),0,1) $(GO) test -mod=readonly $(GOTAGS_BARE) $(race_flag)-shuffle=on ./...
	CGO_ENABLED=$(if $(filter 0,$(RACE)),0,1) $(GO) test -mod=readonly $(GOTAGS) $(race_flag)-shuffle=on ./agentusage/...

# The address sanitizer, over both halves of the sqlite tag gate. The race
# detector and -asan are mutually exclusive, so this is a target and not a
# flag on `test`: the go command refuses `-race -asan` together, and a rule
# that only ran under RACE=0 would leave the default loop unsanitized.
#
# What it buys over the race detector: -race finds a data race on memory the
# program already dereferences correctly, and says nothing about the read or
# write that was never in bounds to begin with. toktop parses JSONL and
# SQLite session rows written by other tools, so an out-of-range slice or a
# bad cast in a decoder is the shape of bug this catches and the race
# detector cannot. The instrumentation is in the test binary only, so the
# released artifact is untouched: `test-dist` never passes -asan.
#
# It costs a C compiler (it needs cgo for the runtime's shadow memory) and
# roughly double the run time of the plain loop, which is why it is its own
# target and not part of the edit-test cycle. ci.yml runs it as its own step
# and `make ci` runs it, so the local gate and the merge gate are the same
# list.
#
# ASAN_SKIP is the one test that cannot run here, skipped by name and not by
# package: TestStaticFrameAllocBudget asserts an exact per-frame allocation
# count, and -asan swaps Go's allocator for the runtime's own, so the number
# the assertion reads is the sanitizer's, not the frame path's. The budget is
# a performance ratchet, and the plain loop above still asserts it on the
# uninstrumented allocator, where the count means what it says. Everything
# else runs, so an out-of-bounds access anywhere in the tree still fails here.
ASAN_SKIP := TestStaticFrameAllocBudget
.PHONY: test-asan
test-asan: ## run all tests under the address sanitizer (needs cgo; not combinable with -race)
	@$(call NEED_CC,go test -asan,)
	CGO_ENABLED=1 $(GO) test -mod=readonly $(GOTAGS_BARE) -asan -shuffle=on -skip '$(ASAN_SKIP)' ./...
	CGO_ENABLED=1 $(GO) test -mod=readonly $(GOTAGS) -asan -shuffle=on -skip '$(ASAN_SKIP)' ./agentusage/...

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
	@if [ "$(RACE)" != "0" ]; then $(call NEED_CC,go test -race,$(RACE_CC_HINT)); fi
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
	@if [ "$(RACE)" != "0" ]; then $(call NEED_CC,go test -race,$(RACE_CC_HINT)); fi
	mkdir -p $(DIST)
	CGO_ENABLED=$(if $(filter 0,$(RACE)),0,1) $(GO) test -mod=readonly $(GOTAGS) $(race_flag)-shuffle=on -coverprofile=$(DIST)/coverage.out ./...
	$(GO) tool cover -func=$(DIST)/coverage.out | tail -1

.PHONY: sbom
sbom: ## generate CycloneDX SBOM of all dependencies into dist/
	@$(CHECK_VERSION)
	mkdir -p $(DIST)
	$(GO) run $(SBOM_TOOL) \
		mod -licenses -std -noserial -notimestamp -json -output $(DIST)/$(SBOM_ASSET) .

# The names a dependency uses for its license text, in the order they are
# preferred. A module that grants under a name not on this list ships no grant
# with the binary, so the list is the whole of the search rather than the first
# few names that happen to cover today's tree.
LICENSE_FILES := LICENSE LICENSE.txt LICENSE.md LICENSE-APACHE LICENSE-MIT COPYING COPYING.txt NOTICE

# The SBOM names a license identifier per module; it does not carry the text,
# and MIT, BSD-3-Clause and Apache-2.0 each require the copyright notice or the
# license itself to travel with the redistributed bytes. A bare binary is a
# redistribution, so the texts ship next to it: one section per module whose
# packages the binary links, copied out of the module cache, which is the file
# that module was published with at the version go.mod pins. docs/DEPENDENCIES.md
# records why the direct dependencies are here; this records the grants.
#
# The module list comes from the same build tags the binaries are built with, so
# a module only the sqlite half links is not read out of go.mod by name. A
# module whose directory holds no file on LICENSE_FILES fails the target rather
# than shipping a section that says nothing: an unlocatable grant is the case
# worth hearing about, and a section that admits it still lets the release go
# out unlicensed.
#
# Named with the `$(BINARY)_` prefix so it rides the toktop_* glob into
# checksums.txt and dist-clean's keep pattern like buildinfo does. buildinfo is
# a prerequisite for the ordering, not for anything read here: test-dist opens
# by deleting every `$(BINARY)_*` in dist/, so a licenses file written beside it
# under `make -j` would be swept by the build it was meant to describe.
.PHONY: licenses
licenses: buildinfo ## write the license text of every module the binary links into dist/
	@$(CHECK_VERSION)
	@mkdir -p $(DIST)
	@{ \
		echo "$(BINARY) $(VERSION): third-party license texts"; \
		echo; \
		echo "One section per module whose packages this binary links, copied from that"; \
		echo "module's own license file at the version go.mod pins."; \
		echo; \
		$(GO) list $(GOTAGS) -deps -f '{{if .Module}}{{.Module.Path}} {{.Module.Version}} {{.Module.Dir}}{{end}}' $(CMD) \
			| sort -u | grep -v "^$$($(GO) list -m) " \
			| while read -r path version dir; do \
				text=""; \
				for name in $(LICENSE_FILES); do \
					if [ -f "$$dir/$$name" ]; then text="$$dir/$$name"; break; fi; \
				done; \
				if [ -z "$$text" ]; then \
					echo "make licenses: $$path $$version ships no license file under $$dir (looked for $(LICENSE_FILES))" >&2; \
					exit 1; \
				fi; \
				echo "================================================================"; \
				echo "$$path $$version"; \
				echo "================================================================"; \
				cat "$$text"; \
				echo; \
			done; \
	} > $(DIST)/$(LICENSES_ASSET)
	@test -s $(DIST)/$(LICENSES_ASSET) || \
		{ echo "make licenses: wrote no module sections; the module list came back empty" >&2; exit 1; }

# The project's own grant, the one the binaries above are covered by. The
# `licenses` asset carries what every dependency asks for; MIT asks the same of
# toktop, and a release publishes binaries carrying no notice otherwise, so
# the text is copied beside them rather than left in the repository a user who
# downloaded a binary may never see. Named with the `$(BINARY)_` prefix, so it
# rides the toktop_* glob into checksums.txt and dist-clean's keep pattern like
# the other text assets, and it takes buildinfo as a prerequisite for the same
# ordering reason: test-dist opens by deleting every `$(BINARY)_*` in dist/.
.PHONY: license
license: buildinfo ## copy this project's own LICENSE into dist/ beside the third-party texts
	@$(CHECK_VERSION)
	@test -s LICENSE || { echo "make license: LICENSE is missing or empty at the tree root" >&2; exit 1; }
	@mkdir -p $(DIST)
	cp LICENSE $(DIST)/$(LICENSE_ASSET)
	@cmp -s LICENSE $(DIST)/$(LICENSE_ASSET) || \
		{ echo "make license: the copy in dist/ does not match LICENSE" >&2; exit 1; }

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
	# 768w against 13,563 at -q 40, a 22% cut of the image that is 73% of a
	# phone's 14,571-byte visit, at 30.0 dB PSNR against the resized source.
	# The page draws that candidate into about 662 device pixels, so the
	# browser downscales it, and a 2x crop of the 768w frame shows no artifact a
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
# macOS); it lives under dist/, and `make clean` refuses to run while the
# directory is there, since a lock nobody holds is not a lock and a `rm -rf
# dist` beside a running deploy would free one. A lock left by a dead process
# is removed by hand, which is what the refusal says.
#
# $(SITE_GUARD) opens the recipe: one shell takes the lock, arms the trap that
# releases it, and defines wait_for_site for the recipe to call once the
# platform call is done. Every recipe that calls wrangler must open with it
# and carry the rest of its work on the same recipe line: a second line is a
# second shell, so the trap would fire and free the lock before the deploy ran.
#
# The curl check comes before the lock and before the platform call, for the
# reason every other target here names its own tools (require-bun, gh in
# release-verify, magick/avifenc in require-encoders, shellcheck/zsh/fish in
# check-shell). wait_for_site reads the health endpoint over curl, so a
# machine without one makes every attempt below fail for a reason that has
# nothing to do with the site: the poll would run all
# $(SITE_HEALTH_TRIES) attempts, sleep $(SITE_HEALTH_WAIT)s between each, and
# then report "$(SITE_HEALTH_URL) never answered ok", which reads as a broken
# or half-applied upload and sends the operator to roll back a deploy that
# published perfectly. curl is the one tool this path takes from the machine
# without naming it, so it is named here; the check is a command -v, so it
# costs nothing and the health poll's own --max-time still bounds the rest.
define SITE_GUARD
# The lock below is a directory under $(DIST), so $(DIST) has to exist first.
# The failure is named here rather than left to the lock: taking the lock
# without its parent answers the same "cannot create directory" the mkdir above
# did, and the branch after it reads that as another deploy or rollback holding
# the lock, which sends the operator looking for a process instead of at a
# read-only or full filesystem.
mkdir -p $(DIST) || { \
	echo "make: cannot create $(DIST), and the deploy lock, the deploy record and the health poll all live under it" >&2; \
	echo "  fix the directory (permissions, a full disk), then run the target again" >&2; \
	exit 1; \
}; \
command -v curl >/dev/null 2>&1 || { \
	echo "make: curl is not on PATH; it is what reads $(SITE_HEALTH_URL) to confirm the site is serving" >&2; \
	echo "  install it, or the health poll below cannot distinguish a broken deploy from a missing curl" >&2; \
	exit 1; \
}; \
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

# What the deploy uploaded, written beside the marker that says one is waiting
# to be undone. A release ships the same facts in buildinfo.txt; the site has
# no artifact, so without this the only record of what is serving is a
# timestamp in the Cloudflare dashboard. `dirty: true` is the ALLOW_DIRTY=1
# case, recorded the way buildinfo records it rather than dropped: those bytes
# came from a tree no commit holds, which is the fact an operator needs first.
#
# The captures line hashes a listing of per-file digests rather than the files
# concatenated, so a renamed capture changes it and the manifest still says
# which files were uploaded. sha256sum and shasum are the same fork the
# checksums target makes, and the function form takes a file or stdin.
define SITE_DEPLOYINFO
if command -v sha256sum >/dev/null 2>&1; then sha256() { sha256sum "$$@"; }; else sha256() { shasum -a 256 "$$@"; }; fi; \
{ \
	echo "commit: $$(git rev-parse HEAD 2>/dev/null || echo unknown)"; \
	if [ -n "$$(git status --porcelain -- site wrangler.jsonc 2>/dev/null)" ]; then echo "dirty: true"; else echo "dirty: false"; fi; \
	echo "wrangler: $(WRANGLER)"; \
	echo "bun: $$(tr -d '[:space:]' < .bun-version 2>/dev/null || echo unknown)"; \
	echo "worker: $$(sha256 site/worker.js | cut -d' ' -f1)"; \
	echo "captures: $$(find site/public -type f | LC_ALL=C sort | while IFS= read -r f; do printf '%s  %s\n' "$$(sha256 "$$f" | cut -d' ' -f1)" "$$f"; done | sha256 | cut -d' ' -f1)"; \
} > $(SITE_DEPLOYED)/manifest
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
# Both extensions GitHub Actions reads, not just the one this tree happens to
# use: a workflow written as .yaml runs in CI with the job's token and its
# network exactly like a .yml, and a glob naming one extension leaves every
# gate below answering about a file that no longer carries the workflow.
WORKFLOWS := $(wildcard .github/workflows/*.yml .github/workflows/*.yaml)
# check-workflow-shell's extractor, and where it writes. dist/ is where the
# completion scripts are generated to as well and .gitignore covers it, so an
# extracted block is a build output rather than a second copy of a workflow
# that nothing keeps in step with the first.
WF_SHELL_AWK  := $(CURDIR)/scripts/workflow-run-blocks.awk
WF_SHELL_DIR := $(DIST)/workflow-shell

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

# Every build input this Makefile exports, with the value a released artifact
# depends on. A workflow step that runs go directly (ci.yml's gofmt, go mod
# tidy, go test, go install tool and the matrix compiles) does not inherit the
# exports above: those are recipe-local to make, so in a workflow the same keys
# have to be spelled in the env block or the runner's own environment reaches
# the compiler. A GitHub-hosted runner keeps those values empty today, which is
# what hides the gap: an image built with `go env -w GOAMD64=v3` or an
# inherited GOPRIVATE sets them, and then a platform compiles differently from
# the one the release path ships. Names AND values, so a rename and a re-pin
# both fail.
#
# GOTOOLCHAIN is here for the reason the export above exists: its default is
# auto, which keeps whatever compiler the runner image carries rather than the
# one go.mod pins, along with that toolchain's GOEXPERIMENT defaults. A setup-go
# step installs the pinned version but does not select it for later commands, so
# every `go` line in a workflow outside make read the runner's toolchain. The
# value is spelled from $(GO_VERSION), so a go.mod bump fails here until the
# workflows are updated with it, the same rule as the rest of the list.
CI_ENV_REQUIRED := \
	LC_ALL=C TZ=UTC \
	GOTOOLCHAIN=go$(GO_VERSION) \
	GOAMD64=v1 GOARM64=v8.0 GOFIPS140=off CGO_ENABLED=0 \
	GOFLAGS= GOEXPERIMENT= GODEBUG= GOENV=off GOWORK=off \
	GOPRIVATE= GONOSUMDB= GONOPROXY= GOINSECURE= GOSUMDB=sum.golang.org

.PHONY: check-ci-env
check-ci-env: ## fail unless every workflow env block pins the build inputs the Makefile exports
	@fail=0; \
	for spec in $(CI_ENV_REQUIRED); do \
		key=$${spec%%=*}; want=$${spec#*=}; \
		for wf in $(WORKFLOWS); do \
			if have=$$(awk -v key="$$key" ' \
				/^ *env: *$$/ { envind = match($$0, /[^ ]/); inenv = 1; next } \
				inenv { \
					if ($$1 ~ /^#/) next; \
					m = match($$0, /[^ ]/); \
					if (m > 0) { \
						if (m <= envind) { inenv = 0; next } \
						if ($$1 == key ":") { \
							g = $$0; sub(/^[^:]*:[ ]*/, "", g); \
							gsub(/"/, "", g); sub(/[ ]+$$/, "", g); \
							print g; found = 1; exit; \
						} \
					} \
					next; \
				} \
				END { if (!found) exit 1 }' "$$wf"); then :; else have="<unset>"; fi; \
			if [ "$$have" != "$$want" ]; then \
				echo "make check-ci-env: $$wf does not pin $$key='$$want' in an env block (found '$$have')," >&2; \
				echo "  so a go step there reads the runner's value instead of the one the Makefile exports;" >&2; \
				echo "  add $$key to the workflow env block, or change it in both places at once" >&2; \
				fail=1; \
			fi; \
		done; \
	done; \
	exit $$fail

.PHONY: check-ci-platforms
check-ci-platforms: ## fail if the ci.yml build matrix does not match PLATFORMS
	@in_workflow=$$(sed -n -E 's/.*goos:[[:space:]]*([a-z0-9]+)[^a-z0-9]*goarch:[[:space:]]*([a-z0-9]+).*/\1\/\2/p' $(CI_WORKFLOW) | sort); \
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
# docs/openapi.yaml is the third kind of file: not a gate, but the
# machine-readable feed contract internal/ingest's test reads, and the only
# YAML in the tree outside .github. It was the same unmapped document for the
# same reason, and a duplicate key or a bad indent in it fails the Go test that
# parses it with a message about a path rather than about the line that is
# wrong.
#
# yamllint runs from the scripts env, the one black, ruff and mypy run from, so
# its pin is a line in scripts/requirements-dev.txt with a hash on the wheel
# like theirs. `uvx yamllint@1.38.0` pinned the linter and nothing else: uvx
# resolves the linter's own requirements out of the index every run, so pyyaml
# and pathspec arrived at whatever the registry served that minute, unhashed
# and unreviewed, on the one gate in the tree with no fixed closure. A gate
# whose linter can change under it is a gate nobody can reproduce a failure of.
# dependabot.yml is not a workflow, but it is the file that keeps the pinned
# action SHAs, the go module and the Python pins current, and GitHub reads it
# the same way: a YAML error in it does not fail a run, it stops every update
# from being offered and the tree keeps building on pins nobody offered to
# move. Same argument as the workflows, so the same gate.
#
# --strict makes a warning fail the gate. yamllint reports several default rules
# at warning level and exits 0 on them, so without it the tree is a gate that
# passes with findings on it: truthy ships a warning, so `cache: "true"` would
# print and the run would still be green. The three rules disabled in .yamllint
# are the only ones that warned, and they are disabled on the merits, so nothing
# else in the set is at warning level today and --strict costs nothing. ci.yml
# calls this target rather than yamllint directly, so the flag is the same
# locally and remotely.
# Every non-jsonc YAML document in the tree, so a new one has to be added here
# deliberately rather than linting by accident on whatever a glob returned.
YAML_FILES := $(WORKFLOWS) .github/dependabot.yml docs/openapi.yaml

# Three shell scripts ship from this tree, one per shell in $(COMPLETION_SHELLS),
# and each is a completion a user's shell sources. The bash one carried a
# `# shellcheck disable=SC2207` for a rule no gate here ever ran, which is worse
# than no suppression at all: it reads as a check that passed. They are all
# checked by generating them and handing the output to an analyzer, because a
# copy kept beside the Go source goes stale the day a flag moves, and the flag
# list is built from the FlagSet at run time. The generated files are under
# dist/, which .gitignore covers.
#
# The zsh and fish scripts were the two that shipped with no analyzer at all,
# which is the state this target existed in for bash before it was fixed. A
# completion that does not parse is not a degraded feature, it is a script the
# sourcing shell rejects outright, and nothing in the tree noticed because
# nothing read them. shellcheck has no zsh or fish mode, so the reader is the
# shell itself: `zsh -n` and `fish --no-execute` parse without executing, which
# is the grammar that would have rejected the file anyway.
.PHONY: check-shell
check-shell: ## analyze the bash, zsh and fish completion scripts 'toktop completion <shell>' prints
	@for tool in $(SHELLCHECK) $(ZSH) $(FISH); do \
		command -v $$tool >/dev/null 2>&1 || { \
			echo "make check-shell: $$tool is not on PATH; the $(COMPLETION_SHELLS) completion scripts in cmd/toktop/completion.go are shell code and no other analyzer reads them" >&2; \
			exit 1; \
		}; \
	done
	@$(VERSION_OLD); \
	have=$$($(SHELLCHECK) --version | awk '/^version:/ { sub(/^version: */, ""); print; exit }'); \
	if version_too_old "$(SHELLCHECK_MIN)" "$$have"; then \
		echo "make check-shell: $(SHELLCHECK) $$have is older than $(SHELLCHECK_MIN), so the run below would clear a script against a rule set that predates its own '# shellcheck disable' comment" >&2; \
		exit 1; \
	fi; \
	have=$$($(ZSH) --version | awk '{ print $$2; exit }'); \
	if version_too_old "$(ZSH_MIN)" "$$have"; then \
		echo "make check-shell: $(ZSH) $$have is older than $(ZSH_MIN), so the parse below would clear a script against a parser that predates the flag it passes" >&2; \
		exit 1; \
	fi; \
	have=$$($(FISH) --version | sed -n 's/.*version //p'); \
	if version_too_old "$(FISH_MIN)" "$$have"; then \
		echo "make check-shell: $(FISH) $$have is older than $(FISH_MIN), so the parse below would clear a script against a parser that predates the flag it passes" >&2; \
		exit 1; \
	fi
	@mkdir -p $(DIST)
	@for shell in $(COMPLETION_SHELLS); do \
		$(GO) run -mod=readonly $(GOTAGS) $(CMD) completion $$shell > $(DIST)/completion.$$shell || exit 1; \
	done
	@$(SHELLCHECK) $(DIST)/completion.bash
	@$(ZSH) -n $(DIST)/completion.zsh
	@$(FISH) --no-execute $(DIST)/completion.fish

# The workflows are shell code too, and they are the shell code every gate in
# this repo is reached through: a `run:` block installs the toolchain, names
# the tags, and calls the make target that does the analyzing. yamllint parses
# the document and biome does not read it, so before this target a workflow
# block was shell nothing read. The ci.yml step that installs zsh and fish and
# then calls check-shell is the sharpest case: an unquoted variable in that
# block is a gate that installs less than it says and reports a pass anyway.
#
# The blocks are extracted rather than linted where they lie, for the reason
# api-surface.awk is generated too: there is no file to hand a linter, and a
# copy kept beside the workflow goes stale the day a step moves. One file per
# block, named for the workflow and the line its `run:` key is on, so a finding
# points at the step and a `# shellcheck disable` scopes to one step rather than
# to every step in the same file. The generated files live under dist/.
#
# SC2154 is excluded, and only here: GITHUB_OUTPUT, RELEASE_REPO and the rest
# are set by the job's env block, not by the step that reads them, so a block
# that never assigns one is correct. Every other default check applies, and
# `make check` and CI run this the same way they run check-shell.
.PHONY: check-workflow-shell
check-workflow-shell: ## run shellcheck over the bash in every workflow 'run:' block
	@command -v $(SHELLCHECK) >/dev/null 2>&1 || { \
		echo "make check-workflow-shell: $(SHELLCHECK) is not on PATH; the bash in .github/workflows/ is the code every gate runs through and no other analyzer reads it" >&2; \
		exit 1; \
	}
	@$(VERSION_OLD); \
	have=$$($(SHELLCHECK) --version | awk '/^version:/ { sub(/^version: */, ""); print; exit }'); \
	if version_too_old "$(SHELLCHECK_MIN)" "$$have"; then \
		echo "make check-workflow-shell: $(SHELLCHECK) $$have is older than $(SHELLCHECK_MIN), so the run below would clear a workflow against a rule set that predates its own '# shellcheck disable' comment" >&2; \
		exit 1; \
	fi
	@rm -rf $(WF_SHELL_DIR)
	@mkdir -p $(WF_SHELL_DIR)
	@for wf in $(WORKFLOWS); do \
		awk -v outdir=$(WF_SHELL_DIR) -f $(WF_SHELL_AWK) "$$wf" || exit 1; \
	done
	@set -- $(WF_SHELL_DIR)/*.bash; \
	if [ ! -e "$$1" ]; then \
		echo "make check-workflow-shell: no 'run:' block extracted from $(WORKFLOWS); the extractor is not reading what CI runs" >&2; \
		exit 1; \
	fi; \
	$(SHELLCHECK) -e SC2154 "$$@"
	@rm -rf $(WF_SHELL_DIR)

.PHONY: check-yaml
check-yaml: ## fail if a workflow, .github/dependabot.yml or docs/openapi.yaml is invalid YAML or breaks the .yamllint rule set
	@$(MAKE) --no-print-directory scripts-env
	@$(SCRIPTS_BIN)/yamllint --strict --config-file .yamllint $(YAML_FILES)

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

# The release path will not package a tree with uncommitted changes
# (check-release-source, override ALLOW_DIRTY=1), so shipped bytes always match
# the commit buildinfo names. Nothing reaches site-deploy: it uploads the
# working tree, so an uncommitted edit to worker.js or to a capture under
# site/public goes to production and is held by no branch, no tag and no
# commit. `make site-rollback` undoes the upload, not the edit, and the next
# deploy by anyone else puts those bytes back.
#
# Scoped to what wrangler uploads. A contributor with Go work in progress can
# still deploy the site; what must not ship is a Worker or a capture that
# exists only on this machine. Without a checkout there is no commit to
# compare, so the deploy is refused by the same rule and the same named
# override the release path uses.
.PHONY: check-deploy-source
check-deploy-source: ## fail unless the files 'make site-deploy' uploads are committed
	@if ! git rev-parse HEAD >/dev/null 2>&1; then \
		echo "make site-deploy: no git checkout here, so the Worker that would go to production is in no commit:" >&2; \
		echo "  deploy from the repository, or pass ALLOW_DIRTY=1 to deploy this tree as it stands" >&2; \
		exit 1; \
	fi; \
	if [ "$(ALLOW_DIRTY)" != "1" ]; then \
		dirty=$$(git status --porcelain -- site wrangler.jsonc); \
		if [ -n "$$dirty" ]; then \
			echo "make site-deploy: site/ or wrangler.jsonc has uncommitted changes, so production would be serving bytes no branch holds:" >&2; \
			printf '%s\n' "$$dirty" | sed 's/^/  /' >&2; \
			echo "  commit them, or pass ALLOW_DIRTY=1 to deploy the tree as it stands" >&2; \
			exit 1; \
		fi; \
	fi

.PHONY: site-deploy
site-deploy: require-bun site-lint site-check check-wrangler-doc check-deploy-source ## gate with site-lint/site-check, deploy the site Worker at the WRANGLER pin, then wait for /health
	@$(SITE_GUARD) \
	mkdir -p $(SITE_DEPLOYED) || { echo "cannot record $(SITE_DEPLOYED) before deploying, so a run interrupted here would leave a live upload that 'make site-rollback' cannot find" >&2; exit 1; }; \
	rm -rf $(SITE_ROLLED_BACK); \
	$(SITE_DEPLOYINFO) || { echo "cannot write the manifest in $(SITE_DEPLOYED); the audit trail of what is serving is in the Cloudflare deployment log" >&2; exit 1; }; \
	(cd site && bunx wrangler@$(WRANGLER) deploy) || { echo "not deployed; $(SITE_DEPLOYED) records this run, so 'make site-rollback' still has something to act on" >&2; exit 1; }; \
	wait_for_site || { echo "deploy finished but the site is not serving; roll back with 'make site-rollback'" >&2; exit 1; }

.PHONY: site-rollback
site-rollback: require-bun ## roll the site Worker back to the version before the last deploy, then wait for /health
	@$(SITE_GUARD) \
	if [ ! -d $(SITE_DEPLOYED) ] && [ -d $(SITE_ROLLED_BACK) ]; then \
		echo "nothing to roll back: $(SITE_ROLLED_BACK) records a rollback this tree already did, and it is the only record of one"; \
		echo "'wrangler rollback' with no version undoes the most recent deployment whoever shipped it, so a second run here would roll back that rollback and put the version you just undid back on the site"; \
		echo "to recover from this machine anyway, check the deployment list in the Cloudflare dashboard for what the deploy before this one was, then run 'cd site && bunx wrangler@$(WRANGLER) rollback' once, or check out the commit that served correctly and run 'make site-deploy' from it"; \
		exit 0; \
	fi; \
	if [ ! -d $(SITE_DEPLOYED) ]; then \
		echo "nothing to roll back, and NO ROLLBACK FROM THIS TREE IS ON RECORD: $(SITE_DEPLOYED) is absent, so this machine did not deploy what is serving (or deploys a different checkout than the one running this)" >&2; \
		echo "'wrangler rollback' with no version undoes the most recent deployment whoever shipped it, and that is the only undo the platform has. A wrong rollback puts the broken version back, so read the deployment list in the Cloudflare dashboard first and check what the deploy before the most recent one was:" >&2; \
		echo "  cd site && bunx wrangler@$(WRANGLER) deployments list" >&2; \
		echo "  cd site && bunx wrangler@$(WRANGLER) rollback \[<version-id>\]   # once, on a version you have just read there" >&2; \
		echo "or check out the commit that served correctly and run 'make site-deploy' from it" >&2; \
		echo "(a rollback this tree did leave $(SITE_ROLLED_BACK) behind, and then the undo above would be the second)" >&2; \
		exit 2; \
	fi; \
	rm -rf $(SITE_ROLLED_BACK) || { echo "cannot clear $(SITE_ROLLED_BACK); the deploy it holds was already undone once, so a rollback from here would undo this one as well" >&2; exit 1; }; \
	mv $(SITE_DEPLOYED) $(SITE_ROLLED_BACK) || { echo "cannot move $(SITE_DEPLOYED) aside; no rollback has run, so 'make site-rollback' is still the right next step" >&2; exit 1; }; \
	(cd site && bunx wrangler@$(WRANGLER) rollback) || { echo "not rolled back; $(SITE_ROLLED_BACK) holds what an earlier run already undid, so this deploy is still the one live" >&2; exit 1; }; \
	wait_for_site || { echo "rollback finished but the site is not serving; retry, or read the deployment log in the Cloudflare dashboard" >&2; exit 1; }

# The two empty-marker branches above are not the same answer, and the second
# one is a failure. Both records survive 'make clean' and 'make dist-clean', so
# a rollback from the deploying machine can find them at all; check-site-
# rollback-states is what keeps that true of the answer, and the reasoning
# behind it is there.

# The site deploy and the site rollback are the one pair here whose second run
# does damage, and both are the same defect in opposite directions: the record
# of what a run did to the platform, and when it is written relative to the
# platform call. Written after the call, a run killed before its recipe ends is
# invisible to the undo that needs it, and the undo nobody can perform is the
# one a half-finished deploy requires. Consumed after the call, a rollback
# killed the same way leaves its record in place, and the next run undoes the
# undo: the version somebody is escaping goes straight back onto the site.
#
# Both recipes therefore keep one rule, checked here so a reordering cannot
# reintroduce either shape silently: site-deploy records the deploy before
# calling wrangler, and site-rollback consumes the record before calling it.
#
# Read off the two recipes as one block, recipe lines only, because each is a
# single continued command and the step that comes first on it is the one that
# decides. Comments are dropped because the ones above name all four steps, and
# the block stops before this recipe so nothing here matches itself. The four
# patterns are the record step each target's own command names and the platform
# call each one makes, and the checks below are only the two orders; what the
# steps are, and every failure they can report, is the recipes' own.
.PHONY: check-site-records
check-site-records: ## fail unless site-deploy records the deploy before calling wrangler and site-rollback consumes it before calling wrangler
	@block() { awk '/^site-deploy:/ { inrecipes = 1 } /^\.PHONY: check-site-records/ { inrecipes = 0 } inrecipes && /^\t/ { print }' $(MAKEFILE_LIST); }; \
	deploy_mark=$$(block | grep -F -n 'mkdir -p $$(SITE_DEPLOYED)' | head -1 | cut -d: -f1); \
	deploy_call=$$(block | grep -F -n 'wrangler@$$(WRANGLER) deploy)' | head -1 | cut -d: -f1); \
	rollback_move=$$(block | grep -F -n 'mv $$(SITE_DEPLOYED)' | head -1 | cut -d: -f1); \
	rollback_call=$$(block | grep -F -n 'wrangler@$$(WRANGLER) rollback)' | head -1 | cut -d: -f1); \
	fail=0; \
	if [ -z "$$deploy_mark" ] || [ -z "$$deploy_call" ] || [ -z "$$rollback_move" ] || [ -z "$$rollback_call" ]; then \
		echo "make check-site-records: cannot find the record and platform-call steps of site-deploy and site-rollback" >&2; \
		fail=1; \
	elif [ "$$deploy_mark" -gt "$$deploy_call" ]; then \
		echo "make check-site-records: site-deploy records the deploy after it calls wrangler, so a deploy interrupted" >&2; \
		echo "  before its recipe ends leaves a live upload that 'make site-rollback' reports as nothing to undo" >&2; \
		fail=1; \
	elif [ "$$rollback_move" -gt "$$rollback_call" ]; then \
		echo "make check-site-records: site-rollback consumes the record after it calls wrangler, so a rollback interrupted" >&2; \
		echo "  before its recipe ends leaves that record in place and the next run rolls back the rollback" >&2; \
		fail=1; \
	fi; \
	exit $$fail

# The other half of the same rule, and it is about the two states a rollback
# that finds nothing can be in. $(SITE_DEPLOYED) absent with
# $(SITE_ROLLED_BACK) beside it is a rollback this tree already did: a second
# run must stop, because the versionless rollback the platform offers would
# undo that undo and put the version somebody is escaping back on the site.
# Both absent is the other state entirely. Nothing on this machine deployed
# what is serving, or this is not the checkout that did, so the site is
# broken and the platform holds the only undo there is; reporting that as
# "nothing to roll back" and exiting 0 ends the incident with the bad Worker
# live, which is the one outcome the marker exists to prevent. It exits 2
# instead and names the deployment list to read and the pinned command to run
# once against a version read from it. It does not call wrangler for the
# operator: a rollback guessed at on a machine that cannot say what the last
# good deployment was is the damage above.
#
# The two answers have to stay apart, and the second one has to stay a
# non-zero exit, so both branches are pinned here: the marker that has to be
# beside it, the marker that must not be, and the order the two are tested in,
# read off the recipe the way check-site-records reads its two steps. A
# rewrite that folds them back into one answer, exits 0 from the second, or
# tests the bare absence first and swallows a rollback already on record fails
# here rather than in the incident.
.PHONY: check-site-rollback-states
check-site-rollback-states: ## fail unless site-rollback tells an already-undone rollback from a deploy it has no record of
	@recipe() { awk '/^site-rollback:/ { inrecipe = 1; next } /^\.PHONY: check-site-rollback-states/ { inrecipe = 0 } inrecipe && /^\t/ { sub(/^\t@/, ""); print }' $(MAKEFILE_LIST); }; \
	already=$$(recipe | grep -F -n '&& [ -d ' | head -1 | cut -d: -f1 | tr -d '[:space:]'); \
	none=$$(recipe | grep -F -n '[ ! -d ' | grep -v -F '&& [ -d ' | head -1 | cut -d: -f1 | tr -d '[:space:]'); \
	stop=$$(recipe | grep -F -n 'exit 0;' | head -1 | cut -d: -f1 | tr -d '[:space:]'); \
	failed=$$(recipe | grep -F -n 'exit 2;' | head -1 | cut -d: -f1 | tr -d '[:space:]'); \
	fail=0; \
	if [ -z "$$already" ] || [ -z "$$none" ] || [ -z "$$stop" ] || [ -z "$$failed" ]; then \
		echo "make check-site-rollback-states: cannot find both empty-marker branches of site-rollback and their two exits" >&2; \
		fail=1; \
	elif [ "$$none" -lt "$$already" ]; then \
		echo "make check-site-rollback-states: site-rollback tests the bare absence of the deploy record before it tests the" >&2; \
		echo "  rollback already on record, so the second branch also catches the first and the answer is wrong in both states" >&2; \
		fail=1; \
	elif [ "$$failed" -le "$$stop" ]; then \
		echo "make check-site-rollback-states: site-rollback reports a broken site with no record of a deploy as exit 0" >&2; \
		echo "  (the 'nothing to roll back' branch), so an incident ends with the bad Worker live because the recovery" >&2; \
		echo "  step said there was nothing to do; that state has to be a non-zero exit" >&2; \
		fail=1; \
	fi; \
	exit $$fail

# wait_for_site reads the health endpoint over curl, and curl is the one tool
# the deploy path takes from the machine without naming it. Unguarded, a
# machine without curl made every attempt fail for a reason that has nothing
# to do with the site, so the poll ran all $(SITE_HEALTH_TRIES) attempts,
# slept $(SITE_HEALTH_WAIT)s between each, and then reported the site was not
# serving. That reads as a broken or half-applied upload and sends the
# operator to roll back a deploy that published perfectly, and the cost is
# borne in the minutes the poll spends proving nothing.
#
# Every other tool this Makefile takes from the host is named before it is
# needed (require-bun, require-uv, require-encoders, gh in release-verify,
# shellcheck/zsh/fish in check-shell), so this is the same rule for the one
# that was missing. The check belongs in $(SITE_GUARD), which both recipes
# open with, and before the lock: a machine missing curl should cost a
# command -v, not a lock that a second deploy then waits on.
#
# The order is checked here so a reordering cannot reintroduce the shape
# silently. $(SITE_GUARD) is a define, so it is read off the Makefile the same
# way the two recipes above are: the curl check before the lock, and both
# before anything the deploy itself does. A check moved after the wrangler
# call is a deploy that has already uploaded by the time the missing tool is
# noticed, which is the half of this no ordering rule below can undo.
.PHONY: check-site-tools
check-site-tools: ## fail unless the site deploy path names curl before taking the lock
	@guard=$$(awk '/^define SITE_GUARD/ { inguard = 1 } /^endef/ { inguard = 0 } inguard { print }' $(MAKEFILE_LIST)); \
	curl_line=$$(printf '%s\n' "$$guard" | grep -F -n 'command -v curl' | head -1 | cut -d: -f1 || true); \
	lock_line=$$(printf '%s\n' "$$guard" | grep -F -n 'mkdir $$(SITE_LOCK)' | head -1 | cut -d: -f1 || true); \
	if [ -z "$$curl_line" ]; then \
		echo "make check-site-tools: the site deploy path does not check for curl" >&2; \
		echo "  wait_for_site reads $(SITE_HEALTH_URL) over curl, so a machine without one runs every" >&2; \
		echo "  attempt, sleeps between each, and then reports the site is not serving, which reads as a" >&2; \
		echo "  broken deploy. Add 'command -v curl' to \$$(SITE_GUARD), before the lock" >&2; \
		exit 1; \
	fi; \
	if [ -z "$$lock_line" ]; then \
		echo "make check-site-tools: cannot find the lock step in \$$(SITE_GUARD)" >&2; \
		exit 1; \
	fi; \
	if [ "$$curl_line" -gt "$$lock_line" ]; then \
		echo "make check-site-tools: \$$(SITE_GUARD) takes the deploy lock before it checks for curl, so a machine" >&2; \
		echo "  missing curl holds the lock and a second deploy on the same checkout waits on it" >&2; \
		exit 1; \
	fi; \
	for target in site-deploy site-rollback; do \
		if ! awk -v t="$$target:" -v g='$$(SITE_GUARD)' '$$0 ~ "^" t { inrecipe = 1; next } inrecipe && /^\t/ { if (index($$0, g)) found = 1; next } inrecipe { inrecipe = 0 } END { exit !found }' $(MAKEFILE_LIST); then \
			echo "make check-site-tools: $$target does not open its recipe with \$$(SITE_GUARD), so the curl check does not reach it" >&2; \
			exit 1; \
		fi; \
	done

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
check: ## verify go.mod, gofmt -s formatting, vet, staticcheck, the completion and workflow shell, the workflow YAML and the doc guards (CI parity)
	@$(MAKE) --no-print-directory check-test-flags
	@$(MAKE) --no-print-directory check-ci-tags
	@$(MAKE) --no-print-directory check-ci-env
	@$(MAKE) --no-print-directory check-ci-platforms
	@$(MAKE) --no-print-directory check-yaml
	@$(MAKE) --no-print-directory check-help-docs
	@$(MAKE) --no-print-directory check-site-records
	@$(MAKE) --no-print-directory check-site-rollback-states
	@$(MAKE) --no-print-directory check-site-tools
	@$(MAKE) --no-print-directory check-changelog-structure
	@$(MAKE) --no-print-directory check-changelog-covers
	@unformatted=$$($(GOFMT) -s -l .); \
		if [ -n "$$unformatted" ]; then \
			echo "needs gofmt (run 'make fmt'):" >&2; echo "$$unformatted" >&2; exit 1; \
		fi
	@$(MAKE) tidy-check
	@$(MAKE) lint
	@$(MAKE) vet
	@$(MAKE) check-shell
	@$(MAKE) check-workflow-shell

.PHONY: ci
ci: ## Go merge gates: tidy-diff, fmt, lint, vet, govulncheck, race tests, address-sanitized tests
	@$(MAKE) check
	@$(MAKE) govulncheck
	@$(MAKE) test RACE=1
	@$(MAKE) test-asan

.PHONY: pr
pr: ## every PR merge gate except the OS matrix: ci + site-lint + site-check + check-wrangler-doc + scripts-check + repro-check-pair
	@$(MAKE) ci
	@$(MAKE) site-lint
	@$(MAKE) site-check
	@$(MAKE) check-wrangler-doc
	@$(MAKE) scripts-check
	@$(MAKE) repro-check-pair

# `rm -rf dist` takes the site deploy lock with it, and a lock nobody holds is
# not a lock: a `make clean` running beside a `make site-deploy` frees the
# directory the deploy is still using, so a second deploy starts alongside the
# first and the platform keeps whichever upload landed last. dist-clean only
# touches regular files at depth 1 and leaves the lock alone; this is the one
# target that does not.
.PHONY: clean
# The two site markers are build output by location and not by nature: they
# record that a deploy from this tree is still live and waiting to be undone,
# and dist-clean already leaves them alone for that reason. A clean that took
# them would make `make site-rollback` refuse with "nothing to roll back" while
# the deployment it would have undone is the one serving, and a routine clean
# is exactly what somebody runs first when the site looks wrong. So the sweep
# deletes everything else in dist/ and leaves the two where they are.
clean: ## remove build artifacts, keeping the site deploy and rollback markers
	@if [ -d $(SITE_LOCK) ]; then \
		echo "make clean: $(SITE_LOCK) is held; a site deploy or rollback is running." >&2; \
		echo "  wait for it to finish, or remove the directory if that process is gone" >&2; \
		exit 1; \
	fi
	@if [ -d $(DIST) ]; then \
		for entry in $(DIST)/* $(DIST)/.[!.]*; do \
			case "$$(basename $$entry)" in \
				$(notdir $(SITE_DEPLOYED))|$(notdir $(SITE_ROLLED_BACK))) continue ;; \
			esac; \
			rm -rf "$$entry"; \
		done; \
	fi
	rm -rf $(BINARY) coverage.out *.test

.PHONY: check-changelog
check-changelog: ## verify CHANGELOG.md contains release section and link for VERSION
	@$(CHECK_VERSION)
	@$(CHECK_CHANGELOG)

# The shape of CHANGELOG.md, and it holds for every section, not only the one
# being cut. A release accumulates entries under the same impact heading each
# time one lands, and a second `### Added` reads as two groups where a reader
# takes the first as the whole: the cut then fails on a section that was
# malformed long before the tag, and the entries a second heading collected
# would ship unlabelled. Checked here rather than in CHECK_CHANGELOG, which
# only runs with a VERSION, so the shape is refused on the push that breaks it
# instead of on the cut that trips over it.
.PHONY: check-changelog-structure
check-changelog-structure: ## fail if a CHANGELOG.md section repeats an impact heading
	@awk 'function report(	 h) { \
			for (h in seen) if (seen[h] > 1) { \
				printf "make: CHANGELOG.md %s repeats %s %d times; one heading per impact\n", sec, h, seen[h]; \
				d = 1 \
			} \
		} \
		/^## \[/ { report(); sec = $$0; delete seen; next } \
		/^### / { seen[$$0]++ } \
		END { report(); exit (d == 1) }' CHANGELOG.md >&2

.PHONY: check-api
check-api: ## verify VERSION removes nothing PUBLIC_PKGS exported at the last release
	@$(CHECK_API)

# Every gate above reads the changelog's shape, not the diff it describes:
# check-changelog finds the section for the version being cut, and it is
# equally satisfied by notes that name the change and by notes that do not.
# So a change to a surface a reader or a caller reads directly can sit in the
# tree for sixty commits and ship under notes written for the other fifty
# nine, which is the one failure the file exists to prevent and nothing here
# would have seen it. A watched surface that moved since the last release tag
# with CHANGELOG.md untouched since that tag is a change shipping unannounced;
# the answer is an entry, not a reworded gate, so the entry is what is asked
# for.
#
# The watched list is the surfaces a reader or a caller reads rather than
# code it compiles: the README, the --help text, the feed contract the
# OpenAPI describes, the importable package and the Worker the site serves.
# The rest of docs/ is deliberately absent (see CHANGELOG_WATCHED above).
# A commit that touches only Go internals needs no entry, which
# is what keeps this from demanding one per commit. The base is the last
# released tag before HEAD, as check-api takes it, so a cut is measured
# against the release it follows and not against the commit ahead of it. A
# checkout with no tag to compare against, or no git at all, has no diff and
# is let past.
#
# A touched CHANGELOG.md is not an entry for the range. One entry written for a
# different subject satisfies the range test for every other commit in it, and
# that is the shape a real unannounced change takes: a fix commits its notes,
# and a later commit moving a watched surface rides them. The obvious repair is
# to test per commit, and it was measured here: it named 21 commits since
# 0.23.0, 8 of them a refactor, a test or a perf change that the range test's
# own comment says needs no entry (`agentusage` is watched, so a rename of an
# unexported identifier moves it). A rule that fires on those trains the entry
# to be boilerplate, which is the outcome the watched list was narrowed to
# prevent, so the range test stands as it is and the three entries it let
# through since 0.23.0 are written by hand instead.
.PHONY: check-changelog-covers
check-changelog-covers: ## fail if a consumer-facing surface moved since the last release with no CHANGELOG.md entry
	@if ! git rev-parse HEAD >/dev/null 2>&1; then exit 0; fi; \
	base=$$(git describe --tags --abbrev=0 HEAD^ 2>/dev/null) || exit 0; \
	if [ -z "$$base" ]; then exit 0; fi; \
	changed=$$(git diff --name-only "$$base"..HEAD -- $(CHANGELOG_WATCHED)); \
	if [ -z "$$changed" ]; then exit 0; fi; \
	git diff --quiet "$$base"..HEAD -- CHANGELOG.md && { \
		echo "make check-changelog-covers: these moved since $$base and CHANGELOG.md has not been touched since $$base:" >&2; \
		printf '%s\n' "$$changed" | sed 's/^/  /' >&2; \
		echo "  every one of them is a surface a reader or a caller reads, so the change ships under notes" >&2; \
		echo "  that do not mention it. Add the entry under '## [Unreleased]', impact heading and all" >&2; \
		echo "  (build, refactor and test-only commits need no entry and are not watched)" >&2; \
		exit 1; \
	}; \
	exit 0

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
release: check-changelog check-changelog-covers check-api check-release-source sbom checksums ## build every release platform and SBOM into dist/ with reproducible checksums

# Without this, `make -j release` runs the prerequisites above at once:
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
	@rm -f $(DIST)/$(CHECKSUMS_ASSET)

# The SBOM is named with a `-` where the binaries use `_`, so it needs its own
# glob: the `$(BINARY)_*` list below would otherwise skip it. `make release`
# runs `sbom` first; run on its own, the glob matches nothing and the list is
# the binaries alone.
.PHONY: checksums
checksums: sbom buildinfo licenses license ## checksum the dist/ binaries into a byte-reproducible tarball
	@$(TAR) --sort=name -cf /dev/null --files-from /dev/null 2>/dev/null || \
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
	@cd $(DIST) && $(TAR) $(TAR_REPRO) -c -f - checksums.txt | gzip -n -6 > $(CHECKSUMS_ASSET)
# The GNU tar probe above runs the flag rather than asking for the version:
# `tar --sort=name --version` exits 0 on bsdtar as well, since --version
# short-circuits before the unknown option is read, so the guard it was written
# for never fired and a bsdtar packing step failed later with a tar diagnostic
# instead. The check has to make tar use the option.
#
# Pack the same file a second time after stamping its mtime to the epoch, and
# compare. TAR_REPRO promises the archive's metadata comes from
# SOURCE_DATE_EPOCH and the sorted member list, not from the filesystem; two
# packs seconds apart only prove that by accident, whereas moving the input's
# mtime is the exact leak TAR_REPRO exists to close. repro-check covers the
# binaries, and the tarball is the one published asset carrying archive metadata
# of its own, so nothing else would catch a --mtime that stopped applying.
# checksums.txt outlives the pack above for this, and is removed here.
	@cd $(DIST) && \
		touch -t 197001010000 checksums.txt && \
		$(TAR) $(TAR_REPRO) -c -f - checksums.txt | gzip -n -6 > $(CHECKSUMS_ASSET).recheck && \
		if cmp -s $(CHECKSUMS_ASSET) $(CHECKSUMS_ASSET).recheck; then \
			rm -f $(CHECKSUMS_ASSET).recheck; \
		else \
			echo "make checksums: $(CHECKSUMS_ASSET) is not the same bytes when the same input is packed again with a different mtime, so the archive is still carrying filesystem metadata:" >&2; \
			rm -f $(CHECKSUMS_ASSET).recheck; exit 1; \
		fi
	@rm -f $(DIST)/checksums.txt

# Binaries of any earlier version are dropped first: leftovers would
# otherwise ride the toktop_* glob into checksums.txt and the release.
#
# dist-clean is a prerequisite here, not a sibling of buildinfo's: the sweep
# deletes every file in dist/ that this VERSION does not publish, so running it
# beside the cross-build deletes binaries as they are written and leaves
# checksums.txt covering fewer files than PLATFORMS. Listing it on both is not
# ordering, and `make -j buildinfo` would race; .NOTPARALLEL covers only the
# `release` target's own prerequisites, so the ordering belongs here where the
# sweep and the build meet.
.PHONY: test-dist
test-dist: dist-clean ## build every release platform without packaging
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

# The path of the artifact for this host, out of $(DIST). The release job
# smoke-tests it, and spelling the name in the workflow instead would be a
# second copy of the naming rule: a BINARY or a VERSION separator that moved
# here would leave the job smoke-testing a file no build wrote, and a
# `test -x` on a path that does not exist is a failure that reads as a broken
# build rather than as a renamed artifact. PLATFORMS is the source of truth
# for what exists; a host outside it is named by the failure below.
.PHONY: host-dist
host-dist: ## print the path of this host's VERSION artifact in dist/
	@$(CHECK_VERSION)
	@goos=$$($(GO) env GOOS); goarch=$$($(GO) env GOARCH); \
	case " $(PLATFORMS) " in \
		*" $$goos/$$goarch "*) ;; \
		*) echo "make host-dist: $$goos/$$goarch is not in PLATFORMS, so 'make release VERSION=$(VERSION)' built no artifact to smoke test" >&2; exit 1;; \
	esac; \
	ext=""; \
	if [ "$$goos" = "windows" ]; then ext=".exe"; fi; \
	echo "$(DIST)/$(BINARY)_$(VERSION)_$${goos}_$${goarch}$$ext"

# What produced the bytes, recorded next to them. A checksum list proves the
# download arrived intact, not which toolchain made it; without the commit,
# the toolchain, and the flags there is nothing faithful to rebuild against.
# `tags` is the value that reached -tags, so the zone tag is in it: the bare
# $(TAGS) named a build nobody made, and this file is what a rebuild is read
# against. `driver_tags` keeps the sqlite gate separable on its own.
# Named to match the toktop_* glob, so it lands in checksums.txt too.
# dist-clean is reached through test-dist, which depends on it, so the sweep
# cannot run beside the cross-build that fills dist/ again.
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
		echo "tags: $(strip $(TAGS) $(ZONE_TAG))"; \
		echo "driver_tags: $(TAGS)"; \
		echo "buildflags: $(GO_BUILDFLAGS)"; \
		echo "ldflags: $(LDFLAGS)"; \
		echo "cgo_enabled: $(CGO_ENABLED)"; \
		echo "goamd64: $(GOAMD64)"; \
		echo "goarm64: $(GOARM64)"; \
		echo "gofips140: $(GOFIPS140)"; \
	} > $(DIST)/$(BUILDINFO_ASSET)

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
#
# The digest tool is chosen once, from the OS the recipe runs on, rather than
# probed at the end: `command -v sha256sum` answers for the shell's PATH, and
# PATH is ambient state, so the same recipe could reach two different tools on
# two machines while reading as the same command. An image whose uname is
# neither Linux nor Darwin is named and refused here rather than falling
# through to a tool nobody checked is there.
.PHONY: release-verify
release-verify: ## fetch every published asset for VERSION and re-verify its checksum (needs gh)
	@$(CHECK_VERSION)
	@command -v gh >/dev/null 2>&1 || { \
		echo "make release-verify: gh is not on PATH; it is what reads the published asset list" >&2; \
		exit 1; \
	}
	@tag=v$(VERSION); \
	dir=$(CURDIR)/$(DIST)/release-verify/$(VERSION); \
	os=$$(uname -s 2>/dev/null || echo unknown); \
	case "$$os" in \
		Linux*) sha256() { sha256sum "$$@"; } ;; \
		Darwin*) sha256() { shasum -a 256 "$$@"; } ;; \
		*) echo "make release-verify: '$$os' is not a runner image this recipe has a digest tool for" >&2; \
		   echo "  it needs sha256sum (Linux) or shasum (macOS), named like the checksums target names them" >&2; \
		   exit 1 ;; \
	esac; \
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
	for asset in $(RELEASE_ASSETS); do printf '%s\n' "$$asset"; done; } | sort > "$$dir/expected.txt"; \
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
	$(TAR) -xzf "$$dir/$(CHECKSUMS_ASSET)" -C "$$dir/sums" || exit 1; \
	listed=$$(cut -c67- "$$dir/sums/checksums.txt" 2>/dev/null | tr -d '*' | sort || true); \
	unlisted=$$(grep -vxF -e "$$listed" -e "$(CHECKSUMS_ASSET)" "$$dir/expected.txt" || true); \
	if [ -z "$$listed" ] || [ -n "$$unlisted" ]; then \
		echo "make release-verify: $$tag's checksums.txt does not cover every artifact it should:" >&2; \
		if [ -n "$$unlisted" ]; then sed 's/^/  UNLISTED   /' <<< "$$unlisted" >&2; fi; \
		echo "  an artifact missing from checksums.txt is one a client cannot verify, so it is not installed." >&2; exit 1; \
	fi; \
	cd "$$dir" && sha256 -c "$$dir/sums/checksums.txt"

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
# (PREFIX=/usr/local or PREFIX=$(brew --prefix)). Empty rather than '/.local'
# when HOME is unset (make under sudo, a systemd unit, a CI container): a
# recursive mkdir of '/.local/bin' then succeeds as root and scatters a user
# binary outside any prefix. install refuses that instead.
PREFIX ?= $(if $(HOME),$(HOME)/.local)

# The name install writes. Windows resolves a command by its PATHEXT, so a
# copy without .exe runs from Git Bash and from nowhere else; the artifact
# rules above already spell the same extension per target.
ifeq ($(OS),Windows_NT)
INSTALL_NAME = $(BINARY).exe
else
INSTALL_NAME = $(BINARY)
endif

# Staging file for the install, so the copy lands beside the destination and
# the rename below is atomic. `install` in place replaces the destination's
# inode (it unlinks first), so there is a window where PREFIX/bin/toktop is a
# copy that has not finished writing; a copy cut short by a full disk or a
# killed make leaves a truncated executable under the installed name. The
# rename has no such window. It also has to be the same directory rather than
# /tmp, because a rename across filesystems is a copy, which is the window
# again.
INSTALL_TMP = .toktop-install-

.PHONY: install
install: build ## install into PREFIX/bin (default ~/.local/bin)
	@if [ -z "$(PREFIX)" ] || [ "$(PREFIX)" = "/" ]; then \
		echo "make install: PREFIX='$(PREFIX)' is not a directory to install into." >&2; \
		echo "  HOME is unset, so ~/.local does not name one here; pass PREFIX=<dir>." >&2; \
		exit 1; \
	fi
	mkdir -p "$(PREFIX)/bin"
	@tmp=$$(mktemp "$(PREFIX)/bin/$(INSTALL_TMP)XXXXXX") || exit 1; \
	trap 'rm -f "$$tmp"' EXIT INT TERM; \
	install -m 0755 $(BINARY) "$$tmp" || exit 1; \
	mv -f "$$tmp" "$(PREFIX)/bin/$(INSTALL_NAME)" || exit 1; \
	trap - EXIT INT TERM
	@case ":$$PATH:" in \
		*":$(PREFIX)/bin:"*) ;; \
		*) echo "make install: installed to $(PREFIX)/bin, which is not on PATH; add it before '$(BINARY)' resolves" >&2; \
		   echo "  export PATH=\"$(PREFIX)/bin:$$PATH\"" >&2 ;; \
	esac

# The remove half of the install path, and it removes only what install put
# there: the installed binary and any staging file a killed install left. It
# says so when there is nothing installed rather than reporting a clean
# uninstall, so a mistyped PREFIX cannot read as a removal that happened.
.PHONY: uninstall
uninstall: ## remove the binary from PREFIX/bin (default ~/.local/bin)
	@if [ -z "$(PREFIX)" ] || [ "$(PREFIX)" = "/" ]; then \
		echo "make uninstall: PREFIX='$(PREFIX)' is not a directory to uninstall from." >&2; \
		exit 1; \
	fi
	@found=0; \
	if [ -f "$(PREFIX)/bin/$(INSTALL_NAME)" ]; then rm -f "$(PREFIX)/bin/$(INSTALL_NAME)" && found=1; fi; \
	for stale in "$(PREFIX)/bin/$(INSTALL_TMP)"*; do \
		[ -e "$$stale" ] || continue; \
		rm -f "$$stale" && found=1; \
	done; \
	if [ "$$found" = 0 ]; then \
		echo "make uninstall: nothing installed under $(PREFIX)/bin" >&2; \
		exit 1; \
	fi
