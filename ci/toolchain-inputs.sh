#!/usr/bin/env bash
# Prints the pins that define the CI toolchain image, one KEY=VALUE per
# line, read from the files that own them. The image tag is a hash of this
# output plus ci/Dockerfile, so a pin change names a new image and an
# unchanged set reuses the published one. Lockfile contents are not inputs:
# the module cache and package store in the image are warm layers that the
# weekly rebuild and main pushes refresh, and a job fetches whatever they
# lack.
set -euo pipefail
cd "$(dirname "$0")/.."

go_version=$(awk '$1 == "toolchain" { sub(/^go/, "", $2); print $2; exit } $1 == "go" { v = $2 } END { if (v != "") print v }' go.mod | head -n 1)
wails_version=$(awk '$1 == "replace" && $2 ~ /^github.com\/wailsapp\/wails\/v3$/ { print $5; exit }' go.mod)
if [[ -z "$wails_version" ]]; then
  wails_version=$(awk '$1 ~ /^github.com\/wailsapp\/wails\/v3$/ { print $2; exit }' go.mod)
fi
pnpm_version=$(sed -n 's/.*"packageManager": *"pnpm@\([^+"]*\).*/\1/p' frontend/package.json)
playwright_version=$(sed -n 's/.*"@playwright\/test": *"\([^"]*\)".*/\1/p' e2e/package.json)
golangci_version=$(sed -n 's/^GOLANGCI_LINT_VERSION *:= *v\(.*\)$/\1/p' Makefile)

for pair in "GO_VERSION=$go_version" "WAILS_VERSION=$wails_version" "PNPM_VERSION=$pnpm_version" \
  "PLAYWRIGHT_VERSION=$playwright_version" "GOLANGCI_LINT_VERSION=$golangci_version"; do
  if [[ "$pair" == *= ]]; then
    echo "toolchain-inputs: ${pair%=} is empty" >&2
    exit 1
  fi
  echo "$pair"
done
