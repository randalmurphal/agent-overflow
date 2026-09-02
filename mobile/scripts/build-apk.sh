#!/usr/bin/env bash
# Build the debug APK: the SPA, the Capacitor sync, and Gradle.
#
# Three steps and they are strictly ordered. `cap sync` copies whatever is
# in `frontend/dist` into the Android assets, so a build that skipped the
# SPA step would package the bundle from whenever it was last built and
# say nothing about it.
#
# AO_SHELL=1 is what makes the SPA build a SHELL build: it points
# `frontend/vite.config.ts`'s Capacitor aliases at the real packages under
# `mobile/node_modules` instead of at the local stub. This script is the
# only thing that sets it, which is what keeps `make build` and every gate
# producing the ordinary desktop bundle.
#
# JAVA_HOME and ANDROID_HOME must be exported by the caller or discovered
# below. Neither is on PATH on the development box, so this script says so
# rather than letting Gradle fail with a message about a toolchain.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
mobile="$(dirname "$here")"
repo="$(dirname "$mobile")"

: "${JAVA_HOME:=$HOME/.jdks/temurin-21}"
: "${ANDROID_HOME:=$HOME/Android/Sdk}"
export JAVA_HOME ANDROID_HOME

if [[ ! -x "$JAVA_HOME/bin/java" ]]; then
  echo "no JDK at JAVA_HOME=$JAVA_HOME — install a JDK 21 and export JAVA_HOME" >&2
  exit 1
fi
if [[ ! -d "$ANDROID_HOME/platforms" ]]; then
  echo "no Android SDK at ANDROID_HOME=$ANDROID_HOME — install platform-tools, platforms;android-35 and build-tools;35.0.0" >&2
  exit 1
fi

# Gradle reads the SDK location from here rather than from the
# environment, and the file names this machine, so it is generated and
# never committed.
printf 'sdk.dir=%s\n' "$ANDROID_HOME" > "$mobile/android/local.properties"

echo "==> building the SPA (shell aliases on)"
AO_SHELL=1 pnpm --dir "$repo/frontend" run build

echo "==> cap sync android"
(cd "$mobile" && pnpm exec cap sync android)

echo "==> assembleDebug"
(cd "$mobile/android" && ./gradlew --no-daemon assembleDebug)

apk="$mobile/android/app/build/outputs/apk/debug/app-debug.apk"
if [[ ! -f "$apk" ]]; then
  echo "gradle reported success but no APK is at $apk" >&2
  exit 1
fi
echo "==> $apk ($(du -h "$apk" | cut -f1))"
