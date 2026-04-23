.PHONY: install dev build test check generate-css

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

install:
	go install tool
	cd frontend && npm install

# generate-css re-derives frontend/src/styles/chroma-{dark,light}.css
# from Chroma's style tables. The output is committed so clean checkouts
# don't need a Go build step to view highlighted code, but every
# build/check path runs it so the CSS never drifts from the Go renderer.
generate-css:
	go run ./cmd/gen-chroma-css

dev: generate-css
	wails3 dev

build: generate-css
	wails3 build

test:
	go test ./...
	cd frontend && npm test

check: generate-css
	go build ./...
	cd frontend && npm run check
