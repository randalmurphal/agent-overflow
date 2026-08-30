#!/usr/bin/env sh
set -eu

usage() {
	cat <<'USAGE'
Usage: scripts/sign-notarize-macos.sh APP_BUNDLE

Developer-ID signs, notarizes, staples, and Gatekeeper-verifies a macOS app.

Required environment:
  AO_MACOS_SIGN_IDENTITY

And either:
  AO_MACOS_NOTARY_KEYCHAIN_PROFILE

Or all three:
  AO_MACOS_NOTARY_APPLE_ID
  AO_MACOS_NOTARY_TEAM_ID
  AO_MACOS_NOTARY_PASSWORD
USAGE
}

[ "$#" -eq 1 ] || { usage >&2; exit 2; }
APP=$1
[ -d "$APP" ] || { echo "ERROR: macOS app bundle does not exist: $APP" >&2; exit 1; }
: "${AO_MACOS_SIGN_IDENTITY:?AO_MACOS_SIGN_IDENTITY is required}"

PROFILE=${AO_MACOS_NOTARY_KEYCHAIN_PROFILE:-}
if [ -z "$PROFILE" ]; then
	: "${AO_MACOS_NOTARY_APPLE_ID:?AO_MACOS_NOTARY_APPLE_ID is required without a keychain profile}"
	: "${AO_MACOS_NOTARY_TEAM_ID:?AO_MACOS_NOTARY_TEAM_ID is required without a keychain profile}"
	: "${AO_MACOS_NOTARY_PASSWORD:?AO_MACOS_NOTARY_PASSWORD is required without a keychain profile}"
fi

TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/agent-overflow-notary.XXXXXX")
trap 'rm -rf "$TMP_DIR"' EXIT HUP INT TERM
SUBMISSION=$TMP_DIR/agent-overflow.zip

codesign \
	--force \
	--deep \
	--options runtime \
	--timestamp \
	--sign "$AO_MACOS_SIGN_IDENTITY" \
	"$APP"
codesign --verify --deep --strict --verbose=2 "$APP"

# Apple recommends ditto for notarization submissions; it preserves bundle
# metadata without making the distributable zip until after the ticket is
# stapled.
ditto -c -k --sequesterRsrc --keepParent "$APP" "$SUBMISSION"
if [ -n "$PROFILE" ]; then
	xcrun notarytool submit "$SUBMISSION" \
		--keychain-profile "$PROFILE" \
		--wait
else
	xcrun notarytool submit "$SUBMISSION" \
		--apple-id "$AO_MACOS_NOTARY_APPLE_ID" \
		--team-id "$AO_MACOS_NOTARY_TEAM_ID" \
		--password "$AO_MACOS_NOTARY_PASSWORD" \
		--wait
fi

xcrun stapler staple "$APP"
xcrun stapler validate "$APP"
codesign --verify --deep --strict --verbose=2 "$APP"
spctl --assess --type execute --verbose=2 "$APP"
