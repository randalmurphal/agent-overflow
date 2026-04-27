.PHONY: install dev dev-wsl build build-wsl test check go-build go-test test-race

# `make dev DEBUG=1` enables lightweight frontend UI render tracing.
# `UI_TRACE=1` is the explicit form; DEBUG=1 is the short dev-mode alias.
UI_TRACE ?= $(DEBUG)

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
	go build ./...

go-test:
	go test ./...

# test-race exercises the concurrency-sensitive packages under -race.
# Scoped to packages with non-trivial goroutine wiring rather than the
# full repo because -race is slow; the listed packages are the ones
# whose teardown / rebind / event-bus paths are most likely to surface
# data-race regressions. The repo root (`.`) is included so the App
# integration tests (transport server boot, multi-session shutdown,
# event-emit fan-out) get the race detector too — that's the surface
# most likely to hide a real-world race. The 240s timeout is the
# documented cap; -race adds significant overhead and the App boot
# tests are the slowest in the matrix.
test-race:
	go test -race -timeout 240s ./internal/transport/... ./internal/wsllauncher/... ./internal/clientmode/... ./internal/editor/... .

install:
	go install tool
	cd frontend && npm install

dev:
	VITE_AGENT_OVERFLOW_UI_TRACE=$(UI_TRACE) wails3 dev

# dev-wsl: cross-compiles the Linux ELF + Windows .exe launcher inside
# this WSL distro, then invokes the .exe via WSL's Windows interop.
# The --distro flag is set from $WSL_DISTRO_NAME so the launcher skips
# the picker and runs the backend in the same distro you're shelled
# into. The override is non-persistent — it doesn't overwrite a
# user-saved choice in %APPDATA%\agent-overflow\wsl.json.
#
# Run this from inside a WSL shell. WSL's interop layer launches the
# .exe as a Windows process; the WebView2 window opens on the Windows
# desktop and connects back to the Linux backend via WSL2's localhost
# forwarding.
dev-wsl: build-wsl
	@if [ -z "$$WSL_DISTRO_NAME" ]; then \
		echo "ERROR: WSL_DISTRO_NAME is unset. Run this target from inside a WSL shell."; \
		exit 1; \
	fi
	@echo "Launching bin/agent-overflow.exe --distro $$WSL_DISTRO_NAME"
	./bin/agent-overflow.exe --distro "$$WSL_DISTRO_NAME"

# build-wsl: cross-compiles the Linux ELF backend + Windows .exe launcher
# without running. Use this when you want to hand the .exe off (e.g.
# copy to the Windows desktop, double-click later) instead of launching
# in place.
build-wsl:
	wails3 task windows:build:wsl

build:
	wails3 build

test:
	$(MAKE) go-test
	cd frontend && npm test

check:
	$(MAKE) go-build
	cd frontend && npm run check
