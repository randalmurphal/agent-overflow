#!/usr/bin/env sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
VERSION=${1:-}

if [ -z "$VERSION" ]; then
	VERSION=$(sed -n 's/^  version: "\([^"]*\)"/\1/p' "$ROOT_DIR/build/config.yml")
fi
[ -n "$VERSION" ] || { echo "ERROR: version is required" >&2; exit 2; }

case "$VERSION" in
	""|.*|-*|*..*|*[!0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz._+-]*)
		echo "ERROR: unsafe release version: $VERSION" >&2
		exit 2
		;;
esac

replace() {
	file=$1
	pattern=$2
	replacement=$3
	count=$(perl -0ne "my @matches = m{$pattern}g; print scalar @matches" "$file")
	[ "$count" -gt 0 ] || { echo "ERROR: pattern did not match $file: $pattern" >&2; exit 1; }
	perl -0pi -e "s{$pattern}{$replacement}g" "$file"
}

replace "$ROOT_DIR/build/config.yml" 'version: "[^"]+"' "version: \"$VERSION\""
replace "$ROOT_DIR/build/darwin/Info.plist" '<key>CFBundleShortVersionString</key>\s*<string>[^<]+</string>' "<key>CFBundleShortVersionString</key>\n\t\t<string>$VERSION</string>"
replace "$ROOT_DIR/build/darwin/Info.plist" '<key>CFBundleVersion</key>\s*<string>[^<]+</string>' "<key>CFBundleVersion</key>\n\t\t<string>$VERSION</string>"
replace "$ROOT_DIR/build/darwin/Info.dev.plist" '<key>CFBundleShortVersionString</key>\s*<string>[^<]+</string>' "<key>CFBundleShortVersionString</key>\n\t\t<string>$VERSION</string>"
replace "$ROOT_DIR/build/darwin/Info.dev.plist" '<key>CFBundleVersion</key>\s*<string>[^<]+</string>' "<key>CFBundleVersion</key>\n\t\t<string>$VERSION</string>"
replace "$ROOT_DIR/build/linux/nfpm/nfpm.yaml" 'version: "[^"]+"' "version: \"$VERSION\""
replace "$ROOT_DIR/build/windows/info.json" '"file_version": "[^"]+"' "\"file_version\": \"$VERSION\""
replace "$ROOT_DIR/build/windows/info.json" '"ProductVersion": "[^"]+"' "\"ProductVersion\": \"$VERSION\""
replace "$ROOT_DIR/build/windows/wails.exe.manifest" '(<assemblyIdentity type="win32" name="com\.agentoverflow\.app" version=")[^"]+(")' "\${1}$VERSION\${2}"
replace "$ROOT_DIR/frontend/package.json" '"version": "[^"]+"' "\"version\": \"$VERSION\""
