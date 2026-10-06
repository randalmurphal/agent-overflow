package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/testutil/mockexec"
)

// The smoke clears app data and changes an emulator PIN. Attaching a Pixel
// must never make it the implicit target instead of the emulator.
func TestAndroidSmokeSelectsOnlyAnExplicitPhone(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
	script, err := os.ReadFile("e2e/scripts/android-smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, devices, serial, human, release, want string }{
		{"phone alone", "pixel device", "", "", "", "Select one test device"},
		{"explicit phone without human", "pixel device", "pixel", "", "", "A real phone requires"},
		{"unknown serial", "pixel device", "missing", "1", "", "does not name an attached"},
		{"two emulators", "emulator-5554 device\nemulator-5556 device", "", "", "", "Select one test device"},
		{"phone beside emulator", "pixel device\nemulator-5554 device", "", "", "", "==> device emulator-5554"},
		{"explicit phone with human", "pixel device", "pixel", "1", "", "==> device pixel"},
		{"release needs emulator", "", "", "", "/candidate.apk", "requires an attached emulator"},
		{"release refuses phone", "pixel device", "pixel", "1", "/candidate.apk", "requires an emulator"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			// The script runs from a copy whose repository holds no APK, so a
			// selected device stops at the missing APK. Run in place, it would
			// go on to restamp frontend/dist and build bin/ao-android-harness
			// whenever a debug APK has been built.
			smoke := filepath.Join(dir, "repo", "e2e", "scripts", "android-smoke.sh")
			if err := os.MkdirAll(filepath.Dir(smoke), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(smoke, script, 0o600); err != nil {
				t.Fatal(err)
			}
			// Refuse every mutation, even in the successful selection cases:
			// this fixture must never reach the actual Playwright launcher.
			mockexec.WriteIn(t, filepath.Join(dir, "platform-tools"), "adb", `#!/usr/bin/env bash
if [[ "$1" == devices ]]; then
  printf 'List of devices attached\n%s\n' "$AO_TEST_ADB_DEVICES"
elif [[ "$*" == *'getprop ro.kernel.qemu' ]]; then
  [[ "$2" == emulator-* ]] && echo 1 || echo 0
else
  echo 'test adb refuses mutations' >&2
  exit 73
fi
`)
			cmd := exec.Command("bash", smoke)
			cmd.Env = append(os.Environ(),
				"ANDROID_HOME="+dir,
				"AO_ANDROID_SERIAL="+tc.serial,
				"AO_ANDROID_HUMAN_LOCK="+tc.human,
				"AO_ANDROID_RELEASE_APK="+tc.release,
				"AO_TEST_ADB_DEVICES="+tc.devices,
			)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatal("smoke reached the real launcher")
			}
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("want %q, got %s", tc.want, out)
			}
		})
	}
}
