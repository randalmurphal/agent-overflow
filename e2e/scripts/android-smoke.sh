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
# the spot is a check people learn to skip. So a host without the SDK, the
# AVD or KVM prints how to set one up and exits 0. What the run costs when
# it IS available is a few minutes, which is why it is its own target
# rather than part of `make e2e`.
#
# WHERE IT RUNS. The emulator, the adb server, the harness backends and
# Playwright share one test network namespace (bin/ao-netns: loopback plus
# a private LAN, no route off it), so nothing the run starts listens on the
# host. The script does its host work first (checks, the release fixture,
# the emulator's runtime files), then runs itself again inside the
# namespace for everything that talks to the device. A real phone
# (AO_ANDROID_HUMAN_LOCK=1) needs the host's adb and so runs on the host
# network: the one exception, and opt-in.
#
# WHAT THIS SCRIPT OWNS, and why each step is here rather than in the spec:
#
#   - The emulator's lifetime: started for the run and stopped after it,
#     including on failure.
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
: "${ANDROID_AVD_HOME:=$HOME/.android/avd}"
export ANDROID_HOME ANDROID_AVD_HOME
adb="$ANDROID_HOME/platform-tools/adb"
pin="1234"
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
apk="${AO_ANDROID_RELEASE_APK:-$repo/mobile/android/app/build/outputs/apk/debug/app-debug.apk}"
emulator="$repo/e2e/scripts/android-emulator.sh"
human="${AO_ANDROID_HUMAN_LOCK:-}"
lock="${AO_EMULATOR_LOCK:-/run/user/$(id -u)/ao-emulator.lock}"

# A device line is "<serial>\t<state>"; only `device` is ready. Anything
# else (offline, unauthorized, a booting emulator) is not something to run
# a suite against, and reading it as one is how a flake gets blamed on the
# app.
ready_devices() { "$adb" devices | tail -n +2 | awk '$2 == "device" { print $1 }'; }

# The emulator path holds android-emulator.sh's lock for the whole run, as
# fd 9: taken by the host phase and inherited across bin/ao-netns, or taken
# here when the device phase was started directly. Only the holder may
# start or stop the emulator.
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

# A background job starts as this shell's fork. Until it runs its command
# it holds this script's signal handlers, and a signal sent to it then is
# lost: the command runs anyway. So each job is started through
# START_MARKED, which marks it in $started once it is a process of its own,
# and stop_jobs signals a job only when it is marked or gone. From the mark
# on, TERM either ends the job before its command starts or reaches the
# command.
START_MARKED='touch "$0/$$" && exec "$@"'

# Stops this script's background jobs and waits for them.
stop_jobs() {
  local running job
  running="$(jobs -p)"
  [[ -n "$running" ]] || return 0
  for job in $running; do
    while kill -0 "$job" 2>/dev/null && [[ ! -e "$started/$job" ]]; do
      sleep 0.02
    done
    kill -TERM "$job" 2>/dev/null || true
  done
  wait $running 2>/dev/null || true
}
started="$(mktemp -d)"

# Long steps run as supervised children: a signal is passed on to the
# child and waited out before the exit cleanup runs, so nothing the step
# started outlives the PIN, the adb server or the emulator it relies on.
# Background children of a script ignore SIGINT, so TERM carries them all.
# The child is found with `jobs -p`, not `$!`: a trap can run after the fork
# and before `$!` is read.
supervise() {
  "$BASH" -c "$START_MARKED" "$started" "$@" &
  local child=$! status=0
  wait "$child" || status=$?
  # A later child can be given this one's process id.
  rm -f "$started/$child"
  return "$status"
}
on_signal() {
  # A second signal must not cut the stop or the cleanup short.
  trap '' INT TERM HUP
  stop_jobs
  exit $((128 + $(kill -l "$1")))
}
trap 'on_signal INT' INT
trap 'on_signal TERM' TERM
trap 'on_signal HUP' HUP

# What the run acquired, released on every exit path: the PIN before the
# emulator stops, since it boots from the same AVD next time.
pinned=""
own_emulator=""
on_exit() {
  # A second signal must not cut the cleanup short.
  trap '' INT TERM HUP
  if [[ -n "$pinned" ]]; then
    "$adb" -s "$pinned" shell locksettings clear --old "$pin" >/dev/null 2>&1 || true
  fi
  if [[ -n "$own_emulator" ]]; then
    "$adb" kill-server >/dev/null 2>&1 || true
    "$emulator" stop >/dev/null || true
  fi
  rm -rf "$started"
}
trap on_exit EXIT

# A real phone is named, never chosen: every case clears the app's data.
human_serial() {
  local serial="${AO_ANDROID_SERIAL:-}"
  if [[ -z "$serial" ]] || ! ready_devices | awk -v wanted="$serial" '$0 == wanted { found=1 } END { exit !found }'; then
    echo "AO_ANDROID_HUMAN_LOCK=1 needs AO_ANDROID_SERIAL naming an attached, ready phone" >&2
    exit 1
  fi
  echo "$serial"
}

# ---- the device phase: in the namespace, or on the host for a phone ----

device_phase() {
  local serial
  if [[ "$human" == 1 ]]; then
    serial="$(human_serial)"
  else
    "$emulator" inside || {
      echo "the emulator phase runs inside the test network namespace; run make e2e-android" >&2
      exit 1
    }
    hold_lock
    if systemctl --user is-active -q ao-emulator.scope; then
      echo "an emulator is already running as ao-emulator.scope, outside this run; stop it first: $emulator stop" >&2
      exit 1
    fi
    # Nothing else can start one while this run holds the lock, so the
    # emulator and this namespace's adb server are the run's own from here.
    own_emulator=1
    supervise "$emulator" start
    serial="$(ready_devices | awk '/^emulator-/ { print; exit }')"
    if [[ -z "$serial" ]]; then
      echo "the emulator booted but adb lists no ready emulator" >&2
      exit 1
    fi
  fi
  echo "==> device $serial"

  echo "==> installing"
  if ! install_output="$("$adb" -s "$serial" install -r "$apk" 2>&1)"; then
    # Release and debug signatures differ. Only the disposable emulator may
    # replace that installation; every case clears its app data anyway.
    if [[ "$install_output" != *INSTALL_FAILED_UPDATE_INCOMPATIBLE* || "$("$adb" -s "$serial" shell getprop ro.kernel.qemu | tr -d '\r')" != 1 ]]; then
      echo "$install_output" >&2
      exit 1
    fi
    "$adb" -s "$serial" uninstall dev.agentoverflow.app
    "$adb" -s "$serial" install "$apk"
  fi

  # The app's data, the notification permission and the launch itself are
  # the spec's `page` fixture's to do, before EVERY case: a phone that has
  # never paired is each case's starting point, not the run's.

  # The credential the app lock falls back to, cleared on every exit path.
  #
  # Unless the device is a REAL phone: it already has its owner's own
  # credential, `locksettings set-pin` over one is refused without the old
  # credential, and typing `1234` at the owner's real prompt would be
  # wrong-PIN attempts Android escalates into a lockout. Under
  # AO_ANDROID_HUMAN_LOCK=1 provisioning is skipped and the spec waits for
  # the owner to answer each prompt themselves.
  if [[ "$human" == 1 ]]; then
    echo "==> AO_ANDROID_HUMAN_LOCK=1 — the owner answers the lock prompts"
  else
    echo "==> setting the device PIN"
    pinned="$serial"
    "$adb" -s "$serial" shell locksettings set-pin "$pin"
  fi

  # The launcher rather than a bare `pnpm exec playwright test`, for two
  # reasons that are the same two every other suite here has: it typechecks
  # the whole e2e tree first (Playwright only STRIPS types, so a typo'd
  # property in this spec would pass an assertion vacuously), and it runs
  # the suite under one owned process tree and memory boundary. Its extra
  # arguments are forwarded verbatim, so the command it actually runs is
  # `pnpm exec playwright test --config=playwright.android.config.ts` with
  # `e2e/` as its working directory. It is also what lets the spec call
  # `launchHarness`, which refuses to spawn a backend outside it. Started
  # in the namespace, the launcher runs the suite there, beside the
  # emulator and adb; a phone's run needs the host's adb.
  local network=()
  if [[ "$human" == 1 ]]; then network=(--host-network); fi
  echo "==> the shell smoke, in the device's WebView"
  cd "$repo"
  supervise env AO_ANDROID_SERIAL="$serial" AO_HARNESS_BIN="$repo/bin/ao-android-harness" \
    bin/ao-harness-e2e "${network[@]}" --config=playwright.android.config.ts "$@"
}

if [[ "${AO_ANDROID_SMOKE_PHASE:-}" == device ]]; then
  device_phase "$@"
  exit 0
fi

# ---- the host phase ----

setup_hint() {
  # The system image must match the host: the emulator runs an arm64
  # guest on Apple Silicon and an x86_64 one everywhere else, and the
  # other one either fails to install or boots at a crawl.
  local abi=x86_64
  case "$(uname -m)" in arm64 | aarch64) abi=arm64-v8a ;; esac
  cat <<MSG
$1, so the shell smoke is skipped.

To run it:
  $ANDROID_HOME/cmdline-tools/latest/bin/sdkmanager "system-images;android-36;google_apis;$abi"
  $ANDROID_HOME/cmdline-tools/latest/bin/avdmanager create avd -n ao -k "system-images;android-36;google_apis;$abi"
  make apk && make e2e-android

The run starts and stops the emulator itself, inside the test network
namespace and under a memory cap (docs/references/android-emulator.md).
MSG
}

skip() {
  if [[ -n "${AO_ANDROID_RELEASE_APK:-}" ]]; then
    echo "$1; explicit release validation requires the emulator." >&2
    exit 1
  fi
  setup_hint "$1"
  exit 0
}

[[ -x "$adb" ]] || skip "No adb at $adb"
if [[ "$human" == 1 ]]; then
  if [[ -n "${AO_ANDROID_RELEASE_APK:-}" ]]; then
    echo "The release lifecycle test changes network radios and requires the emulator." >&2
    exit 1
  fi
  phone="$(human_serial)"
  echo "==> phone $phone"
else
  [[ -x "$ANDROID_HOME/emulator/emulator" ]] || skip "No emulator in $ANDROID_HOME"
  [[ -d "$ANDROID_AVD_HOME/${AO_EMULATOR_AVD:-ao}.avd" ]] || skip "No AVD '${AO_EMULATOR_AVD:-ao}' under $ANDROID_AVD_HOME"
  [[ -e /dev/kvm ]] || skip "No /dev/kvm on this host"
fi

if [[ ! -f "$apk" ]]; then
  echo "no APK at $apk — build the requested APK first" >&2
  exit 1
fi
if [[ -n "${AO_ANDROID_RELEASE_APK:-}" ]]; then
  export JAVA_HOME="${JAVA_HOME:-$HOME/.jdks/temurin-21}"
  # apksigner runs `java` from PATH, not JAVA_HOME.
  export PATH="$JAVA_HOME/bin:$PATH"
  "$ANDROID_HOME/build-tools/36.0.0/apksigner" verify "$apk"
  manifest="$("$ANDROID_HOME/build-tools/36.0.0/aapt" dump badging "$apk")"
  if [[ "$manifest" != "package: name='dev.agentoverflow.app'"* ]]; then
    echo "AO_ANDROID_RELEASE_APK must be an Agent Overflow APK." >&2
    exit 1
  fi
  if [[ "$manifest" == *application-debuggable* ]]; then
    echo "AO_ANDROID_RELEASE_APK must be a non-debuggable signed release APK." >&2
    exit 1
  fi
  # Capacitor can override the manifest default; inspect the artifact's config,
  # not the source tree that may have changed since the candidate was built.
  unzip -p "$apk" assets/capacitor.config.json | node -e '
    let input = "";
    process.stdin.on("data", chunk => input += chunk);
    process.stdin.on("end", () => {
      const value = JSON.parse(input).android?.webContentsDebuggingEnabled;
      if (value !== undefined && value !== false) {
        throw new Error("The release APK must not enable WebView debugging.");
      }
    });
  '
fi

# Only this test runner builds/uses an explicitly newer release fixture.
# The ordinary harness binary and production dist retain their real versions.
supervise node "$repo/e2e/scripts/build-android-bundle-fixture.ts" "$apk"

if [[ "$human" == 1 ]]; then
  device_phase "$@"
  exit 0
fi

# Native input selectors need Playwright's small Android driver, and the
# emulator its runtime files: both fetched here, since the namespace has no
# route off the machine. Cached after the first run. The lock taken here is
# held through the device phase, which inherits it as fd 9.
supervise pnpm --dir "$repo/e2e" exec playwright install android
hold_lock
supervise "$emulator" prepare
# The device phase is another process with marks of its own, and exec runs
# no exit cleanup.
rm -rf "$started"
exec env AO_ANDROID_SMOKE_PHASE=device "$repo/bin/ao-netns" "$repo/e2e/scripts/android-smoke.sh" "$@"
