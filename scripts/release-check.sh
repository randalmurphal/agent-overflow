#!/usr/bin/env sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

run() {
	printf '\n==> %s\n' "$*"
	"$@"
}

# The checks below regenerate derived files (Wails bindings, icons). If any
# regenerated output differs from what's committed, the release workflow's
# clean-tree guard will fail in CI — catch the drift here instead. Uncommitted
# work you started with is fine; only changes the checks themselves introduce
# fail.
STATUS_BEFORE=$(git -C "$ROOT_DIR" status --porcelain)

run sh -c "cd '$ROOT_DIR/frontend' && pnpm run build"
run make -C "$ROOT_DIR" go-build
run make -C "$ROOT_DIR" go-test
run make -C "$ROOT_DIR" provider-smoke-compile
run sh -c "cd '$ROOT_DIR/frontend' && pnpm run check"
run sh -c "cd '$ROOT_DIR/frontend' && AO_PERF_CONTRACT=1 pnpm test"
run sh -c "cd '$ROOT_DIR/frontend' && AO_PERF_CONTRACT=1 pnpm run test:browser"
run make -C "$ROOT_DIR" build

case "$(uname -s)" in
	Darwin)
		run "$ROOT_DIR/scripts/verify-macos-app.sh" "$ROOT_DIR/bin/agent-overflow.app"
		;;
esac

case "$(uname -s)" in
	Linux)
		run make -C "$ROOT_DIR" build-wsl
		;;
	*)
		printf '\n==> Skipping make build-wsl on %s; run it from Linux/WSL for Windows release artifacts.\n' "$(uname -s)"
		;;
esac

STATUS_AFTER=$(git -C "$ROOT_DIR" status --porcelain)
if [ "$STATUS_AFTER" != "$STATUS_BEFORE" ]; then
	echo "ERROR: the checks changed tracked files — regenerated output differs from what's committed." >&2
	echo "Commit the changes below or the release workflow's clean-tree guard will fail in CI:" >&2
	tmpdir=$(mktemp -d)
	trap 'rm -rf "$tmpdir"' EXIT
	printf '%s\n' "$STATUS_BEFORE" | sort > "$tmpdir/before"
	printf '%s\n' "$STATUS_AFTER" | sort > "$tmpdir/after"
	comm -13 "$tmpdir/before" "$tmpdir/after" >&2
	exit 1
fi
