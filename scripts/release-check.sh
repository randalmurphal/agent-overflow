#!/usr/bin/env sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

run() {
	printf '\n==> %s\n' "$*"
	"$@"
}

run sh -c "cd '$ROOT_DIR/frontend' && pnpm run build"
run make -C "$ROOT_DIR" go-build
run make -C "$ROOT_DIR" go-test
run sh -c "cd '$ROOT_DIR/frontend' && pnpm run check"
run sh -c "cd '$ROOT_DIR/frontend' && pnpm test"
run sh -c "cd '$ROOT_DIR/frontend' && pnpm run test:browser"
run make -C "$ROOT_DIR" build

case "$(uname -s)" in
	Linux)
		run make -C "$ROOT_DIR" build-wsl
		;;
	*)
		printf '\n==> Skipping make build-wsl on %s; run it from Linux/WSL for Windows release artifacts.\n' "$(uname -s)"
		;;
esac
