.PHONY: install dev build test check go-build go-test test-race

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

build:
	wails3 build

test:
	$(MAKE) go-test
	cd frontend && npm test

check:
	$(MAKE) go-build
	cd frontend && npm run check
