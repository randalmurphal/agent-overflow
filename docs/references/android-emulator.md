# Android emulator

How to run the Android Emulator for `make e2e-android` without losing the
host, and what was established about the emulator's behavior on the way.
Launch through `e2e/scripts/android-emulator.sh`; do not start the emulator
bare on a shared machine.

## The adb proxy leak

Android Emulator 37.1.11 (the newest build in the stable and beta channels
as of September 2026) with the `android-36;google_apis` image leaks host
memory from its own adb proxy. The stack, captured with gdb on the emulator
process, is `android::emulation::AdbHub::readSocket` calling `new[]` with a
length read from a stream it has just logged as
`HOST==>GUEST Received invalid packet`. Each occurrence allocates 1.3 to
3.4 GB, tcmalloc reports it as `large alloc`, and the buffer is never
freed. It fired on most Playwright Android sessions, usually while the
shell WebView paired and mounted, and it stacks per emulator lifetime.
Uncapped, one afternoon of smoke runs reached 30 GB and the kernel OOM
killer took the WSL VM down along with the developer's app backend.

What does not help, all verified:

- Renderer choice. It reproduces with `-gpu swiftshader_indirect`,
  `-gpu off`, `-feature -Vulkan`, and with window blur disabled.
- Disabling the vsock transport. `-feature -VirtioVsockPipe` leaves adbd
  offline on the API 36 image and the boot never completes.
- Waiting for a fix. No newer emulator was available; check
  `sdkmanager --list` before assuming this still holds.

What contains it: a hard cgroup memory cap around the emulator and a
restart between test batches. `e2e/scripts/android-emulator.sh` runs the
emulator inside a Docker container with `--memory` set (10 GB by default;
the idle guest plus renderer sits near 4 GB and each pairing can add
3 to 4 GB). The worst case is then the container dying with OOM, which
the smoke reports as a closed device, not the host. Keep `earlyoom` or an
equivalent host-level guard as the second line; it is not a substitute
for the cap.

## Running it

Prerequisites on Linux or WSL2: Docker usable by the developer, `/dev/kvm`
present, the SDK under `$ANDROID_HOME` with `emulator`, `platform-tools`,
`platforms;android-36`, `build-tools;36.0.0` and the system image, and an
AVD created with `avdmanager` (the smoke's no-device message prints the
commands). No `sudo` is needed: the launcher adds the container to the
`kvm` group by gid and mounts the SDK read-only.

```
e2e/scripts/android-emulator.sh start
make apk && make e2e-android
e2e/scripts/android-emulator.sh stop
```

Boot takes about 50 s. `stop` also removes the AVD lock files a killed
emulator leaves behind; a launch that fails immediately after a crash is
usually those locks.

Other platforms have no container recipe here. Run the emulator under the
memory limit the platform offers, watch its resident size across a run,
and restart it after a few pairings.

## Driving the WebView from a spec

Findings from `e2e/android/shell-boot.spec.ts` work that are easy to lose:

- The shell WebView is not edge to edge. Page coordinates need the status
  bar inset added before they become screen pixels for `input swipe`;
  read it from `dumpsys window` (`statusBars ... frame=[0,0][w,h]`).
- The PIN prompt leaves the soft keyboard up. It shrinks the viewport and
  the composer then covers content near the bottom. Blur the focused
  element and check `dumpsys input_method` for `mInputShown` before
  measuring geometry; send Back only while the IME reports shown, since a
  second Back navigates the app.
- A touch that lands during a fling stops the fling instead of tapping.
  Wait for scroll positions to settle before tapping after a swipe.
- Elements are visible to Playwright while off screen inside the timeline.
  Scroll them into view and assert their box is inside `innerHeight`
  before touching them.
