.PHONY: install dev build test check

install:
	go install tool
	cd frontend && npm install

dev:
	wails3 dev

build:
	wails3 build

test:
	go test ./...
	cd frontend && npm test

check:
	go build ./...
	cd frontend && npm run check
