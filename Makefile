.PHONY: help vitest-unit vitest-browser script-tests go-test-noremote ao-harness-docs methodgen forge-schemas linear-schema go-lint go-lint-whole-tree go-lint-host-files go-vet fmt-check go-vuln bindings-check frontend-lint frontend-deadcode install dev dev-wsl launch-wsl harness-wsl perf-wsl soak soak-check soak-contract build build-wsl test check verify release release-wsl-noremote release-macos go-build netns-tool gocache-trim gocache-trim-tool go-test test-race provider-smoke-compile provider-smoke provider-smoke-revert import-corpus-smoke mockprovider mockforge harness-build e2e-browsers harness harness-window soak-window e2e e2e-mobile-browser release-upgrade-check apk apk-release e2e-android

# Print the supported build, test, harness, and smoke targets. Keep this
# short enough to use from an unfamiliar checkout. `make e2e` is the
# hermetic harness smoke gate. `make provider-smoke` is deliberately separate
# because it spends real provider tokens. `make import-corpus-smoke` reads
# only caller-supplied copies of provider homes.
help:
	@printf '%s\n' \
		'Build:   make build | make check | make verify' \
		'Tests:   make test | make test-race | make go-test GO_TEST_PKGS=./internal/x GO_TEST_FLAGS=... | make go-test-noremote | make vitest-unit | make vitest-browser | make script-tests' \
		'Static:  make fmt-check | make go-vet | make go-lint | make go-lint-whole-tree | make go-lint-host-files | make frontend-lint | make frontend-deadcode | make bindings-check | make go-vuln' \
		'Android: make apk | make apk-release | make e2e-android' \
		'Mobile browser: make e2e-mobile-browser (Chromium + WebKit)' \
		'Harness: make harness | make harness-window | make harness-wsl' \
		'Long-run: make soak | make soak-window | make soak-check' \
		'Perf:    make perf-wsl | make soak-contract' \
		'Smoke:   make e2e (mocked) | make provider-smoke (real tokens)' \
		'Release: make release-upgrade-check [PREVIOUS_RELEASE=vX.Y.Z] [RELEASE_UPGRADE_BOOTSTRAP=1 RELEASE_UPGRADE_SCALE=0.02] [RELEASE_UPGRADE_ARCHIVE_OUT=FILE] [RELEASE_UPGRADE_CORPUS=/data-root]' \
		'Import:  make import-corpus-smoke AO_IMPORT_CORPUS_CLAUDE=/copy AO_IMPORT_CORPUS_CODEX=/copy' \
		'Docs:    make ao-harness-docs' \
		'Codegen: make methodgen | make forge-schemas | make linear-schema'

ao-harness-docs:
	go generate ./cmd/ao-harness

# Regenerate the bound-method classification table and its client-side
# route mirror. ONE command for both halves, because they come out of one
# scan: internal/transport/methods_gen.go and
# frontend/src/lib/transport/methodRoutes.ts. `make go-test`'s
# TestMethodsGen_InSync fails on either being stale and names this target.
methodgen:
	go run ./internal/transport/methodgen

# Refresh the GitHub and gitlab.com GraphQL schemas that internal/git's
# documents and the fake forge's answers are checked against
# (internal/git/forgeschema). It fetches both public schemas on the host
# network with no token; review the diff, then commit it. Tests never
# fetch them.
forge-schemas:
	go run ./internal/git/forgeschema/refresh

# Refresh Linear's public GraphQL schema that internal/tracker/linear's
# documents are checked against (linearschema/schema.graphql). It fetches
# the published SDL on the host network with no key; review the diff,
# then commit it. Tests never fetch it.
linear-schema:
	go run ./internal/tracker/linear/refresh

# `make dev DEBUG=1` / `make dev-wsl DEBUG=1` enables every debug surface
# wired through this Makefile: frontend UI render tracing, raw provider
# stdio capture, and the loopback pprof listener. Use UI_TRACE=1 or
# PROVIDER_DEBUG=1 for narrower captures.
#
# UI_TRACE=1 alone is the LIGHT tier: event traces + spring chase
# telemetry (`scroll.spring.chase`), cheap enough to measure
# production-representative frame cadence (e.g.
# `make build-wsl UI_TRACE=1`). UI_ORACLES=1 adds the heavy standing
# oracles (per-row resize / margin-divergence / reasoning-tail observers)
# and the throttled DOM snapshot walks; DEBUG=1 turns on both. UI_ORACLES
# requires UI_TRACE at runtime — UI_ORACLES=1 without UI_TRACE=1 builds
# the oracle code but the disabled base trace gate keeps everything off.
UI_TRACE ?= $(DEBUG)
UI_ORACLES ?= $(DEBUG)

# DEBUG=1 also starts the loopback pprof listener ([::1]:6363,
# internal/observability/pprofserve). Zero cost until an endpoint is hit
# and never bound beyond loopback. Export AGENT_OVERFLOW_PPROF yourself
# for an explicit addr or to enable it without the rest of DEBUG.
AGENT_OVERFLOW_PPROF ?= $(if $(filter 1,$(DEBUG)),1,)

# Raw provider stdio capture logs land in
# <dbDir>/logs/provider-events-YYYY-MM-DD.ndjson (one JSON object per line:
# {ts, threadId, direction, provider, data}). PROVIDER_DEBUG=1 is the narrow
# shorthand for AGENT_OVERFLOW_DEBUG=provider; DEBUG=1 broadens that to `all`.
ifeq ($(PROVIDER_DEBUG),1)
AGENT_OVERFLOW_DEBUG := provider
endif
ifeq ($(DEBUG),1)
AGENT_OVERFLOW_DEBUG := all
endif

# `make build-wsl WSL_BUILD_MODE=build:dev` produces a dev-mode bundle
# with `import.meta.env.DEV=true` so UI render trace and other DEV-gated
# code light up under the WSL launcher path. Default is the production
# `build` script used by package:wsl:zip and direct distribution builds;
# dev-wsl overrides to `build:dev` automatically. Allowed values are
# enforced by the build-wsl recipe — only `build` and `build:dev` are
# valid pnpm scripts here.
WSL_BUILD_MODE ?= build
WSL_VERSION ?= $(VERSION)

# internal/testutil/reporoot reads this line, so the tests that read the
# Go tree read these packages.
GO_PACKAGE_ROOTS := . ./cmd/... ./internal/...

# Builds and tests run at the lowest CPU priority, so they yield to the
# developer's running app and its provider CLIs. make renices itself once
# while it reads this file; every recipe and everything it starts inherits
# the priority. A make whose goals launch an app for the developer to use
# keeps normal priority; its sub-makes that only build are lowered. Where
# renice fails the build still runs, and says so once.
NICE_EXEMPT_GOALS := dev dev-wsl launch-wsl harness-wsl perf-wsl soak harness harness-window soak-window
ifeq ($(filter $(NICE_EXEMPT_GOALS),$(MAKECMDGOALS))$(AO_RENICE_FAILED),)
AO_RENICE_ERROR := $(shell renice 19 -p $$PPID 2>&1 >/dev/null || echo "exit $$?")
ifneq ($(AO_RENICE_ERROR),)
$(warning could not lower make to nice 19 ($(AO_RENICE_ERROR)); running at normal priority)
export AO_RENICE_FAILED := 1
endif
endif

# Every Go command under make builds with -trimpath. The compiler then leaves
# the checkout's directory out of a package's build cache key, so worktrees
# holding the same sources share cache entries. File paths in stack traces and
# from runtime.Caller become module-relative (agent-overflow/internal/...);
# tests locate the checkout through internal/testutil/reporoot. Flags the
# developer set are kept: GOFLAGS from the make command line or the
# environment, else the value saved with `go env -w`.
override GOFLAGS := $(strip $(filter-out -trimpath,$(or $(GOFLAGS),$(shell go env GOFLAGS 2>/dev/null))) -trimpath)
export GOFLAGS

# Single source of truth for the release version is build/config.yml#info.version.
# `make build` reads it and forwards it via VERSION= to wails3 build, which the
# platform Taskfiles consume as {{.VERSION | default "dev"}} to stamp
# main.version via -ldflags="-X main.version=...". Override at the make command
# line for one-off builds: `make build VERSION=0.0.2-rc1`.
VERSION ?= $(shell grep '^  version:' build/config.yml | sed 's/.*"\(.*\)".*/\1/')

ifeq ($(shell uname -s),Darwin)
# The Go runtime requires macOS 13 Ventura (Go 1.27), on both
# architectures.
MACOSX_DEPLOYMENT_TARGET := 13.0
export MACOSX_DEPLOYMENT_TARGET
export CGO_CFLAGS := -mmacosx-version-min=$(MACOSX_DEPLOYMENT_TARGET)
export CGO_CXXFLAGS := -mmacosx-version-min=$(MACOSX_DEPLOYMENT_TARGET)
# `-Wl,-w` silences ld64's "built for newer macOS version" diff between
# the SDK objects (Apple's installed SDK floor — currently macOS 26 on
# fresh Xcode) and our deployment target, plus the noisy
# "malformed LC_DYSYMTAB" warnings that surface when Go's runtime
# objects hand off to ld64. Both are cosmetic: the resulting binaries
# run on every macOS ≥ MACOSX_DEPLOYMENT_TARGET as advertised. ld64
# has no per-warning suppression so this is the cleanest knob.
export CGO_LDFLAGS := -mmacosx-version-min=$(MACOSX_DEPLOYMENT_TARGET) -Wl,-w
endif

# Use these targets for Go verification instead of bare `go build` / `go test`.
# On Darwin, the exported CGO flags keep Wails Objective-C objects and the final
# binary on the same macOS deployment target, avoiding noisy linker warnings.
#
# The second pass compiles the `nogui` half. That tag selects a whole
# alternative file set (`main_nogui.go`, `internal/app/app_startup.go`'s
# windowless branch, and every `!nogui` desktop file's absence), and TWO
# shipping artifacts are built with it: the Windows launcher's WSL payload and
# the headless Linux binary serve mode runs on. Nothing else in the default
# gates compiles it, so a `!nogui` file gaining a symbol the nogui half does
# not define breaks only the release build, at release time.
#
# It lives here rather than in `check` because `make go-build` is the target
# the repo's own per-task checklist names, and `check`, `verify` and
# `scripts/release-check.sh` all reach it from here. Cost, measured on an idle
# WSL host: ~3s on top of go-build's ~7s warm (most packages have an identical
# file set under both tag sets, so the build cache answers them outright), ~35s
# from a cold cache. `TestGoBuildCompilesTheNoguiHalf` fails if this pass is
# removed.
#
# The `noremote` passes compile the variant without remote access
# (internal/buildvariant): the desktop half the noremote harness binary
# uses, the nogui half its WSL payload ships, and its Windows launcher.
go-build: gocache-trim-tool
	@set -e; $(gocache-trim-around); \
	packages=$$(go list -f '{{if or .GoFiles .CgoFiles}}{{.ImportPath}}{{end}}' $(GO_PACKAGE_ROOTS) | sed '/^$$/d'); \
	if [ -z "$$packages" ]; then echo "ERROR: no Go packages found"; exit 1; fi; \
	go build $$packages; \
	nogui=$$(go list -tags nogui -f '{{if or .GoFiles .CgoFiles}}{{.ImportPath}}{{end}}' $(GO_PACKAGE_ROOTS) | sed '/^$$/d'); \
	if [ -z "$$nogui" ]; then echo "ERROR: no Go packages found for the nogui build"; exit 1; fi; \
	go build -tags nogui $$nogui; \
	noremote=$$(go list -tags noremote -f '{{if or .GoFiles .CgoFiles}}{{.ImportPath}}{{end}}' $(GO_PACKAGE_ROOTS) | sed '/^$$/d'); \
	go build -tags noremote $$noremote; \
	noremote_nogui=$$(go list -tags nogui,noremote -f '{{if or .GoFiles .CgoFiles}}{{.ImportPath}}{{end}}' $(GO_PACKAGE_ROOTS) | sed '/^$$/d'); \
	go build -tags nogui,noremote $$noremote_nogui; \
	GOOS=windows CGO_ENABLED=0 go build -tags noremote -o /dev/null ./cmd/agent-overflow-windows

# Every test runs inside the isolated network namespace (loopback plus a
# private LAN, no route off it; internal/netisolate), so a test that binds
# every interface or dials out reaches nothing beyond the namespace in any
# WSL networking mode, and its temp files land in a private RAM directory
# that goes with the namespace. `go test` itself runs under bin/ao-netns rather than
# through `-exec`, which would disable the test cache. go-prefetch fetches,
# on the host network first, what every package of the module and its tests
# import plus the module's tools, since a test may run `go tool wails3` and
# a whole-tree contract test (TestThreadUpdatedFramesComeFromTheAnnouncer)
# lists and compiles every package whichever packages are named. The CI
# toolchain image carries that set rather than the whole module graph, and
# can lag go.mod, so a bare `go mod download` here would reach the proxy
# from every job and a narrower prefetch would strand a module in the
# namespace.
# Narrow a run with GO_TEST_PKGS and GO_TEST_FLAGS:
#   make go-test GO_TEST_PKGS=./internal/shellenv GO_TEST_FLAGS='-run TestSync -count=1'
NETNS := $(CURDIR)/bin/ao-netns
go-prefetch = go list -e -deps -test $(1) tool > /dev/null
GO_TEST_PKGS ?= $(GO_PACKAGE_ROOTS)
GO_TEST_FLAGS ?=
# GO_TEST_TAGS are build tags for every Go command go-test starts: the
# package list, the prefetch and the tests.
GO_TEST_TAGS ?=

include build/go-build-tool.mk

netns-tool:
	@$(call go-build-tool,bin/ao-netns,./cmd/ao-netns)

# gocache-trim caps the Go build cache (cmd/ao-gocache-trim; off in CI).
# go-build, go-test and test-race run a pass before their build or test and
# another when it ends, whatever its status (gocache-trim-around). There a
# failed pass prints its error and the target goes on, since a cache
# problem says nothing about the build or the tests, while an invalid
# AO_GOCACHE_MAX_GIB fails the target, since it leaves the cache uncapped.
# `make gocache-trim` fails on either.
GOCACHE_TRIM := $(CURDIR)/bin/ao-gocache-trim
gocache-trim-tool:
	@[ -n "$$CI" ] || { $(call go-build-tool,bin/ao-gocache-trim,./cmd/ao-gocache-trim); }

gocache-trim: gocache-trim-tool
	@[ -n "$$CI" ] || $(GOCACHE_TRIM)

# gocache-trim-around opens a recipe's shell: it runs a pass now and traps
# the shell's exit to run one after, keeping the recipe's exit status
# unless that pass fails.
gocache-trim-pass = { [ -n "$$CI" ] || $(GOCACHE_TRIM) -warn; }
gocache-trim-around = $(gocache-trim-pass); trap 'status=$$?; $(gocache-trim-pass) || status=$$?; exit $$status' EXIT

# GO_TEST_EXCLUDE is an extended regexp over import paths to leave out,
# for CI shards that run the heavy packages on their own runners.
# The timeout is 30m rather than go's 10m: at nice 19 on a loaded host
# ./internal/app alone runs past 10 minutes. A -timeout in GO_TEST_FLAGS
# replaces it.
GO_TEST_EXCLUDE ?=
# The targets that run cached tests turn VCS stamping off for every Go
# command they start, the tests' own included. With it on, `go list` and
# `go build` of a main package run `git status`, which refreshes the index
# through .git/index.lock and so changes .git's entry in the checkout root's
# listing; Go keys a cached test result by that listing, so every test that
# lists the root would rerun after any other package's run. Test binaries
# carry no VCS stamp either way.
go-test-tags = $(if $(GO_TEST_TAGS),-tags $(GO_TEST_TAGS))
go-test go-test-noremote test-race: override GOFLAGS := $(filter-out -buildvcs=false,$(GOFLAGS)) -buildvcs=false
go-test: netns-tool gocache-trim-tool
	@set -e; $(gocache-trim-around); \
	packages=$$(go list $(go-test-tags) $(GO_TEST_PKGS)); \
	if [ -n "$(GO_TEST_EXCLUDE)" ]; then packages=$$(printf '%s\n' $$packages | grep -vE '$(GO_TEST_EXCLUDE)' || true); fi; \
	if [ -z "$$packages" ]; then echo "ERROR: no Go packages found"; exit 1; fi; \
	$(call go-prefetch,$(go-test-tags) $(GO_PACKAGE_ROOTS)); \
	$(NETNS) go test -timeout 30m $(go-test-tags) $(GO_TEST_FLAGS) $$packages

# go-test-noremote runs, compiled with -tags noremote
# (docs/architecture/noremote-build.md), every package whose test binary the
# tag changes: one that links a package whose files differ under the tag,
# that package included. GO_FILE_SET is every file a build constraint
# selects, embeds included; a package's imports change only with those
# files, so any other test binary is the one `make go-test` already ran.
# Each go list is its own assignment, so set -e stops on its failure.
# GO_TEST_FLAGS adds flags as for go-test, but not tags: this target owns
# them, and a caller's tags would replace noremote.
GO_FILE_SET := {{.ImportPath}} {{.GoFiles}} {{.CgoFiles}} {{.CFiles}} {{.CXXFiles}} {{.HFiles}} {{.SFiles}} {{.SysoFiles}} {{.EmbedFiles}} {{.TestGoFiles}} {{.XTestGoFiles}} {{.TestEmbedFiles}} {{.XTestEmbedFiles}}
GO_TEST_MAINS := {{if eq .Name "main"}}{{.ImportPath}}{{range .Deps}}|{{.}}{{end}}{{"\n"}}{{end}}
go-test-noremote:
	@set -e; \
	case ' $(GO_TEST_FLAGS) ' in *' -tags'*|*' --tags'*) echo "ERROR: go-test-noremote sets the tags itself and cannot add others (GO_TEST_FLAGS=$(GO_TEST_FLAGS))"; exit 2;; esac; \
	if [ -n "$(GO_TEST_TAGS)" ]; then echo "ERROR: go-test-noremote sets the tags itself and cannot add others (GO_TEST_TAGS=$(GO_TEST_TAGS))"; exit 2; fi; \
	plain=$$(go list -f '$(GO_FILE_SET)' $(GO_PACKAGE_ROOTS)); \
	tagged=$$(go list -tags noremote -f '$(GO_FILE_SET)' $(GO_PACKAGE_ROOTS)); \
	mains=$$(go list -tags noremote -test -f '$(GO_TEST_MAINS)' $(GO_PACKAGE_ROOTS)); \
	differ=$$(printf '%s\n%s\n' "$$plain" "$$tagged" | sort | uniq -u | cut -d' ' -f1 | sort -u); \
	packages=$$(printf '%s\n' "$$mains" | awk -F'|' -v differ="$$differ" ' \
		BEGIN { n = split(differ, list, "\n"); for (i = 1; i <= n; i++) changed[list[i]] = 1 } \
		$$1 ~ /^agent-overflow(\/.*)?\.test$$/ { \
			pkg = substr($$1, 1, length($$1) - 5); hit = (pkg in changed); \
			for (i = 2; i <= NF && !hit; i++) { dep = $$i; sub(/ \[.*$$/, "", dep); hit = (dep in changed) } \
			if (hit) print pkg }' | sort -u); \
	if [ -z "$$packages" ]; then echo "ERROR: no test binary changes under -tags noremote"; exit 1; fi; \
	echo "go-test-noremote: $$(echo "$$packages" | wc -l) packages:" $$packages; \
	$(MAKE) go-test GO_TEST_PKGS="$$(echo $$packages)" GO_TEST_TAGS=noremote

# Static checks. Each is one CI job (.github/workflows/ci.yml) and runs the
# same way here. go-lint runs golangci-lint at the pinned version, installed
# under bin/ with this repo's Go toolchain (a release binary built with an
# older Go refuses a newer go.mod). `go install` runs every time and does
# nothing when the binary is current. It is a host executable rather than
# `go run` because the Windows pass below sets GOOS for the packages it
# reads, which `go run` would apply to the linter's own build.
# GOLANGCI_LINT names a prebuilt binary instead, which the CI toolchain
# image carries at the same pin. GO_LINT_FLAGS carries extra flags, such as
# --new-from-patch for the new-issues gate.
GOLANGCI_LINT_VERSION := v2.14.0
GOLANGCI_LINT_BUILT := bin/golangci-lint-$(GOLANGCI_LINT_VERSION)/golangci-lint
GOLANGCI_LINT ?= $(GOLANGCI_LINT_BUILT)
GOLANGCI_LINT_BUILD := $(filter $(GOLANGCI_LINT_BUILT),$(GOLANGCI_LINT))
.PHONY: $(GOLANGCI_LINT_BUILT)
$(GOLANGCI_LINT_BUILT):
	GOBIN=$(abspath $(dir $@)) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
GO_LINT_FLAGS ?=
go-lint: $(GOLANGCI_LINT_BUILD)
	$(GOLANGCI_LINT) run --timeout 20m $(GO_LINT_FLAGS) ./...

# go-lint-whole-tree runs the two linters that have no backlog over the
# whole tree, not only a change's lines: exhaustive reports at the switch,
# not at the constant a change added, and unused at the identifier whose
# last use a change deleted, whose own lines are not in the change. One
# process runs both over the untagged file set, test files included; the
# other passes run exhaustive alone.
#
# exhaustive reads every Go file this host compiles and the Windows
# launcher's packages as Windows compiles them. Two passes reach every file
# here: one with no build tag and one with every tag that selects files,
# because no file needs one of those tags on and another off.
# TestExhaustiveLintPassesReachEveryFile reads the passes from this recipe
# and fails for a file none of them compiles. Every file only macOS
# compiles is in the untagged set, which CI's macOS job lints with `make
# go-lint-host-files`.
#
# unused reads one file set, the untagged Linux one: a helper that only
# another platform or a build tag calls lives in that platform's or tag's
# files, beside its callers.
GO_LINT_WHOLE_TREE = $(GOLANGCI_LINT) run --timeout 20m
go-lint-whole-tree: $(GOLANGCI_LINT_BUILD)
	$(GO_LINT_WHOLE_TREE) --enable-only exhaustive,unused ./...
	$(GO_LINT_WHOLE_TREE) --enable-only exhaustive --build-tags nogui,noremote,providersmoke,linearlive ./...
	@set -e; for tags in "" noremote; do \
		packages=$$(GOOS=windows CGO_ENABLED=0 go list -deps -tags "$$tags" ./cmd/agent-overflow-windows | sed -n 's|^agent-overflow/|./|p'); \
		if [ -z "$$packages" ]; then echo "ERROR: no packages found for the Windows launcher"; exit 1; fi; \
		echo "exhaustive: the Windows launcher's packages, tags '$$tags'"; \
		GOOS=windows CGO_ENABLED=0 $(GO_LINT_WHOLE_TREE) --enable-only exhaustive --build-tags "$$tags" $$packages; \
	done

# go-lint-host-files runs exhaustive over the files this host compiles and
# Linux does not, which go-lint-whole-tree on Linux cannot read: CI's macOS
# job runs it. It lints every package this host compiles whose file set
# (GO_FILE_SET) differs from Linux's, so a package gains or loses the pass
# with its files and the other packages are read as compiled dependencies,
# not type-checked from source. A switch outside those packages over an enum
# that gains a constant in a file only this host compiles is not read.
GO_HAS_FILES := {{if or .GoFiles .CgoFiles .TestGoFiles .XTestGoFiles}}$(GO_FILE_SET){{end}}
go-lint-host-files: $(GOLANGCI_LINT_BUILD)
	@set -e; \
	host=$$(go list -f '$(GO_HAS_FILES)' $(GO_PACKAGE_ROOTS)); \
	linux=$$(GOOS=linux CGO_ENABLED=1 go list -f '$(GO_FILE_SET)' $(GO_PACKAGE_ROOTS)); \
	packages=$$({ printf '%s\n' "$$linux" | sed 's/^/linux /'; printf '%s\n' "$$host" | sed 's/^/host /'; } | awk ' \
		$$1 == "linux" { sub(/^linux /, ""); onLinux[$$0] = 1; next } \
		{ sub(/^host /, ""); if ($$0 != "" && !($$0 in onLinux)) print $$1 }' | \
		sort -u | sed -e 's|^agent-overflow$$|.|' -e 's|^agent-overflow/|./|'); \
	if [ -z "$$packages" ]; then echo "ERROR: no package's files differ between this host and Linux"; exit 1; fi; \
	echo "go-lint-host-files: $$(echo "$$packages" | wc -l) packages:" $$packages; \
	$(GO_LINT_WHOLE_TREE) --enable-only exhaustive $$packages

go-vet:
	go vet $(GO_PACKAGE_ROOTS)
	go vet -tags nogui $(GO_PACKAGE_ROOTS)
	go vet -tags noremote $(GO_PACKAGE_ROOTS)

# gofmt -l prints nothing when every tracked Go file is formatted.
fmt-check:
	@unformatted=$$(git ls-files -- '*.go' | while IFS= read -r f; do [ -f "$$f" ] && printf '%s\n' "$$f"; done | xargs gofmt -l); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi

# go.mod and go.sum must already be tidy. Development builds run `go mod
# tidy` through the Taskfile; release builds skip it, so this is the check.
tidy-check:
	go mod tidy -diff

# govulncheck reports known vulnerabilities reachable from this module.
GOVULNCHECK_VERSION := latest
go-vuln:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

# bindings-check regenerates the Wails bindings and the methodgen tables the
# way the release build does and fails when the committed copies differ.
# Generation writes into the tree, so run it on a clean checkout or after
# committing: an unrelated local edit to the generated files fails it.
bindings-check:
	wails3 generate bindings -f '-tags production' -clean=true -ts
	$(MAKE) methodgen
	git diff --exit-code --stat -- frontend/bindings internal/transport/methods_gen.go frontend/src/lib/transport/methodRoutes.ts

# The frontend build follows the lockfile. pnpm records the lockfile it
# installed at node_modules/.pnpm/lock.yaml; when the checked-in lockfile is
# newer (a merge, a branch switch), every target that runs the frontend
# reinstalls first, and a current install costs one stat. `make install`
# stays the first-time setup because it also fetches the Go tools and the
# Playwright Chromium.
FRONTEND_DEPS := frontend/node_modules/.pnpm/lock.yaml
$(FRONTEND_DEPS): frontend/pnpm-lock.yaml
	cd frontend && pnpm install --frozen-lockfile

# frontend-lint runs oxlint over the frontend (frontend/.oxlintrc.json).
# OXLINT_FLAGS carries extra flags; CI lints only the files a change touched
# with --deny-warnings until the warning backlog is cleared.
OXLINT_FLAGS ?=
frontend-lint: $(FRONTEND_DEPS)
	cd frontend && pnpm exec oxlint $(OXLINT_FLAGS)

# frontend-deadcode runs knip over the frontend, counting the Playwright
# suite's imports (knip.config.ts): a file, export, type or dependency
# nothing uses fails it.
frontend-deadcode: $(FRONTEND_DEPS)
	cd frontend && pnpm run deadcode

# test-race exercises the concurrency-sensitive packages under -race.
# Scoped to packages with non-trivial goroutine wiring rather than the
# full repo because -race is slow; the listed packages are the ones
# whose teardown / rebind / event-bus paths are most likely to surface
# data-race regressions. Triage is included for its stream-persist
# timer flushes, which run off the provider read loop and serialize
# against replace/settle writes via streamFlushMu. Provider is included
# because every session runs a read-loop goroutine mutating state that
# binding goroutines read (sessionID was a real lock-free race caught
# only by review, 2026-08-24) plus five mutexes and per-process pipe
# pumps on the Codex side. Browser is included because page operations,
# popup adoption, idle shutdown, settings reconfigure, and session teardown
# converge on one lazily started Chrome process. `internal/app` carries the
# App integration tests (transport boot, multi-session shutdown, event fan-out)
# after root decomposition; root remains for executable-bootstrap contracts.
# Those are the surfaces most likely to hide a real-world race. The
# timeout is per test binary, and every listed package runs in ONE go
# test invocation, so the big suites compete for CPU. Since the
# storetest template-DB conversion (29ec9041 triage, ed21d0fc root —
# one migration replay per package instead of one per test) the two
# heavy legs measure ~108s (triage) and ~270s (root) under -race on an
# idle WSL host (2026-08-25); before it, root alone hit 1800s. The
# timeout stays 1800s for deadlock protection on loaded hosts —
# tighten only if you've measured headroom.
RACE_PKGS := ./internal/transport/... ./internal/triage/... ./internal/provider/... ./internal/wsllauncher/... ./internal/clientmode/... ./internal/editor/... ./internal/browser/... ./internal/app/... .
test-race: netns-tool gocache-trim-tool
	@set -e; $(gocache-trim-around); \
	$(call go-prefetch,$(GO_PACKAGE_ROOTS)); \
	$(NETNS) go test -race -timeout 1800s $(RACE_PKGS)

# provider-smoke is the real-provider gate: it drives one trivial workflow
# through the REAL `claude` and `codex` binaries (default PATH resolution — no
# binary-path override) and asserts schema acceptance, envelope round-trip, and
# the §9 worktree/branch rules. A Claude-only scenario additionally builds a
# real multi-branch transcript and proves the CLI resumes a fork cut by the
# session importer's lazy branch materialisation, and another proves the CLI's
# queue-boundary merge of two separately flushed messages is folded and
# revertable. It SPENDS REAL MODEL TOKENS: one trivial turn per provider, four
# for the imported-branch scenario, three Haiku turns for the merge scenario,
# and four answered turns plus one early interrupt per provider for the revert
# flow, four for the Claude worktree follow scenario, five per provider for
# the external worktree removal scenario (one cut mid-command), and three
# Haiku turns for the forked skill scenario (two stopped mid-fork).
# Both CLIs must be installed and authenticated.
#
# The `providersmoke` build tag keeps these tests out of `make go-test`.
# `make verify` compiles them without running any test so production API changes
# cannot leave the manual gate broken; that compile-only check stays hermetic.
# Run the real gate manually before a release and after upgrading either
# provider CLI; the mocked suites accept any structured-output schema, so
# nothing else in the repo can catch a schema the real CLIs reject. See
# internal/app/providersmoke_test.go.
#
# -timeout covers the sum of the in-test deadlines (6m per workflow leg, 3m for
# the imported-branch scenario, 6m for the merge scenario, 6m per revert leg,
# 3m per queue leg, 8m per fork leg, 5m for the worktree follow scenario,
# 6m per external-removal leg, 8m for the forked skill scenario, plus auth
# probes) with headroom, so a wedged turn fails through the gate's own
# diagnostics rather than as a bare test-binary timeout panic.
provider-smoke-compile:
	go test -tags providersmoke -run '^$$' ./internal/app

provider-smoke:
	go test -tags providersmoke -run 'TestProviderSmoke' -v -count=1 -timeout 90m ./internal/app

# Focused real-provider rollback/resume gate; also included in provider-smoke.
provider-smoke-revert:
	go test -tags providersmoke -run '^TestProviderSmokeRevertFlows$$' -v -count=1 -timeout 15m ./internal/app

# Two supplied production artifacts, disposable state, mocked providers.
.PHONY: service-artifact-smoke
service-artifact-smoke: netns-tool
	@test -n "$(AO_SERVICE_SMOKE_BASELINE)" -a -n "$(AO_SERVICE_SMOKE_CANDIDATE)" || \
		{ echo 'Set AO_SERVICE_SMOKE_BASELINE and AO_SERVICE_SMOKE_CANDIDATE to absolute artifact paths.'; exit 1; }
	$(call go-prefetch,./internal/supervise)
	AO_SERVICE_SMOKE_BASELINE="$(AO_SERVICE_SMOKE_BASELINE)" AO_SERVICE_SMOKE_CANDIDATE="$(AO_SERVICE_SMOKE_CANDIDATE)" \
		$(NETNS) go test ./internal/supervise -run TestProductionServiceArtifact -v -count=1 -timeout 4m

# import-corpus-smoke is the manual session-import gate: it runs the Claude
# transcript reader, the Codex rollout reader, and the store writer over a COPY
# of a developer's real provider homes and reports what the corpus contains
# (warnings by code, unknown wire types, corrupt lines, wall time, peak heap).
# The committed importer tests use synthetic fixtures, which only know the
# shapes whoever wrote them knew about; this is what catches real-world format
# drift.
#
# It spends no tokens and spawns nothing — it reads each file, builds the
# imported rows, and applies them to a throwaway SQLite store — but it must
# never be pointed at the live homes: the gate FAILS when a corpus root overlaps
# ~/.claude or ~/.codex. Copy them first, and repoint the Codex thread index at
# the copy (its rollout_path column is absolute):
#
#   cp -a ~/.claude /tmp/claude-corpus
#   cp -a ~/.codex  /tmp/codex-corpus
#   sqlite3 /tmp/codex-corpus/state_5.sqlite \
#     "UPDATE threads SET rollout_path = replace(rollout_path, '$$HOME/.codex', '/tmp/codex-corpus');"
#   make import-corpus-smoke \
#     AO_IMPORT_CORPUS_CLAUDE=/tmp/claude-corpus AO_IMPORT_CORPUS_CODEX=/tmp/codex-corpus
#
# Unlike provider-smoke there is no build tag: both legs are compiled into
# `make go-test` and SKIP when their variable is unset, which is what keeps the
# runner's own logic covered (importcorpussmoke_fixture_test.go) without ever
# reading a provider home. Either variable may be set on its own. See
# internal/app/importcorpussmoke_test.go.
import-corpus-smoke: netns-tool
	$(call go-prefetch,./internal/app)
	AO_IMPORT_CORPUS_CLAUDE="$(AO_IMPORT_CORPUS_CLAUDE)" AO_IMPORT_CORPUS_CODEX="$(AO_IMPORT_CORPUS_CODEX)" \
		$(NETNS) go test -run 'TestImportCorpusSmoke' -v -count=1 -timeout 20m ./internal/app

# playwright install is idempotent and cached (~/.cache/ms-playwright);
# the Chromium binary backs the frontend browser test project
# (`pnpm test:browser`), which is part of `make test` and `make verify`.
install:
	go install tool
	cd frontend && pnpm install --frozen-lockfile
	cd frontend && pnpm exec playwright install chromium

# dev: one build of the dev bundle (frontend embedded, DEV=true), then one
# launch. Nothing watches the tree: a code change needs another `make dev`,
# the same as `make dev-wsl`. A supervisor that rebuilt and relaunched on
# every save made the app unusable while editing.
dev: $(FRONTEND_DEPS)
	VITE_AGENT_OVERFLOW_UI_TRACE=$(UI_TRACE) VITE_AGENT_OVERFLOW_UI_ORACLES=$(UI_ORACLES) wails3 build DEV=true
	AGENT_OVERFLOW_DEBUG=$(AGENT_OVERFLOW_DEBUG) AGENT_OVERFLOW_PPROF=$(AGENT_OVERFLOW_PPROF) wails3 task run

# dev-wsl: cross-compiles the Linux ELF + Windows .exe launcher inside
# this WSL distro, copies the .exe to a versioned Windows-native path,
# and invokes it via Windows's loader. The --distro flag is set from
# $WSL_DISTRO_NAME so the launcher skips the picker and runs the
# backend in the same distro you're shelled into. The override is
# non-persistent — it doesn't overwrite a user-saved choice in
# %APPDATA%\agent-overflow\wsl.json.
#
# Why the staging copy and not just `./bin/agent-overflow.exe`:
# Launching the .exe directly off the WSL filesystem makes Windows
# load it through the SMB-redirector / 9P bridge at \\wsl.localhost.
# That layer caches process images, so a freshly-built .exe gets
# loaded from a stale cached image — strings on disk show the new
# code, but the running process behaves like the old one. Symptom
# we hit: connectivity-error page from a probeBootstrap variant we'd
# already removed in source. Copying to %LOCALAPPDATA% (a true
# NTFS path on the C: drive) bypasses the redirector and Windows
# loads the actual current bytes. Each dev launch uses a versioned
# filename because Windows keeps the running .exe locked; overwriting a
# fixed staging path fails with "Permission denied" and would otherwise
# relaunch the old image. forge avoids this by installing into Program
# Files; we do the dev-mode equivalent.
#
# %LOCALAPPDATA% is resolved at recipe-execution time via cmd.exe
# interop, not from $$LOCALAPPDATA in our Linux shell. WSLENV in
# this repo's typical setup doesn't propagate Windows env vars,
# so $$LOCALAPPDATA is empty here even though the Windows host has
# it. The cmd.exe call always works inside an interop-capable shell.
#
# dev-wsl also stamps a unique payload version so the Windows launcher
# reinstalls ~/.local/bin/agent-overflow inside WSL on every dev run.
# The normal package path may use a stable VERSION; dev mode cannot,
# or the launcher will correctly skip reinstalling what it thinks is
# the same embedded payload.
#
# Debug env has two WSLENV hops in this target:
#   1. WSL shell -> Windows launcher (.exe launched through interop).
#   2. Windows launcher -> WSL backend (wsl.exe child).
# The launcher already handles hop 2 (its own vars directly, the
# diagnostic set via internal/diagenv Passthrough), but hop 1 must be
# whitelisted here or Windows never receives the vars in the first
# place. DEV_WSL_FWD_VARS is that hop-1 list:
# AGENT_OVERFLOW_WEBVIEW_LOG (opt-in chrome_debug.log; spawns a console
# window — closing it kills the app, WebView2Feedback #3192) and
# AGENT_OVERFLOW_WEBVIEW_SOFTWARE (opt-in --disable-gpu software
# rendering, desktop-stutter diagnostics) only need hop 1: they are
# consumed by the Windows launcher itself. AGENT_OVERFLOW_PPROF,
# AGENT_OVERFLOW_RENDERER_DIAG and AO_HARNESS_REAL_BROWSER
# (internal/diagenv) ride both hops to reach the WSL backend — the last
# one is what makes `AO_HARNESS_REAL_BROWSER=1 make harness-wsl` the
# Windows leg of the manual real-browser-engine gate
# (docs/specs/embedded-browser.md §10). It is inert on every other
# profile: the backend refuses it whenever the soak autopilot is armed,
# and no non-isolated boot reads it at all.
DEV_WSL_FWD_VARS := AGENT_OVERFLOW_DEBUG AGENT_OVERFLOW_WEBVIEW_LOG AGENT_OVERFLOW_WEBVIEW_SOFTWARE AGENT_OVERFLOW_WEBVIEW_EXTRA_ARGS AGENT_OVERFLOW_PPROF AGENT_OVERFLOW_RENDERER_DIAG AO_HARNESS_REAL_BROWSER

# LAUNCH_PROFILE selects which instance launch-wsl builds and runs:
# empty is the developer's normal dev instance, `harness` is the isolated
# mocked instance you drive (make harness-wsl), `soak` is that same
# instance with the soak autopilot armed (make soak,
# docs/architecture/soak-rig.md), and `perf` is a third isolated harness
# reserved for destructive renderer benchmarks. It is forwarded verbatim to the
# launcher's --profile flag, which is THE axis behind every piece of
# per-instance state (single-instance id, window title, WebView2 profile,
# CDP port, launcher log, window placement, backend data dir).
LAUNCH_PROFILE ?=
LAUNCH_WSL_BUILD_MODE ?= build:dev

dev-wsl:
	@$(MAKE) launch-wsl LAUNCH_PROFILE= UI_TRACE=$(UI_TRACE) UI_ORACLES=$(UI_ORACLES)

# harness-wsl: the Windows harness — a SECOND, fully isolated instance of
# the real Windows app (own launcher window, WebView2 profile, log, data
# dir) running against ao-mockprovider, which you then drive: bin/ao-harness,
# Playwright, or by hand at the window. Same build path as dev-wsl;
# everything that could collide with the developer's own instance is
# switched by --profile harness, and the backend can only ever see
# ao-mockprovider and its own ~/.agent-overflow-harness data dir.
harness-wsl:
	@$(MAKE) launch-wsl LAUNCH_PROFILE=harness UI_TRACE=$(UI_TRACE) UI_ORACLES=$(UI_ORACLES)

# perf-wsl: a THIRD isolated Windows harness for destructive renderer A/B
# runs. It deliberately passes the production `build` mode, so the embedded
# assets measured by this profile are the assets shipped to users. Its data
# root, WebView2 profile, launcher identity, window state, log,
# forensics, and CDP endpoint differ from dev, harness, and soak. Benchmark
# reset/interrupt/reload commands can therefore target it without touching a
# long-running rig or the developer's real app.
perf-wsl:
	@$(MAKE) launch-wsl LAUNCH_PROFILE=perf LAUNCH_WSL_BUILD_MODE=build UI_TRACE=$(UI_TRACE) UI_ORACLES=$(UI_ORACLES)

# soak: the same isolated Windows instance with ONE preset armed — the
# soak autopilot, which seeds two threads and streams background-subagent
# activity forever, so it can sit visible and untouched for hours
# reproducing the WebView2 renderer-hang steady state
# (docs/architecture/soak-rig.md). Its data dir is ~/.agent-overflow-soak,
# separate from the harness profile's. Check on it with `make soak-check`.
soak:
	@$(MAKE) launch-wsl LAUNCH_PROFILE=soak UI_TRACE=$(UI_TRACE) UI_ORACLES=$(UI_ORACLES)

# launch-wsl bakes the launch profile into the exe NAME
# (agent-overflow-harness-<ts>-<pid>.exe vs agent-overflow-dev-...), not
# just into argv. Process listings truncate argv but always show the
# image name; identical names across profiles made a "kill the harness"
# image-name match kill the developer's main instance (incident
# 2026-08-30). Keep the profile in the name.
launch-wsl:
	@if [ -z "$$WSL_DISTRO_NAME" ]; then \
		echo "ERROR: WSL_DISTRO_NAME is unset. Run this target from inside a WSL shell."; \
		exit 1; \
	fi
	@case "$(LAUNCH_PROFILE)" in ""|harness|soak|perf) ;; *) echo "ERROR: LAUNCH_PROFILE must be empty, 'harness', 'soak', or 'perf', got '$(LAUNCH_PROFILE)'" >&2; exit 1;; esac
	@set -e; \
	PROFILE_TAG="$(LAUNCH_PROFILE)"; [ -n "$$PROFILE_TAG" ] || PROFILE_TAG=dev; \
	DEV_VERSION=$$PROFILE_TAG-$$(date +%Y%m%d%H%M%S)-$$$$; \
	$(MAKE) build-wsl WSL_VERSION=$$DEV_VERSION WSL_FORCE_RELINK=1 UI_TRACE=$(UI_TRACE) UI_ORACLES=$(UI_ORACLES) WSL_BUILD_MODE=$(LAUNCH_WSL_BUILD_MODE); \
	if [ -n "$(LAUNCH_PROFILE)" ]; then \
		$(MAKE) mockprovider mockforge; \
		PROFILE_BIN="$$HOME/.local/share/agent-overflow/$(LAUNCH_PROFILE)/bin"; \
		mkdir -p "$$PROFILE_BIN"; \
		for MOCK in ao-mockprovider ao-mockforge; do \
			cp "bin/$$MOCK" "$$PROFILE_BIN/$$MOCK.tmp.$$$$"; \
			mv -f "$$PROFILE_BIN/$$MOCK.tmp.$$$$" "$$PROFILE_BIN/$$MOCK"; \
			echo "Installed $$MOCK at $$PROFILE_BIN/$$MOCK"; \
		done; \
	fi; \
	WIN_LAD=$$(/mnt/c/Windows/System32/cmd.exe /c 'echo %LOCALAPPDATA%' 2>/dev/null | tr -d '\r\n'); \
	if [ -z "$$WIN_LAD" ]; then \
		echo "ERROR: could not resolve %LOCALAPPDATA% via cmd.exe interop."; \
		exit 1; \
	fi; \
	WIN_DEV_DIR_LINUX=$$(wslpath -u "$$WIN_LAD")/agent-overflow/dev; \
	WIN_DEV_EXE_LINUX="$$WIN_DEV_DIR_LINUX/agent-overflow-$$DEV_VERSION.exe"; \
	mkdir -p "$$WIN_DEV_DIR_LINUX"; \
	find "$$WIN_DEV_DIR_LINUX" -maxdepth 1 -name "agent-overflow-$$PROFILE_TAG-*.exe" ! -name "agent-overflow-$$DEV_VERSION.exe" -delete 2>/dev/null || true; \
	cp bin/agent-overflow.exe "$$WIN_DEV_EXE_LINUX"; \
	PROFILE_ARGS=""; \
	if [ -n "$(LAUNCH_PROFILE)" ]; then \
		PROFILE_ARGS="--profile $(LAUNCH_PROFILE)"; \
	fi; \
	if [ "$(LAUNCH_PROFILE)" = "soak" ]; then \
		SOAK_LOG=$$(scripts/soak-check.sh --print-log-path); \
		echo ""; \
		echo "Soak launcher log: $$SOAK_LOG"; \
		echo "Watchdog one-liner: grep -nE 'render(er ran no script| recovery)|rebuilding controller' \"$$SOAK_LOG\""; \
		echo "Summary:            make soak-check"; \
		echo ""; \
	elif [ "$(LAUNCH_PROFILE)" = "harness" ]; then \
		echo ""; \
		echo "Windows harness data root: $$HOME/.agent-overflow-harness"; \
		echo "Drive it:                  bin/ao-harness list | info | health   (build: make harness-build)"; \
		echo "Stop it:                   bin/ao-harness down   (closes the launcher window too)"; \
		echo ""; \
	elif [ "$(LAUNCH_PROFILE)" = "perf" ]; then \
		echo ""; \
		echo "Windows perf data root: $$HOME/.agent-overflow-perf"; \
		echo "CDP:                   127.0.0.1:9226"; \
		echo "Drive it explicitly:   bin/ao-harness --instance $$HOME/.agent-overflow-perf ..."; \
		echo ""; \
	fi; \
	echo "Launching $$WIN_DEV_EXE_LINUX --distro $$WSL_DISTRO_NAME $$PROFILE_ARGS"; \
	FWD_WSLENV="$$WSLENV"; \
	for spec in $(foreach v,$(DEV_WSL_FWD_VARS),"$(v)=$($(v))"); do \
		name=$${spec%%=*}; \
		[ -n "$${spec#*=}" ] || continue; \
		case ":$$FWD_WSLENV:" in \
			*:$$name:*) ;; \
			*) FWD_WSLENV="$$name$${FWD_WSLENV:+:$$FWD_WSLENV}" ;; \
		esac; \
	done; \
	WSLENV="$$FWD_WSLENV" $(foreach v,$(DEV_WSL_FWD_VARS),$(v)="$($(v))") "$$WIN_DEV_EXE_LINUX" --distro "$$WSL_DISTRO_NAME" $$PROFILE_ARGS

# soak-check: read-only summary of the running (or last) soak — uptime,
# render-recovery episodes, controller rebuilds. Safe to run at any time;
# it only reads launcher-soak.log.
soak-check:
	@scripts/soak-check.sh

# Probe the actual WebView2 scroll and compositor contracts without changing
# persisted state. Requires a running isolated soak instance.
# Requires a running `make soak` instance on its isolated CDP port.
soak-contract:
	@AO_CDP_PORT=9224 scripts/perfprobe/probe scroll-contract
	@AO_CDP_PORT=9224 scripts/perfprobe/probe compositor-contract

# build-wsl: cross-compiles the Linux ELF backend + Windows .exe launcher
# without running. Use this when you want to hand the .exe off (e.g.
# copy to the Windows desktop, double-click later) instead of launching
# in place.
build-wsl: $(FRONTEND_DEPS)
	@case "$(WSL_BUILD_MODE)" in build|build:dev) ;; *) echo "ERROR: WSL_BUILD_MODE must be 'build' or 'build:dev', got '$(WSL_BUILD_MODE)'" >&2; exit 1;; esac
	@if [ -n "$(WSL_FORCE_RELINK)" ]; then rm -f bin/agent-overflow.exe bin/agent-overflow-linux; fi
	VITE_AGENT_OVERFLOW_UI_TRACE=$(UI_TRACE) VITE_AGENT_OVERFLOW_UI_ORACLES=$(UI_ORACLES) DEV="$$(case "$(WSL_BUILD_MODE)" in build:dev) echo true ;; *) echo false ;; esac)" WSL_LAUNCHER_MODE="$$(case "$(WSL_BUILD_MODE)" in build:dev) echo dev ;; *) echo prod ;; esac)" VERSION="$(WSL_VERSION)" WSL_GO_TAGS="$(WSL_GO_TAGS)" WSL_LDFLAGS="$(WSL_LDFLAGS)" wails3 task windows:build:wsl

ifeq ($(shell uname -s),Darwin)
build: $(FRONTEND_DEPS)
	VERSION="$(VERSION)" wails3 task darwin:package
else
build: $(FRONTEND_DEPS)
	VERSION="$(VERSION)" wails3 build
endif

# ---- Agent test harness (docs/architecture/agent-harness.md) ----
#
# HARNESS_DATA_DIR defaults to a stable per-checkout borrowed root so a
# re-run reuses seeded state; pass HARNESS_DATA_DIR=$(mktemp -d) for a
# fresh throwaway. Managed `ao-harness run --plan` runs own fresh roots.
# `make harness` prints the __AO_HARNESS__ bootstrap line
# (URL + token + paths) on stdout — everything an agent or Playwright
# MCP session needs to attach. The checkout-path suffix keeps two
# worktrees from sharing a DB / generated workspaces (each boot rewrites
# the provider settings to its own mock binary — a shared dir would
# point one harness at the other's).
# Mirror os.TempDir(): $TMPDIR (sans trailing slash) if set, else /tmp —
# instanceinfo.DataRootFor must resolve the same root or `make harness`
# and `ao-harness list` disagree about which instance a worktree has.
HARNESS_TMPDIR := $(if $(TMPDIR),$(patsubst %/,%,$(TMPDIR)),/tmp)
HARNESS_DATA_DIR ?= $(HARNESS_TMPDIR)/agent-overflow-harness$(subst /,-,$(CURDIR))

mockprovider:
	go build -o bin/ao-mockprovider ./cmd/ao-mockprovider

# mockforge is the fake gh and glab an isolated boot runs in place of the
# real CLIs (cmd/ao-mockforge, internal/harness/forgefake).
mockforge:
	go build -o bin/ao-mockforge ./cmd/ao-mockforge

# harness-build produces bin/agent-overflow with the production SPA
# embedded, plus the sibling ao-mockprovider and ao-mockforge the harness
# resolves by default. That same bin/agent-overflow carries the remaining CLI
# verbs (D30) and the workflow tools (D77); there is no second binary to
# build. UI_TRACE=1
# bakes the render-trace instrumentation into the SPA (see the flag docs at
# the top of this file).
#
# bin/ao-harness rides along: it is the shell/agent driver for a running
# instance (docs/specs/testing-harness.md §3, cmd/ao-harness/AGENTS.md),
# and it resolves the backend binary as its own sibling, so building the
# two together is what makes `bin/ao-harness up` need no configuration.
#
# The packages whose test binaries the suite runs, built as
# bin/ao-<name>-test, and the browsers each run installs are declared in
# build/test-inputs.mk.
include build/test-inputs.mk
harness-build: mockprovider mockforge $(FRONTEND_DEPS)
	cd frontend && VITE_AGENT_OVERFLOW_UI_TRACE=$(UI_TRACE) VITE_AGENT_OVERFLOW_UI_ORACLES=$(UI_ORACLES) pnpm run build
	go build -ldflags "-X main.version=$(VERSION)" -o bin/agent-overflow .
	go build -tags noremote -ldflags "-X main.version=$(VERSION)" -o bin/agent-overflow-noremote .
	go build -ldflags "-X main.version=$(VERSION)" -o bin/ao-harness ./cmd/ao-harness
	go build -o bin/ao-harness-e2e ./cmd/ao-harness-e2e
	$(foreach pkg,$(E2E_TEST_PACKAGES),go test -c -o bin/ao-$(notdir $(pkg))-test $(pkg) &&) true

harness: harness-build
	bin/agent-overflow --harness --data-dir "$(HARNESS_DATA_DIR)"

# harness-window / soak-window are the same two backends behind a REAL
# webview window instead of a headless port (docs/specs/testing-harness.md
# §1): isolated data root, mock providers, no single-instance
# registration, webview storage under the data root. Under WSLg the
# window lands on the Windows desktop, so this is live-testable from the
# dev environment. Both block; Ctrl-C or closing the window ends them.
ifeq ($(shell uname -s),Darwin)
harness-window: harness-build
	go build -o bin/ao-darwin-harness ./cmd/ao-darwin-harness
	bin/ao-darwin-harness --binary "$(CURDIR)/bin/agent-overflow" --data-root "$(HARNESS_DATA_DIR)" --plist "$(CURDIR)/build/darwin/Info.dev.plist" --driver "$(CURDIR)/bin/ao-harness" -- --harness --window
else
harness-window: harness-build
	@set -e; \
	trap 'bin/ao-harness down --instance "$(HARNESS_DATA_DIR)" >/dev/null 2>&1 || true' EXIT INT TERM; \
	bin/ao-harness up --window --data-dir "$(HARNESS_DATA_DIR)"; \
	while bin/ao-harness info --instance "$(HARNESS_DATA_DIR)" >/dev/null 2>&1; do sleep 1; done
endif

# soak-window IS the soak preset, so it spells --autopilot explicitly:
# --soak alone is only the launcher-shell isolated backend, and the flag
# semantics stay uniform (nothing implies the autopilot).
#
# The -soak suffix mirrors the binary's own windowed-soak default
# (instanceinfo.DefaultSoakDataRoot): the soak autopilot refuses a data dir
# holding threads it did not seed, so it cannot share the harness root.
ifeq ($(shell uname -s),Darwin)
soak-window: harness-build
	go build -o bin/ao-darwin-harness ./cmd/ao-darwin-harness
	bin/ao-darwin-harness --binary "$(CURDIR)/bin/agent-overflow" --data-root "$(HARNESS_DATA_DIR)-soak" --plist "$(CURDIR)/build/darwin/Info.plist" --driver "$(CURDIR)/bin/ao-harness" -- --soak --autopilot --window
else
soak-window: harness-build
	@set -e; \
	trap 'bin/ao-harness down --instance "$(HARNESS_DATA_DIR)-soak" >/dev/null 2>&1 || true' EXIT INT TERM; \
	bin/ao-harness up --soak --autopilot --window --data-dir "$(HARNESS_DATA_DIR)-soak"; \
	while bin/ao-harness info --instance "$(HARNESS_DATA_DIR)-soak" >/dev/null 2>&1; do sleep 1; done
endif

# e2e runs the Playwright harness suite (e2e/) against a fresh
# harness-build. Browser downloads are versioned and cached by Playwright.
# E2E_ARGS passes through to `playwright test`. CI builds once and runs
# bin/ao-harness-e2e --shard=N/8 in each shard.
E2E_ARGS ?=
e2e e2e-mobile-browser: harness-build
	cd e2e && pnpm install --frozen-lockfile
	$(MAKE) e2e-browsers
	bin/ao-harness-e2e $(E2E_ARGS) $(if $(filter e2e-mobile-browser,$@),tests/compact-browser-lock.spec.ts)

# e2e-browsers installs the suite's browsers (E2E_BROWSERS); a present
# build is skipped. CI's Playwright shards run it after the e2e
# dependencies.
e2e-browsers:
	scripts/playwright-browsers.sh E2E_BROWSERS

# release-upgrade-check: the candidate migrates the corpus the previous
# release published, boots on it, tops it up and archives it for the next
# release (docs/architecture/release-upgrade-check.md). A release gate, an
# operator tool; CI's upgrade-lineage job runs the script on the harness
# build's binaries. Every pull request runs the upgrade-corpus-coverage
# spec in the Playwright suite instead. PREVIOUS_RELEASE picks
# the release to upgrade from (default:
# the newest GitHub release); RELEASE_UPGRADE_BOOTSTRAP=1 seeds a synthetic
# corpus with that release's binary instead of its published corpus, at
# RELEASE_UPGRADE_SCALE (1 is about 2 GB, 0.02 a smoke slice);
# RELEASE_UPGRADE_ARCHIVE_OUT writes the corpus for the next release;
# RELEASE_UPGRADE_CORPUS runs an existing data root instead (an `ao-harness
# clone` of a real database, never committed or archived);
# RELEASE_UPGRADE_REPORT names the JSON report.
RELEASE_UPGRADE_SCALE ?= 1
release-upgrade-check: harness-build
	cd e2e && pnpm install --frozen-lockfile
	scripts/playwright-browsers.sh E2E_BROWSERS_MANUAL
	scripts/release-upgrade-check.sh --scale "$(RELEASE_UPGRADE_SCALE)" \
		$(if $(PREVIOUS_RELEASE),--previous "$(PREVIOUS_RELEASE)") \
		$(if $(RELEASE_UPGRADE_BOOTSTRAP),--bootstrap) \
		$(if $(RELEASE_UPGRADE_ARCHIVE_OUT),--archive-out "$(RELEASE_UPGRADE_ARCHIVE_OUT)") \
		$(if $(RELEASE_UPGRADE_CORPUS),--corpus "$(RELEASE_UPGRADE_CORPUS)") \
		$(if $(RELEASE_UPGRADE_REPORT),--report "$(RELEASE_UPGRADE_REPORT)")

# The Android shell (mobile/). Builds the ordinary production SPA,
# syncs it into the native project, and assembles the debug APK. See
# mobile/AGENTS.md for the toolchain this needs and what is deferred.
apk apk-release:
	cd mobile && pnpm install --frozen-lockfile
	mobile/scripts/build-apk.sh $(if $(filter apk-release,$@),release,debug)

# The compact Playwright project driven inside a running emulator's
# WebView, which is the only place the native seams are real. It is
# deliberately NOT a blocking gate: a laptop with no emulator prints how
# to start one and exits clean, because the seams' web fallbacks are
# already covered by `make test` and an unrunnable check that fails is a
# check people learn to skip.
# The runner builds a dedicated newer-release bundle fixture for adoption;
# UI trace also makes its renderer inspectable without changing the APK.
e2e-android: UI_TRACE=1
e2e-android: harness-build netns-tool
	cd e2e && pnpm install --frozen-lockfile
	e2e/scripts/android-smoke.sh

# AO_PERF_CONTRACT=1 enforces the wall-clock timing contracts (see
# frontend/src/test/helpers/perfContract.ts). They are gated off by
# default so an ad-hoc `pnpm test` beside the soak rig or a perf profile
# reports the numbers instead of failing on borrowed CPU; the gate is
# where they must always run. vitest-unit and vitest-browser are the one
# entry for each Vitest project: `make test`, `make verify` and CI's two
# Vitest jobs run them. VITEST_BASE=<ref> runs only the test files the
# diff from <ref> can affect (frontend/scripts/vitest-changed.mjs), which
# is what a pull request's jobs pass.
test: $(FRONTEND_DEPS)
	$(MAKE) go-test
	$(MAKE) vitest-unit
	$(MAKE) vitest-browser

vitest-unit vitest-browser: netns-tool $(FRONTEND_DEPS)
	cd frontend && AO_PERF_CONTRACT=1 $(NETNS) pnpm run $(if $(filter vitest-browser,$@),test:browser,test)$(if $(VITEST_BASE),:affected '$(VITEST_BASE)')

# script-tests runs the tests of the scripts under scripts/: every
# scripts/*.test.mjs and the shell scripts' own tests.
script-tests: netns-tool
	$(NETNS) node --test scripts/*.test.mjs
	$(NETNS) sh scripts/test-install-download.sh
	$(NETNS) sh scripts/test-ci-change.sh
	$(NETNS) sh scripts/test-playwright-browsers.sh

check: $(FRONTEND_DEPS)
	$(MAKE) go-build
	cd frontend && pnpm run check

verify:
	./scripts/release-check.sh

release:
	./scripts/build-release.sh --version "$(VERSION)"

# release-wsl-noremote builds the Windows/WSL release without remote access
# (internal/buildvariant), updating from the GitLab project in
# UPDATE_SOURCE (host/group/project):
#   make release-wsl-noremote UPDATE_SOURCE=gitlab.example.com/group/agent-overflow
release-wsl-noremote:
	./scripts/build-release-noremote.sh --version "$(VERSION)" --update-source "$(UPDATE_SOURCE)"

release-macos:
	./scripts/build-release.sh --version "$(VERSION)" --only-macos
