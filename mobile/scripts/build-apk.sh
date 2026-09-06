#!/usr/bin/env bash
# Build a debug or signed release APK from the ordinary production SPA.
#
# Three steps and they are strictly ordered. `cap sync` copies whatever is
# in `frontend/dist` into the Android assets, so a build that skipped the
# SPA step would package the bundle from whenever it was last built and
# say nothing about it.
#
# The SPA it syncs is the ordinary `frontend` build, the same bundle the
# backend embeds and serves to a paired phone: there is no shell flavour,
# because a phone that adopts the backend's bundle has to find the native
# seams in it (mobile/AGENTS.md § One bundle).
#
# JAVA_HOME and ANDROID_HOME must be exported by the caller or discovered
# below. Neither is on PATH on the development box, so this script says so
# rather than letting Gradle fail with a message about a toolchain.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
mobile="$(dirname "$here")"
repo="$(dirname "$mobile")"

node "$here/generate-icons.mjs" --check

variant="${1:-debug}"
case "$variant" in
  debug) task=Debug ;;
  release)
    task=Release
    for name in AO_ANDROID_KEYSTORE AO_ANDROID_STORE_PASSWORD AO_ANDROID_KEY_ALIAS AO_ANDROID_KEY_PASSWORD; do
      if [[ -z "${!name:-}" ]]; then
        echo "release signing needs $name; see docs/architecture/remote-access-setup.md" >&2
        exit 1
      fi
    done
    if [[ ! -f "$AO_ANDROID_KEYSTORE" || "$AO_ANDROID_KEYSTORE" != /* ]]; then
      echo "AO_ANDROID_KEYSTORE must name an existing absolute keystore path" >&2
      exit 1
    fi
    ;;
  *) echo "Usage: $0 [debug|release]" >&2; exit 2 ;;
esac

: "${JAVA_HOME:=$HOME/.jdks/temurin-21}"
: "${ANDROID_HOME:=$HOME/Android/Sdk}"
export JAVA_HOME ANDROID_HOME

if [[ ! -x "$JAVA_HOME/bin/java" ]]; then
  echo "no JDK at JAVA_HOME=$JAVA_HOME — install a JDK 21 and export JAVA_HOME" >&2
  exit 1
fi
if [[ ! -d "$ANDROID_HOME/platforms" ]]; then
  echo "no Android SDK at ANDROID_HOME=$ANDROID_HOME — install platform-tools, platforms;android-36 and build-tools;36.0.0" >&2
  exit 1
fi

# Gradle reads the SDK location from here rather than from the
# environment, and the file names this machine, so it is generated and
# never committed.
printf 'sdk.dir=%s\n' "$ANDROID_HOME" > "$mobile/android/local.properties"

echo "==> building the SPA"
pnpm --dir "$repo/frontend" run build

echo "==> cap sync android"
(cd "$mobile" && pnpm exec cap sync android)

echo "==> test${task}UnitTest and assemble${task}"
# The JVM half of the native code, run BEFORE the APK is assembled so a
# broken bundle store cannot be packaged. `BundleStore` deliberately takes
# a directory and no Android type precisely so the update mechanic — state
# transitions, unzip, verification, rollback — is provable here rather than
# on an emulator (mobile/AGENTS.md § The bundle plugin).
(cd "$mobile/android" && ./gradlew --no-daemon "test${task}UnitTest" "assemble${task}")

apk="$mobile/android/app/build/outputs/apk/$variant/app-$variant.apk"
if [[ ! -f "$apk" ]]; then
  echo "gradle reported success but no APK is at $apk" >&2
  exit 1
fi
echo "==> $apk ($(du -h "$apk" | cut -f1))"

if [[ "$variant" == release ]]; then
  "$ANDROID_HOME/build-tools/36.0.0/apksigner" verify "$apk"
  version="$(node -p "require(process.argv[1]).version" "$repo/frontend/package.json")"
  out="$repo/dist/release/$version"
  mkdir -p "$out"
  cp "$apk" "$out/agent-overflow-android.apk"
  echo "==> $out/agent-overflow-android.apk"
fi
