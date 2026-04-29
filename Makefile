.PHONY: install dev dev-wsl build build-wsl test check go-build go-test test-race

# `make dev DEBUG=1` enables lightweight frontend UI render tracing.
# `UI_TRACE=1` is the explicit form; DEBUG=1 is the short dev-mode alias.
UI_TRACE ?= $(DEBUG)
GO_PACKAGE_ROOTS := . ./internal/... ./build/...

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
# data-race regressions. The repo root (`.`) is included so the App
# integration tests (transport server boot, multi-session shutdown,
# event-emit fan-out) get the race detector too — that's the surface
# most likely to hide a real-world race. The 600s timeout absorbs the
# App boot suite, which runs ~380s under -race on slower hosts (WSL,
# CI runners). Tighten only if you've measured headroom.
test-race:
	go test -race -timeout 600s ./internal/transport/... ./internal/wsllauncher/... ./internal/clientmode/... ./internal/editor/... .

install:
	go install tool
	cd frontend && corepack pnpm install --frozen-lockfile

dev:
	VITE_AGENT_OVERFLOW_UI_TRACE=$(UI_TRACE) wails3 dev

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
dev-wsl:
	@if [ -z "$$WSL_DISTRO_NAME" ]; then \
		echo "ERROR: WSL_DISTRO_NAME is unset. Run this target from inside a WSL shell."; \
		exit 1; \
	fi
	@set -e; \
	DEV_VERSION=dev-$$(date +%Y%m%d%H%M%S)-$$$$; \
	$(MAKE) build-wsl WSL_VERSION=$$DEV_VERSION WSL_FORCE_RELINK=1; \
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
	echo "Launching $$WIN_DEV_EXE_LINUX --distro $$WSL_DISTRO_NAME"; \
	"$$WIN_DEV_EXE_LINUX" --distro "$$WSL_DISTRO_NAME"

# build-wsl: cross-compiles the Linux ELF backend + Windows .exe launcher
# without running. Use this when you want to hand the .exe off (e.g.
# copy to the Windows desktop, double-click later) instead of launching
# in place.
build-wsl:
	cd frontend && corepack pnpm run build
	@if [ -n "$(WSL_FORCE_RELINK)" ]; then rm -f bin/agent-overflow.exe bin/agent-overflow-linux; fi
	VERSION="$(WSL_VERSION)" wails3 task windows:build:wsl

build:
	wails3 build

test:
	$(MAKE) go-test
	cd frontend && corepack pnpm test

check:
	$(MAKE) go-build
	cd frontend && corepack pnpm run check
