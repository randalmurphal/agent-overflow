#!/usr/bin/env bash
# The Android emulator for `make e2e-android`, inside the test network
# namespace and under a hard memory cap. Read docs/references/android-emulator.md
# before changing the launch: emulator 37.1 leaks multi-gigabyte host
# allocations from its adb proxy on every Playwright session, and an uncapped
# emulator has taken the whole WSL VM down with it. The cap is the
# containment, not a tuning knob.
#
#   e2e/scripts/android-emulator.sh prepare   # on the host: runtime files
#   e2e/scripts/android-emulator.sh start     # in the namespace: boot AVD `ao`
#   e2e/scripts/android-emulator.sh stop
#
# The emulator, its console and adb ports, and the adb server all live in
# the namespace bin/ao-netns makes (loopback plus a private LAN, no route
# off it), so nothing listens on the host. android-smoke.sh runs start and
# stop there; start refuses anywhere else.
#
# The SDK's emulator needs libraries a host may lack, so `prepare` builds
# e2e/android/emulator.Dockerfile and exports its files once; start runs
# the SDK's binary on them with bubblewrap. A container runtime cannot run
# inside the namespace (rootless Podman rejoins its own user namespace), and
# the namespace must hold the emulator for adb and the suite to reach it.
#
#   AO_EMULATOR_AVD      AVD name (default ao)
#   AO_EMULATOR_MEMORY   memory cap (default 10G; the leak needs 3-4 GB of
#                        headroom per pairing on top of a ~4 GB idle guest)
#   AO_EMULATOR_ARGS     extra emulator flags
#
# Linux and WSL2 only: it needs /dev/kvm, bubblewrap, a user systemd with
# the memory controller delegated, and Docker or Podman for `prepare`.
set -euo pipefail

: "${ANDROID_HOME:=$HOME/Android/Sdk}"
: "${ANDROID_AVD_HOME:=$HOME/.android/avd}"
avd="${AO_EMULATOR_AVD:-ao}"
memory="${AO_EMULATOR_MEMORY:-10G}"
unit="ao-emulator.scope"
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
adb="$ANDROID_HOME/platform-tools/adb"
cache="${XDG_CACHE_HOME:-$HOME/.cache}/agent-overflow"
rootfs="$cache/emulator-rootfs"
log="$cache/emulator.log"

# The namespace bin/ao-netns makes has loopback and lan0 and no default
# route; the host has more. netisolate.Contained judges the same way.
contained() {
  [[ "$(ip -o link show | awk -F': ' '{ print $2 }' | sort | tr '\n' ' ')" == "lan0 lo " ]] &&
    [[ -z "$(ip route show default)" ]]
}

case "${1:-}" in
  prepare)
    for need in docker bwrap systemd-run; do
      command -v "$need" >/dev/null 2>&1 || { echo "missing $need" >&2; exit 1; }
    done
    docker build -q -t ao-emulator -f "$repo/e2e/android/emulator.Dockerfile" "$repo/e2e/android" >/dev/null
    image="$(docker image inspect -f '{{.Id}}' ao-emulator)"
    if [[ "$(cat "$rootfs/.ao-image" 2>/dev/null)" != "$image" ]]; then
      rm -rf "$rootfs"
      mkdir -p "$rootfs"
      container="$(docker create ao-emulator true)"
      trap 'docker rm "$container" >/dev/null' EXIT
      docker export "$container" | tar -x -C "$rootfs"
      echo "$image" >"$rootfs/.ao-image"
    fi
    echo "==> emulator runtime ready in $rootfs"
    exit 0
    ;;
  stop)
    systemctl --user stop "$unit" >/dev/null 2>&1 || true
    rm -f "$ANDROID_AVD_HOME/$avd.avd"/*.lock
    echo "==> emulator stopped"
    exit 0
    ;;
  start) ;;
  *) echo "usage: $0 prepare|start|stop" >&2; exit 2 ;;
esac

contained || {
  echo "start the emulator inside the test network namespace (bin/ao-netns), as android-smoke.sh does; outside it, its ports and adb's would listen on the host" >&2
  exit 1
}
for need in bwrap systemd-run "$adb" "$ANDROID_HOME/emulator/emulator"; do
  command -v "$need" >/dev/null 2>&1 || [[ -x "$need" ]] || { echo "missing $need" >&2; exit 1; }
done
[[ -e /dev/kvm ]] || { echo "/dev/kvm is required; enable nested virtualization for this VM" >&2; exit 1; }
[[ -f "$rootfs/.ao-image" ]] || { echo "no emulator runtime in $rootfs; run: $0 prepare" >&2; exit 1; }
[[ -d "$ANDROID_AVD_HOME/$avd.avd" ]] || {
  echo "no AVD '$avd' under $ANDROID_AVD_HOME; create it with avdmanager first (make e2e-android prints the commands)" >&2
  exit 1
}
if systemctl --user is-active -q "$unit"; then
  echo "an emulator is already running as $unit; stop it first: $0 stop" >&2
  exit 1
fi
systemctl --user reset-failed "$unit" >/dev/null 2>&1 || true
# A killed emulator leaves AVD lock files that refuse the next launch.
rm -f "$ANDROID_AVD_HOME/$avd.avd"/*.lock

# The scope caps memory with no swap and four CPUs, as the container did;
# the process stays in this namespace. HOME is a tmpfs except the AVD
# folder, so the emulator writes nothing else of the developer's.
mkdir -p "$cache"
read -ra extra <<<"${AO_EMULATOR_ARGS:-}"
systemd-run --user --scope --unit="$unit" -q \
  -p MemoryMax="$memory" -p MemorySwapMax=0 -p CPUQuota=400% \
  bwrap --bind "$rootfs" / --dev /dev --dev-bind /dev/kvm /dev/kvm --proc /proc \
    --ro-bind /sys /sys --tmpfs /tmp --tmpfs "$HOME" \
    --ro-bind "$ANDROID_HOME" "$ANDROID_HOME" --bind "$HOME/.android" "$HOME/.android" \
    --setenv HOME "$HOME" --setenv ANDROID_HOME "$ANDROID_HOME" --setenv ANDROID_SDK_ROOT "$ANDROID_HOME" \
    --setenv ANDROID_AVD_HOME "$ANDROID_AVD_HOME" \
    "$ANDROID_HOME/emulator/emulator" -avd "$avd" -no-window -no-audio -no-boot-anim \
    -gpu swiftshader_indirect -no-snapshot -memory 3072 -cores 2 "${extra[@]}" \
  >"$log" 2>&1 &

echo "==> booting $avd as $unit (memory cap $memory)"
start=$(date +%s)
until [[ "$("$adb" shell getprop sys.boot_completed 2>/dev/null | tr -d '\r')" == 1 ]]; do
  if (( $(date +%s) - start > 5 )) && ! systemctl --user is-active -q "$unit"; then
    echo "emulator exited during boot:" >&2
    grep -v '^DEBUG' "$log" | tail -20 >&2
    exit 1
  fi
  if (( $(date +%s) - start > 400 )); then echo "boot timed out" >&2; exit 1; fi
  sleep 3
done
echo "==> booted in $(( $(date +%s) - start ))s; adb serial $("$adb" devices | awk '/^emulator-/ { print $1; exit }')"
