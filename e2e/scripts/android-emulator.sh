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
#   e2e/scripts/android-emulator.sh inside    # exit 0 inside the namespace
#
# The emulator, its console and adb ports, and the adb server all live in
# the namespace bin/ao-netns makes (loopback plus a private LAN, no route
# off it), so nothing listens on the host. android-smoke.sh runs start and
# stop there; start refuses anywhere else.
#
# The SDK's emulator needs libraries a host may lack, so `prepare` builds
# e2e/android/emulator.Dockerfile and exports its files, once for each
# version of that file; start runs
# the SDK's binary on them with bubblewrap. A container runtime cannot run
# inside the namespace (rootless Podman rejoins its own user namespace), and
# the namespace must hold the emulator for adb and the suite to reach it.
#
# prepare and start hold an exclusive lock and the emulator inherits it, so
# one emulator runs at a time and its runtime files are never replaced under
# it. android-smoke.sh holds the same lock for its whole run. The lock lives
# beside the user manager that owns ao-emulator.scope, so every run of this
# user contends for it whatever cache it uses.
#
#   AO_EMULATOR_AVD      AVD name (default ao)
#   AO_EMULATOR_MEMORY   memory cap (default 10G; the leak needs 3-4 GB of
#                        headroom per pairing on top of a ~4 GB idle guest)
#   AO_EMULATOR_ARGS     extra emulator flags
#   AO_EMULATOR_LOCK     lock file, for tests that fake the scope (default
#                        /run/user/$UID/ao-emulator.lock)
#
# Linux and WSL2 only: it needs /dev/kvm, bubblewrap, flock, a user systemd
# with the memory controller delegated, and Docker or Podman when `prepare`
# has to build the runtime.
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
lock="${AO_EMULATOR_LOCK:-/run/user/$(id -u)/ao-emulator.lock}"

# Inside the namespace bin/ao-netns built: its helper recorded that
# namespace's identity in AO_NETISOLATE_NS (netisolate.Inside), and the
# interfaces are its loopback and lan0 at 10.203.0.2 with no default route.
# The identity is what a lookalike host cannot have.
inside() {
  [[ -n "${AO_NETISOLATE_NS:-}" && "$(readlink /proc/self/ns/net)" == "$AO_NETISOLATE_NS" ]] || return 1
  [[ "$(ip -o link show | awk -F': ' '{ print $2 }' | sort | tr '\n' ' ')" == "lan0 lo " ]] || return 1
  ip -o addr show dev lan0 | awk '
    $3 == "inet" && $4 == "10.203.0.2/24" { addressed = 1; next }
    $3 == "inet6" && $4 ~ /^fe80:/ { next }
    { bad = 1 }
    END { exit !(addressed && !bad) }' || return 1
  [[ -z "$(ip route show default)" && -z "$(ip -6 route show default)" ]]
}

# The lock may arrive as fd 9 from android-smoke.sh, which holds it for the
# whole run; otherwise this command takes it. The emulator inherits fd 9, so
# a running emulator keeps holding it after start returns. fd 9 is matched
# to the lock file by identity, so a path spelled another way still matches.
hold_lock() {
  mkdir -p "$(dirname "$lock")"
  if [[ ! "/proc/$$/fd/9" -ef "$lock" ]]; then
    exec 9>>"$lock"
  fi
  flock -n 9 || {
    echo "another Android emulator or smoke run holds $lock" >&2
    exit 1
  }
}

case "${1:-}" in
  inside)
    inside
    exit
    ;;
  prepare)
    command -v flock >/dev/null 2>&1 || { echo "missing flock" >&2; exit 1; }
    hold_lock
    # The runtime is what the Dockerfile builds, so the Dockerfile's hash
    # names it. Files exported from an unchanged Dockerfile are reused
    # without the image, which a container prune may have removed.
    dockerfile="$repo/e2e/android/emulator.Dockerfile"
    runtime="$(sha256sum "$dockerfile" | cut -d' ' -f1)"
    if [[ "$(cat "$rootfs/.ao-runtime" 2>/dev/null)" != "$runtime" ]]; then
      command -v docker >/dev/null 2>&1 || { echo "missing docker" >&2; exit 1; }
      if systemctl --user is-active -q "$unit"; then
        echo "an emulator is running as $unit on the current runtime; stop it first: $0 stop" >&2
        exit 1
      fi
      docker build -q -t ao-emulator -f "$dockerfile" "$repo/e2e/android" >/dev/null
      # Extracted beside the old root and swapped in whole, so a failed
      # export leaves the previous runtime intact and stamped.
      fresh="$rootfs.new"
      rm -rf "$fresh" "$rootfs.old"
      mkdir -p "$fresh"
      container="$(docker create ao-emulator true)"
      trap 'docker rm "$container" >/dev/null' EXIT
      docker export "$container" | tar -x -C "$fresh"
      echo "$runtime" >"$fresh/.ao-runtime"
      if [[ -d "$rootfs" ]]; then mv "$rootfs" "$rootfs.old"; fi
      mv "$fresh" "$rootfs"
      rm -rf "$rootfs.old"
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
  *) echo "usage: $0 prepare|start|stop|inside" >&2; exit 2 ;;
esac

inside || {
  echo "start the emulator inside the test network namespace (bin/ao-netns), as android-smoke.sh does; outside it, its ports and adb's would listen on the host" >&2
  exit 1
}
for need in bwrap flock systemd-run "$adb" "$ANDROID_HOME/emulator/emulator"; do
  command -v "$need" >/dev/null 2>&1 || [[ -x "$need" ]] || { echo "missing $need" >&2; exit 1; }
done
[[ -e /dev/kvm ]] || { echo "/dev/kvm is required; enable nested virtualization for this VM" >&2; exit 1; }
[[ -f "$rootfs/.ao-runtime" ]] || { echo "no emulator runtime in $rootfs; run: $0 prepare" >&2; exit 1; }
[[ -d "$ANDROID_AVD_HOME/$avd.avd" ]] || {
  echo "no AVD '$avd' under $ANDROID_AVD_HOME; create it with avdmanager first (make e2e-android prints the commands)" >&2
  exit 1
}
hold_lock
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
read -ra extra <<<"${AO_EMULATOR_ARGS:-}"
# Until the boot is confirmed the launch is this command's to undo. A signal
# or a failed boot stops the launcher, before or after it made the scope,
# waits for it so it cannot make one later, then stops the scope. Holding
# the lock means no other run can have made one. The launcher is found with
# `jobs -p`, not `$!`: a trap can run after the fork and before `$!` is read.
booted=""
abandon() {
  [[ -z "$booted" ]] || return 0
  local launched
  launched="$(jobs -p)"
  if [[ -n "$launched" ]]; then
    kill -TERM $launched 2>/dev/null || true
    wait $launched 2>/dev/null || true
  fi
  systemctl --user stop "$unit" >/dev/null 2>&1 || true
}
trap abandon EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP
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
booted=1
echo "==> booted in $(( $(date +%s) - start ))s; adb serial $("$adb" devices | awk '/^emulator-/ { print $1; exit }')"
