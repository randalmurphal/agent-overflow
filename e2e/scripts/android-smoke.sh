#!/usr/bin/env bash
# `make e2e-android`: the shell smoke, inside a real Android WebView.
#
# WHY IT EXISTS. Everything in `frontend/src/lib/native/` has a web
# fallback and `make test` covers the fallback side of every seam — that
# a browser build is INERT. What no fallback can answer is the other side:
# whether the bundle boots at all under the shell's fixed origin, whether
# the plugins the APK was built with actually register, whether the app
# lock gates the app, and whether the back button reaches
# `showCompactList`. Those need a device or an emulator.
#
# WHY IT IS NOT A BLOCKING GATE. A laptop with no emulator cannot answer
# those questions, and a check that fails for a reason nobody can fix on
# the spot is a check people learn to skip. So a missing emulator prints
# how to start one and exits 0. What the run costs when it IS available is
# a few minutes, which is why it is its own target rather than part of
# `make e2e`.
#
# WHAT THIS SCRIPT OWNS, and why each step is here rather than in the spec:
#
#   - The APK install.
#   - The device PIN. The emulator has no biometric, and `native/lock.ts`
#     passes `allowDeviceCredential: true` so the platform falls back to
#     the credential the phone is unlocked with — so a device with NO
#     credential has no prompt for the spec to answer. Set before the run
#     and cleared after, including on failure.
#
# What the spec owns is everything per CASE and everything that depends
# on a port that does not exist yet: `pm clear` and the launch (its
# `page` fixture, so every case starts from a phone that has never
# paired — the shell persists its backend endpoint and its session in
# the WebView's localStorage, and the harness backend is on a fresh
# ephemeral port each run), the notification permission grant, the
# harness backend, the `adb reverse` forward that lets the device reach
# it, and the pairing. `android/shell-boot.spec.ts` argues the reverse
# forward in place.
set -euo pipefail

: "${ANDROID_HOME:=$HOME/Android/Sdk}"
export ANDROID_HOME
adb="$ANDROID_HOME/platform-tools/adb"
pkg="dev.agentoverflow.app"
pin="1234"

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
  # The system image must match the host: the emulator runs an arm64
  # guest on Apple Silicon and an x86_64 one everywhere else, and the
  # other one either fails to install or boots at a crawl.
  case "$(uname -m)" in
    arm64 | aarch64) abi=arm64-v8a ;;
    *) abi=x86_64 ;;
  esac
  cat <<MSG
No Android device or emulator is attached, so the shell smoke is skipped.

To run it:
  $ANDROID_HOME/cmdline-tools/latest/bin/sdkmanager "system-images;android-36;google_apis;$abi"
  $ANDROID_HOME/cmdline-tools/latest/bin/avdmanager create avd -n ao -k "system-images;android-36;google_apis;$abi"
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

# The app's data, the notification permission and the launch itself are
# the spec's `page` fixture's to do, before EVERY case: a phone that has
# never paired is each case's starting point, not the run's.

# The credential the app lock falls back to. Cleared on every exit path,
# including a failed run, so the emulator is left as it was found.
echo "==> setting the device PIN"
"$adb" -s "$serial" shell locksettings set-pin "$pin"
trap '"$adb" -s "$serial" shell locksettings clear --old "$pin" >/dev/null 2>&1 || true' EXIT

# The launcher rather than a bare `pnpm exec playwright test`, for two
# reasons that are the same two every other suite here has: it typechecks
# the whole e2e tree first (Playwright only STRIPS types, so a typo'd
# property in this spec would pass an assertion vacuously), and it runs
# the suite under one owned process tree and memory boundary. Its extra
# arguments are forwarded verbatim, so the command it actually runs is
# `pnpm exec playwright test --config=playwright.android.config.ts` with
# `e2e/` as its working directory. It is also what lets the spec call
# `launchHarness`, which refuses to spawn a backend outside it.
echo "==> the shell smoke, in the emulator's WebView"
cd "$repo"
AO_ANDROID_SERIAL="$serial" bin/ao-harness-e2e --config=playwright.android.config.ts
