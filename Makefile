.PHONY: install dev build test check generate-css

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
