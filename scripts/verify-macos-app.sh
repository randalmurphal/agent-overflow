#!/usr/bin/env sh
set -eu

usage() {
	echo "Usage: scripts/verify-macos-app.sh [--require-notarized] APP_BUNDLE" >&2
}

REQUIRE_NOTARIZED=0
if [ "${1:-}" = "--require-notarized" ]; then
	REQUIRE_NOTARIZED=1
	shift
fi
[ "$#" -eq 1 ] || { usage; exit 2; }
APP=$1
[ "$(uname -s)" = Darwin ] || { echo "ERROR: macOS app verification requires macOS" >&2; exit 1; }
[ -d "$APP" ] || { echo "ERROR: app bundle does not exist: $APP" >&2; exit 1; }

PLIST=$APP/Contents/Info.plist
EXECUTABLE=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleExecutable' "$PLIST")
BUNDLE_ID=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$PLIST")
VERSION=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$PLIST")
BIN=$APP/Contents/MacOS/$EXECUTABLE

[ "$BUNDLE_ID" = com.agentoverflow.app ] || { echo "ERROR: unexpected bundle id: $BUNDLE_ID" >&2; exit 1; }
[ -n "$VERSION" ] || { echo "ERROR: bundle version is empty" >&2; exit 1; }
[ -x "$BIN" ] || { echo "ERROR: bundle executable is missing or not executable: $BIN" >&2; exit 1; }
case "$(lipo -archs "$BIN")" in
	*arm64*) ;;
	*) echo "ERROR: macOS executable does not contain arm64: $BIN" >&2; exit 1 ;;
esac

codesign --verify --deep --strict --verbose=2 "$APP"
if [ "$REQUIRE_NOTARIZED" -eq 1 ]; then
	if codesign -dvv "$APP" 2>&1 | grep -q '^Signature=adhoc$'; then
		echo "ERROR: release bundle is only ad-hoc signed" >&2
		exit 1
	fi
	xcrun stapler validate "$APP"
	spctl --assess --type execute --verbose=2 "$APP"
fi

echo "Verified macOS app: $BUNDLE_ID $VERSION ($(lipo -archs "$BIN"))"
