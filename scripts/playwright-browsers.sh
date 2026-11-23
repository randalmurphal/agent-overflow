#!/usr/bin/env bash
# Installs a declared set of Playwright browsers: the one installation path
# for the CI image (ci/Dockerfile), .github/actions/setup, and the Make
# targets that run Playwright.
#
#   scripts/playwright-browsers.sh [--with-deps] SET
#
# SET names a set in build/test-inputs.mk. `playwright install` downloads
# only a build that is missing. --with-deps then installs the OS libraries
# the set needs when Playwright's own simulation finds one missing (Linux;
# Playwright asks for sudo when not root). The Playwright CLI is
# $PLAYWRIGHT_CLI, else the e2e package's, else the frontend package's;
# both lockfiles pin one version.
set -euo pipefail
cd "$(dirname "$0")/.."

deps=false
if [[ "${1:-}" == --with-deps ]]; then
  deps=true
  shift
fi
if [[ $# -ne 1 || ! "$1" =~ ^[A-Z][A-Z0-9_]*$ ]]; then
  echo "usage: scripts/playwright-browsers.sh [--with-deps] SET (a set in build/test-inputs.mk)" >&2
  exit 2
fi
set_name=$1
browsers=$(sed -n "s/^${set_name} *:= *//p" build/test-inputs.mk)
if [[ -z "$browsers" ]]; then
  echo "playwright-browsers: build/test-inputs.mk declares no set ${set_name}" >&2
  exit 2
fi

if [[ -n "${PLAYWRIGHT_CLI:-}" ]]; then
  read -r -a cli <<< "$PLAYWRIGHT_CLI"
elif [[ -x e2e/node_modules/.bin/playwright ]]; then
  cli=(e2e/node_modules/.bin/playwright)
elif [[ -x frontend/node_modules/.bin/playwright ]]; then
  cli=(frontend/node_modules/.bin/playwright)
else
  echo "playwright-browsers: no Playwright CLI; install the e2e or frontend dependencies, or set PLAYWRIGHT_CLI" >&2
  exit 1
fi

echo "playwright-browsers: ${set_name} = ${browsers}"
# shellcheck disable=SC2086
"${cli[@]}" install $browsers
if [[ "$deps" == true ]]; then
  # shellcheck disable=SC2086
  if ! "${cli[@]}" install-deps --dry-run $browsers > /dev/null 2>&1; then
    # shellcheck disable=SC2086
    "${cli[@]}" install-deps $browsers
  fi
fi
