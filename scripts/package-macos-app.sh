#!/usr/bin/env sh
set -eu
[ "$#" -eq 3 ] || { echo "Usage: package-macos-app.sh BINARY PLIST DESTINATION.app" >&2; exit 2; }
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
. "$SCRIPT_DIR/macos-bundle.sh"
binary=$1
plist=$2
destination=$3
parent=$(dirname -- "$destination")
mkdir -p "$parent"
# Signal handling and temporary files follow main_shell_signals_unix_test.go.
# The stage is on the destination's filesystem, so publishing is a rename.
stage_prefix=$parent/.ao-bundle.$$.
trap 'rm -rf "$stage_prefix"*' EXIT
case ${BASH_VERSION:-} in 5.2.*) ;; *) trap 'exit 129' HUP; trap 'exit 130' INT; trap 'exit 143' TERM ;; esac
stage=$(mktemp -d "${stage_prefix}XXXXXX")
app=$stage/$(basename -- "$destination")
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp "$binary" "$app/Contents/MacOS/agent-overflow"
cp "$plist" "$app/Contents/Info.plist"
cp "$SCRIPT_DIR/../build/darwin/icons.icns" "$app/Contents/Resources"
if [ -f "$SCRIPT_DIR/../build/darwin/Assets.car" ]; then
	cp "$SCRIPT_DIR/../build/darwin/Assets.car" "$app/Contents/Resources"
fi
if [ "$(uname -s)" = Darwin ]; then
	codesign --force --deep --sign - "$app"
	codesign --verify --deep --strict "$app"
else
	echo "Skipping codesign (not available on $(uname -s)). Sign the .app on macOS before distribution."
fi
publish_macos_bundle "$app" "$destination"
