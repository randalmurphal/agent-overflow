.PHONY: help ao-harness-docs install dev dev-wsl launch-wsl harness-wsl perf-wsl soak soak-check soak-contract build build-wsl test check verify release go-build go-test test-race provider-smoke import-corpus-smoke mockprovider harness-build harness harness-window soak-window e2e

# Print the supported build, test, harness, and smoke targets. Keep this
# short enough to use from an unfamiliar checkout. `make e2e` is the
# hermetic harness smoke gate. `make provider-smoke` is deliberately separate
# because it spends real provider tokens. `make import-corpus-smoke` reads
# only caller-supplied copies of provider homes.
help:
	@printf '%s\n' \
		'Build:   make build | make check | make verify' \
		'Tests:   make test | make test-race' \
		'Harness: make harness | make harness-window | make harness-wsl' \
		'Long-run: make soak | make soak-window | make soak-check' \
		'Perf:    make perf-wsl | make soak-contract' \
		'Smoke:   make e2e (mocked) | make provider-smoke (real tokens)' \
		'Import:  make import-corpus-smoke AO_IMPORT_CORPUS_CLAUDE=/copy AO_IMPORT_CORPUS_CODEX=/copy' \
		'Docs:    make ao-harness-docs'

ao-harness-docs:
	go generate ./cmd/ao-harness

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

# DEBUG=1 also starts the loopback pprof listener (127.0.0.1:6363,
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

GO_PACKAGE_ROOTS := . ./cmd/... ./internal/...

# Single source of truth for the release version is build/config.yml#info.version.
# `make build` reads it and forwards it via VERSION= to wails3 build, which the
# platform Taskfiles consume as {{.VERSION | default "dev"}} to stamp
# main.version via -ldflags="-X main.version=...". Override at the make command
# line for one-off builds: `make build VERSION=0.0.2-rc1`.
VERSION ?= $(shell grep '^  version:' build/config.yml | sed 's/.*"\(.*\)".*/\1/')

ifeq ($(shell uname -s),Darwin)
HOST_ARCH := $(shell uname -m)
# Apple Silicon macOS binaries cannot target earlier than 11.0. Keep
# Intel at 10.15 so local `make check` / `make test` stay warning-free
# without dropping older x86_64 support.
ifeq ($(HOST_ARCH),arm64)
MACOSX_DEPLOYMENT_TARGET := 11.0
else
MACOSX_DEPLOYMENT_TARGET := 10.15
endif
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
go-build:
	@set -e; \
	packages=$$(go list -f '{{if or .GoFiles .CgoFiles}}{{.ImportPath}}{{end}}' $(GO_PACKAGE_ROOTS) | sed '/^$$/d'); \
	if [ -z "$$packages" ]; then echo "ERROR: no Go packages found"; exit 1; fi; \
	go build $$packages

go-test:
	@set -e; \
	packages=$$(go list $(GO_PACKAGE_ROOTS)); \
	if [ -z "$$packages" ]; then echo "ERROR: no Go packages found"; exit 1; fi; \
	go test $$packages

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
# pumps on the Codex side. The repo root (`.`)
# is included so the App integration tests (transport server boot,
# multi-session shutdown, event-emit fan-out) get the race detector
# too — that's the surface most likely to hide a real-world race. The
# timeout is per test binary, and every listed package runs in ONE go
# test invocation, so the big suites compete for CPU. Since the
# storetest template-DB conversion (29ec9041 triage, ed21d0fc root —
# one migration replay per package instead of one per test) the two
# heavy legs measure ~108s (triage) and ~270s (root) under -race on an
# idle WSL host (2026-08-25); before it, root alone hit 1800s. The
# timeout stays 1800s for deadlock protection on loaded hosts —
# tighten only if you've measured headroom.
test-race:
	go test -race -timeout 1800s ./internal/transport/... ./internal/triage/... ./internal/provider/... ./internal/wsllauncher/... ./internal/clientmode/... ./internal/editor/... .

# provider-smoke is the real-provider gate: it drives one trivial workflow
# through the REAL `claude` and `codex` binaries (default PATH resolution — no
# binary-path override) and asserts schema acceptance, envelope round-trip, and
# the §9 worktree/branch rules. A Claude-only scenario additionally builds a
# real multi-branch transcript and proves the CLI resumes a fork cut by the
# session importer's lazy branch materialisation. It SPENDS REAL MODEL TOKENS —
# one trivial turn per provider plus four for that scenario — and requires both
# CLIs installed and authenticated.
#
# The `providersmoke` build tag is what keeps it out of `make go-test` and
# `make verify`, which stay hermetic. Run this manually before a release and
# after upgrading either provider CLI; the mocked suites accept any
# structured-output schema, so nothing else in the repo can catch a schema the
# real CLIs reject. See providersmoke_test.go.
#
# -timeout covers the sum of the in-test deadlines (6m per workflow leg, 3m for
# the imported-branch scenario, plus per-leg auth probes) with headroom, so a
# wedged turn fails through the gate's own diagnostics rather than as a bare
# test-binary timeout panic.
provider-smoke:
	go test -tags providersmoke -run 'TestProviderSmoke' -v -count=1 -timeout 20m .

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
# importcorpussmoke_test.go.
import-corpus-smoke:
	AO_IMPORT_CORPUS_CLAUDE="$(AO_IMPORT_CORPUS_CLAUDE)" AO_IMPORT_CORPUS_CODEX="$(AO_IMPORT_CORPUS_CODEX)" \
		go test -run 'TestImportCorpusSmoke' -v -count=1 -timeout 20m .

# playwright install is idempotent and cached (~/.cache/ms-playwright);
# the Chromium binary backs the frontend browser test project
# (`pnpm test:browser`), which is part of `make test` and `make verify`.
install:
	go install tool
	cd frontend && pnpm install --frozen-lockfile
	cd frontend && pnpm exec playwright install chromium

dev:
	go build -o bin/agent-overflow-dev ./cmd/agent-overflow-dev
	AGENT_OVERFLOW_DEBUG=$(AGENT_OVERFLOW_DEBUG) AGENT_OVERFLOW_PPROF=$(AGENT_OVERFLOW_PPROF) VITE_AGENT_OVERFLOW_UI_TRACE=$(UI_TRACE) VITE_AGENT_OVERFLOW_UI_ORACLES=$(UI_ORACLES) bin/agent-overflow-dev

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
# consumed by the Windows launcher itself. AGENT_OVERFLOW_PPROF and
# AGENT_OVERFLOW_RENDERER_DIAG (internal/diagenv) ride both hops to
# reach the WSL backend.
DEV_WSL_FWD_VARS := AGENT_OVERFLOW_DEBUG AGENT_OVERFLOW_WEBVIEW_LOG AGENT_OVERFLOW_WEBVIEW_SOFTWARE AGENT_OVERFLOW_WEBVIEW_EXTRA_ARGS AGENT_OVERFLOW_PPROF AGENT_OVERFLOW_RENDERER_DIAG

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

launch-wsl:
	@if [ -z "$$WSL_DISTRO_NAME" ]; then \
		echo "ERROR: WSL_DISTRO_NAME is unset. Run this target from inside a WSL shell."; \
		exit 1; \
	fi
	@case "$(LAUNCH_PROFILE)" in ""|harness|soak|perf) ;; *) echo "ERROR: LAUNCH_PROFILE must be empty, 'harness', 'soak', or 'perf', got '$(LAUNCH_PROFILE)'" >&2; exit 1;; esac
	@set -e; \
	DEV_VERSION=dev-$$(date +%Y%m%d%H%M%S)-$$$$; \
	$(MAKE) build-wsl WSL_VERSION=$$DEV_VERSION WSL_FORCE_RELINK=1 UI_TRACE=$(UI_TRACE) UI_ORACLES=$(UI_ORACLES) WSL_BUILD_MODE=$(LAUNCH_WSL_BUILD_MODE); \
	if [ -n "$(LAUNCH_PROFILE)" ]; then \
		$(MAKE) mockprovider; \
		mkdir -p "$$HOME/.local/bin"; \
		cp bin/ao-mockprovider "$$HOME/.local/bin/ao-mockprovider.tmp.$$$$"; \
		mv -f "$$HOME/.local/bin/ao-mockprovider.tmp.$$$$" "$$HOME/.local/bin/ao-mockprovider"; \
		echo "Installed mock provider at $$HOME/.local/bin/ao-mockprovider (all isolated profiles resolve it beside the backend binary)"; \
	fi; \
	WIN_LAD=$$(/mnt/c/Windows/System32/cmd.exe /c 'echo %LOCALAPPDATA%' 2>/dev/null | tr -d '\r\n'); \
	if [ -z "$$WIN_LAD" ]; then \
		echo "ERROR: could not resolve %LOCALAPPDATA% via cmd.exe interop."; \
		exit 1; \
	fi; \
	WIN_DEV_DIR_LINUX=$$(wslpath -u "$$WIN_LAD")/agent-overflow/dev; \
	WIN_DEV_EXE_LINUX="$$WIN_DEV_DIR_LINUX/agent-overflow-$$DEV_VERSION.exe"; \
	mkdir -p "$$WIN_DEV_DIR_LINUX"; \
	find "$$WIN_DEV_DIR_LINUX" -maxdepth 1 -name 'agent-overflow-dev-*.exe' ! -name "agent-overflow-$$DEV_VERSION.exe" -delete 2>/dev/null || true; \
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
build-wsl:
	@case "$(WSL_BUILD_MODE)" in build|build:dev) ;; *) echo "ERROR: WSL_BUILD_MODE must be 'build' or 'build:dev', got '$(WSL_BUILD_MODE)'" >&2; exit 1;; esac
	cd frontend && VITE_AGENT_OVERFLOW_UI_TRACE=$(UI_TRACE) VITE_AGENT_OVERFLOW_UI_ORACLES=$(UI_ORACLES) pnpm run $(WSL_BUILD_MODE)
	@if [ -n "$(WSL_FORCE_RELINK)" ]; then rm -f bin/agent-overflow.exe bin/agent-overflow-linux; fi
	WSL_LAUNCHER_MODE="$$(case "$(WSL_BUILD_MODE)" in build:dev) echo dev ;; *) echo prod ;; esac)" VERSION="$(WSL_VERSION)" wails3 task windows:build:wsl

ifeq ($(shell uname -s),Darwin)
build:
	VERSION="$(VERSION)" wails3 task darwin:package
else
build:
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

# harness-build produces bin/agent-overflow with the production SPA
# embedded, plus the sibling ao-mockprovider the harness resolves by
# default. That same bin/agent-overflow is the workflow CLI the workflows
# spec drives by verb (D30) — there is no second binary to build. UI_TRACE=1
# bakes the render-trace instrumentation into the SPA (see the flag docs at
# the top of this file).
#
# bin/ao-harness rides along: it is the shell/agent driver for a running
# instance (docs/specs/testing-harness.md §3, cmd/ao-harness/AGENTS.md),
# and it resolves the backend binary as its own sibling, so building the
# two together is what makes `bin/ao-harness up` need no configuration.
harness-build: mockprovider
	cd frontend && VITE_AGENT_OVERFLOW_UI_TRACE=$(UI_TRACE) VITE_AGENT_OVERFLOW_UI_ORACLES=$(UI_ORACLES) pnpm run build
	go build -ldflags "-X main.version=$(VERSION)" -o bin/agent-overflow .
	go build -ldflags "-X main.version=$(VERSION)" -o bin/ao-harness ./cmd/ao-harness
	go build -o bin/ao-harness-e2e ./cmd/ao-harness-e2e

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
# (instanceinfo.SoakDataRootFor): the soak autopilot refuses a data dir
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
# harness-build. Chromium comes from `make install`'s playwright cache.
e2e: harness-build
	cd e2e && pnpm install --frozen-lockfile
	bin/ao-harness-e2e

test:
	$(MAKE) go-test
	cd frontend && pnpm test
	cd frontend && pnpm run test:browser

check:
	$(MAKE) go-build
	cd frontend && pnpm run check

verify:
	./scripts/release-check.sh

release:
	./scripts/build-release.sh --version "$(VERSION)"
