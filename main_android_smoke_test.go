package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The smoke clears app data and changes an emulator PIN. Attaching a Pixel
// must never make it the implicit target instead of the emulator.
func TestAndroidSmokeSelectsOnlyAnExplicitPhone(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
	for _, tc := range []struct{ name, devices, serial, human, want string }{
		{"phone alone", "pixel device", "", "", "Select one test device"},
		{"explicit phone without human", "pixel device", "pixel", "", "A real phone requires"},
		{"unknown serial", "pixel device", "missing", "1", "does not name an attached"},
		{"two emulators", "emulator-5554 device\nemulator-5556 device", "", "", "Select one test device"},
		{"phone beside emulator", "pixel device\nemulator-5554 device", "", "", "==> device emulator-5554"},
		{"explicit phone with human", "pixel device", "pixel", "1", "==> device pixel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			platformTools := filepath.Join(dir, "platform-tools")
			if err := os.Mkdir(platformTools, 0700); err != nil {
				t.Fatal(err)
			}
			// Refuse every mutation, even in the successful selection cases:
			// this fixture must never reach the actual Playwright launcher.
			stub := `#!/usr/bin/env bash
if [[ "$1" == devices ]]; then
  printf 'List of devices attached\n%s\n' "$AO_TEST_ADB_DEVICES"
elif [[ "$*" == *'getprop ro.kernel.qemu' ]]; then
  [[ "$2" == emulator-* ]] && echo 1 || echo 0
else
  echo 'test adb refuses mutations' >&2
  exit 73
fi
`
			if err := os.WriteFile(filepath.Join(platformTools, "adb"), []byte(stub), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "e2e/scripts/android-smoke.sh")
			t.Setenv("ANDROID_HOME", dir)
			t.Setenv("AO_ANDROID_SERIAL", tc.serial)
			t.Setenv("AO_ANDROID_HUMAN_LOCK", tc.human)
			t.Setenv("AO_TEST_ADB_DEVICES", tc.devices)
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
