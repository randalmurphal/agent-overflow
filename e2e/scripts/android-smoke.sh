#!/usr/bin/env bash
# `make e2e-android`: the compact suite, inside a real Android WebView.
#
# WHY IT EXISTS. Everything in `frontend/src/lib/native/` has a web
# fallback and `make test` covers the fallback side of every seam — that
# a browser build is INERT. What no fallback can answer is the other side:
# whether the bundle boots at all under the shell's fixed origin, whether
# the plugins the APK was built with actually register, and whether the
# back button reaches `showCompactList`. Those need a device or an
# emulator.
#
# WHY IT IS NOT A BLOCKING GATE. A laptop with no emulator cannot answer
# those questions, and a check that fails for a reason nobody can fix on
# the spot is a check people learn to skip. So a missing emulator prints
# how to start one and exits 0. What the run costs when it IS available is
# a few minutes, which is why it is its own target rather than part of
# `make e2e`.
set -euo pipefail

: "${ANDROID_HOME:=$HOME/Android/Sdk}"
export ANDROID_HOME
adb="$ANDROID_HOME/platform-tools/adb"

if [[ ! -x "$adb" ]]; then
  echo "no adb at $adb — install platform-tools, then: make e2e-android"
  exit 0
fi

# A device line is "<serial>\t<state>"; only `device` is ready. Anything
# else (offline, unauthorized, a booting emulator) is not something to
# run a suite against, and reading it as one is how a flake gets blamed
# on the app.
devices="$("$adb" devices | tail -n +2 | awk '$2 == "device" { print $1 }')"
if [[ -z "$devices" ]]; then
  cat <<'MSG'
No Android device or emulator is attached, so the shell smoke is skipped.

To run it:
  $ANDROID_HOME/cmdline-tools/latest/bin/sdkmanager "system-images;android-35;google_apis;x86_64"
  $ANDROID_HOME/cmdline-tools/latest/bin/avdmanager create avd -n ao -k "system-images;android-35;google_apis;x86_64"
  $ANDROID_HOME/emulator/emulator -avd ao &
  make apk && make e2e-android
MSG
  exit 0
fi

serial="$(echo "$devices" | head -n1)"
echo "==> device $serial"

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
apk="$repo/mobile/android/app/build/outputs/apk/debug/app-debug.apk"
if [[ ! -f "$apk" ]]; then
  echo "no APK at $apk — run 'make apk' first" >&2
  exit 1
fi

echo "==> installing"
"$adb" -s "$serial" install -r "$apk"

# The compact project against the harness, driving the app INSIDE the
# shell's WebView over adb's debug bridge. `AO_ANDROID_SERIAL` is what
# the config reads to attach to the WebView rather than launching its own
# Chromium.
echo "==> compact project, in the shell"
cd "$repo"
AO_ANDROID_SERIAL="$serial" bin/ao-harness-e2e --project=compact
