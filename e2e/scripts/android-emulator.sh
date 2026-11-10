#!/usr/bin/env bash
# Start or stop the Android emulator for `make e2e-android` under a hard
# memory cap. Read docs/references/android-emulator.md before changing the
# launch: emulator 37.1 leaks multi-gigabyte host allocations from its adb
# proxy on every Playwright session, and an uncapped emulator has taken the
# whole WSL VM down with it. The cap is the containment, not a tuning knob.
#
#   e2e/scripts/android-emulator.sh start   # boots AVD `ao`, waits for it
#   e2e/scripts/android-emulator.sh stop
#
#   AO_EMULATOR_AVD      AVD name (default ao)
#   AO_EMULATOR_MEMORY   cgroup cap (default 10g; the leak needs 3-4 GB of
#                        headroom per pairing on top of a ~4 GB idle guest)
#   AO_EMULATOR_ARGS     extra emulator flags
#
# Linux and WSL2 only: it needs Docker with /dev/kvm. Elsewhere, run the
# emulator under whatever memory limit the platform offers and restart it
# between test batches.
set -euo pipefail

: "${ANDROID_HOME:=$HOME/Android/Sdk}"
: "${ANDROID_AVD_HOME:=$HOME/.android/avd}"
avd="${AO_EMULATOR_AVD:-ao}"
memory="${AO_EMULATOR_MEMORY:-10g}"
name="ao-emulator"
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
adb="$ANDROID_HOME/platform-tools/adb"

case "${1:-}" in
  start) ;;
  stop)
    docker rm -f "$name" >/dev/null 2>&1 || true
    rm -f "$ANDROID_AVD_HOME/$avd.avd"/*.lock
    echo "==> $name stopped"
    exit 0
    ;;
  *) echo "usage: $0 start|stop" >&2; exit 2 ;;
esac

for need in docker "$adb" "$ANDROID_HOME/emulator/emulator"; do
  command -v "$need" >/dev/null 2>&1 || [[ -x "$need" ]] || { echo "missing $need" >&2; exit 1; }
done
[[ -e /dev/kvm ]] || { echo "/dev/kvm is required; enable nested virtualization for this VM" >&2; exit 1; }
[[ -d "$ANDROID_AVD_HOME/$avd.avd" ]] || {
  echo "no AVD '$avd' under $ANDROID_AVD_HOME; create it with avdmanager first (make e2e-android prints the commands)" >&2
  exit 1
}
if docker ps -q --filter "name=^$name$" | grep -q .; then
  echo "==> $name is already running"
  exit 0
fi
docker rm -f "$name" >/dev/null 2>&1 || true
# A killed emulator leaves AVD lock files that refuse the next launch.
rm -f "$ANDROID_AVD_HOME/$avd.avd"/*.lock

docker build -q -t ao-emulator -f "$repo/e2e/android/emulator.Dockerfile" "$repo/e2e/android" >/dev/null
kvm_gid="$(stat -c %g /dev/kvm)"
docker run -d --name "$name" --network host --device /dev/kvm --group-add "$kvm_gid" \
  --user "$(id -u):$(id -g)" --memory "$memory" --memory-swap "$memory" --cpus 4 \
  -e HOME="$HOME" -e ANDROID_HOME="$ANDROID_HOME" -e ANDROID_SDK_ROOT="$ANDROID_HOME" \
  -e ANDROID_AVD_HOME="$ANDROID_AVD_HOME" \
  -v "$ANDROID_HOME:$ANDROID_HOME:ro" -v "$HOME/.android:$HOME/.android" \
  ao-emulator "$ANDROID_HOME/emulator/emulator" -avd "$avd" -no-window -no-audio -no-boot-anim \
  -gpu swiftshader_indirect -no-snapshot -memory 3072 -cores 2 ${AO_EMULATOR_ARGS:-} >/dev/null

echo "==> booting $avd in $name (memory cap $memory)"
start=$(date +%s)
until [[ "$("$adb" shell getprop sys.boot_completed 2>/dev/null | tr -d '\r')" == 1 ]]; do
  if ! docker ps -q --filter "name=^$name$" | grep -q .; then
    echo "emulator exited during boot:" >&2
    docker logs "$name" 2>&1 | grep -v '^DEBUG' | tail -20 >&2
    exit 1
  fi
  if (( $(date +%s) - start > 400 )); then echo "boot timed out" >&2; exit 1; fi
  sleep 3
done
echo "==> booted in $(( $(date +%s) - start ))s; adb serial $("$adb" devices | awk '/^emulator-/ { print $1; exit }')"
echo "    stop with: $0 stop   (restart between test batches; see docs/references/android-emulator.md)"
