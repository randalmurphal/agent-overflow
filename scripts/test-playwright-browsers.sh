#!/usr/bin/env sh
# Checks scripts/playwright-browsers.sh against a stand-in Playwright CLI:
# a set resolves to the names build/test-inputs.mk declares, --with-deps
# installs OS libraries only when Playwright's simulation finds one
# missing, and an unknown set or a bare browser name is refused.
set -eu

ROOT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ao-test-playwright-browsers.XXXXXX")
trap 'rm -rf "$TMP_DIR"' EXIT
trap 'exit 129' HUP; trap 'exit 130' INT; trap 'exit 143' TERM

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

calls=$TMP_DIR/calls
cat > "$TMP_DIR/playwright" <<'CLI'
#!/bin/sh
echo "$*" >> "$CALLS"
case "$*" in
	"install-deps --dry-run"*) [ "${DEPS_MISSING:-0}" = 0 ] ;;
esac
CLI
chmod +x "$TMP_DIR/playwright"
export CALLS="$calls" PLAYWRIGHT_CLI="$TMP_DIR/playwright"
manual=`sed -n 's/^E2E_BROWSERS_MANUAL *:= *//p' "$ROOT_DIR/build/test-inputs.mk"`
[ -n "$manual" ] || fail "build/test-inputs.mk declares no E2E_BROWSERS_MANUAL"

"$ROOT_DIR/scripts/playwright-browsers.sh" E2E_BROWSERS_MANUAL > /dev/null
got=`cat "$calls"`
[ "$got" = "install $manual" ] || fail "without --with-deps: $got"

: > "$calls"
"$ROOT_DIR/scripts/playwright-browsers.sh" --with-deps E2E_BROWSERS_MANUAL > /dev/null
got=`cat "$calls"`
[ "$got" = "install $manual
install-deps --dry-run $manual" ] || fail "with the libraries present: $got"

: > "$calls"
DEPS_MISSING=1 "$ROOT_DIR/scripts/playwright-browsers.sh" --with-deps E2E_BROWSERS_MANUAL > /dev/null
got=`cat "$calls"`
[ "$got" = "install $manual
install-deps --dry-run $manual
install-deps $manual" ] || fail "with a library missing: $got"

for bad in NO_SUCH_SET chromium; do
	: > "$calls"
	if "$ROOT_DIR/scripts/playwright-browsers.sh" "$bad" > /dev/null 2>&1; then fail "$bad was accepted"; fi
	got=`cat "$calls"`
	[ -z "$got" ] || fail "$bad reached Playwright: $got"
done

echo "playwright-browsers.sh tests passed"
