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
stage=$(mktemp -d "$parent/.ao-bundle.XXXXXX")
trap 'rm -rf "$stage"' EXIT
trap 'exit 1' HUP INT TERM
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
