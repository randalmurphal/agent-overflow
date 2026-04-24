.PHONY: install dev build test check go-build go-test

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
export CGO_LDFLAGS := -mmacosx-version-min=$(MACOSX_DEPLOYMENT_TARGET)
endif

# Use these targets for Go verification instead of bare `go build` / `go test`.
# On Darwin, the exported CGO flags keep Wails Objective-C objects and the final
# binary on the same macOS deployment target, avoiding noisy linker warnings.
go-build:
	go build ./...

go-test:
	go test ./...

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
