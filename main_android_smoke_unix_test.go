//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/netisolate"
	"agent-overflow/internal/testutil/mockexec"
)

// smokeFixture is a copy of the Android smoke scripts in a repository that
// holds no APK, beside a fake SDK, so no case can restamp frontend/dist,
// build bin/ao-android-harness or reach a real device, emulator or launcher.
type smokeFixture struct {
	dir, smoke, log string
	env             []string
}

func newSmokeFixture(t *testing.T) smokeFixture {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
	dir := t.TempDir()
	scripts := filepath.Join(dir, "repo", "e2e", "scripts")
	if err := os.MkdirAll(scripts, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"android-smoke.sh", "android-emulator.sh"} {
		body, err := os.ReadFile(filepath.Join("e2e", "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scripts, name), body, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f := smokeFixture{dir: dir, smoke: filepath.Join(scripts, "android-smoke.sh"), log: filepath.Join(dir, "calls.log")}
	// The fake adb lists $AO_TEST_ADB_DEVICES, answers the emulator probe,
	// and records every call; installs and PIN changes succeed silently.
	mockexec.WriteIn(t, filepath.Join(dir, "platform-tools"), "adb", `#!/usr/bin/env bash
echo "adb $*" >>"$AO_TEST_LOG"
if [[ "$1" == devices ]]; then
  printf 'List of devices attached\n%s\n' "$AO_TEST_ADB_DEVICES"
elif [[ "$*" == *'getprop ro.kernel.qemu' ]]; then
  [[ "$2" == emulator-* ]] && echo 1 || echo 0
fi
`)
	// systemctl stands in for the user manager: the emulator scope is
	// active only when a case says so.
	bin := filepath.Join(dir, "bin")
	mockexec.WriteIn(t, bin, "systemctl", `#!/usr/bin/env bash
echo "systemctl $*" >>"$AO_TEST_LOG"
if [[ "$*" == *is-active* ]]; then [[ "${AO_TEST_SCOPE_ACTIVE:-}" == 1 ]]; fi
`)
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name == "PATH" || name == "XDG_CACHE_HOME" || name == netisolate.EnvNamespace || strings.HasPrefix(name, "AO_ANDROID_") || strings.HasPrefix(name, "AO_EMULATOR_") {
			continue
		}
		f.env = append(f.env, kv)
	}
	f.env = append(f.env,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ANDROID_HOME="+dir,
		"ANDROID_AVD_HOME="+filepath.Join(dir, "avd"),
		"XDG_CACHE_HOME="+filepath.Join(dir, "cache"),
		"AO_TEST_LOG="+f.log,
	)
	return f
}

// fakeEmulator replaces android-emulator.sh with a recorder that reports
// itself inside the namespace, so the device phase runs without one.
func (f smokeFixture) fakeEmulator(t *testing.T) {
	t.Helper()
	mockexec.Write(t, filepath.Join(filepath.Dir(f.smoke), "android-emulator.sh"), `#!/usr/bin/env bash
echo "emulator $1" >>"$AO_TEST_LOG"
`)
}

func (f smokeFixture) command(env ...string) *exec.Cmd {
	cmd := exec.Command("bash", f.smoke)
	cmd.Env = append(append([]string{}, f.env...), env...)
	return cmd
}

func (f smokeFixture) calls(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(f.log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(body)
}

// The smoke clears app data and changes an emulator PIN. A real phone is
// only ever the target when named under AO_ANDROID_HUMAN_LOCK=1, and the
// emulator run never addresses the host's devices at all: it boots its own
// inside the test network namespace.
func TestAndroidSmokeSelectsOnlyAnExplicitPhone(t *testing.T) {
	for _, tc := range []struct {
		name, devices, serial, human, release string
		emulatorSDK                           bool
		want                                  []string
	}{
		{name: "phone without a serial", devices: "pixel\tdevice", human: "1", want: []string{"needs AO_ANDROID_SERIAL"}},
		{name: "unknown serial", devices: "pixel\tdevice", serial: "missing", human: "1", want: []string{"needs AO_ANDROID_SERIAL"}},
		{name: "named phone", devices: "pixel\tdevice", serial: "pixel", human: "1", want: []string{"==> phone pixel", "no APK"}},
		{name: "release refuses phone", devices: "pixel\tdevice", serial: "pixel", human: "1", release: "/candidate.apk", want: []string{"requires the emulator"}},
		{name: "release needs emulator", release: "/candidate.apk", want: []string{"explicit release validation requires the emulator"}},
		{name: "phone beside the emulator run", devices: "pixel\tdevice", emulatorSDK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newSmokeFixture(t)
			if tc.emulatorSDK {
				mockexec.WriteIn(t, filepath.Join(f.dir, "emulator"), "emulator", "#!/bin/sh\nexit 0\n")
				if err := os.MkdirAll(filepath.Join(f.dir, "avd", "ao.avd"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			out, err := f.command(
				"AO_ANDROID_SERIAL="+tc.serial,
				"AO_ANDROID_HUMAN_LOCK="+tc.human,
				"AO_ANDROID_RELEASE_APK="+tc.release,
				"AO_TEST_ADB_DEVICES="+tc.devices,
			).CombinedOutput()
			if tc.emulatorSDK {
				// Without KVM the run skips; with it, it stops at the missing
				// APK. Either way it never addressed the attached phone.
				if !strings.Contains(string(out), "is skipped") && !strings.Contains(string(out), "no APK") {
					t.Fatalf("want a skip or the missing APK, got %v: %s", err, out)
				}
				if calls := f.calls(t); strings.Contains(calls, "pixel") || strings.Contains(calls, "adb -s") {
					t.Fatalf("the emulator run addressed a host device:\n%s", calls)
				}
				return
			}
			if err == nil {
				t.Fatalf("smoke passed its checks with no APK: %s", out)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(out), want) {
					t.Fatalf("want %q, got %s", want, out)
				}
			}
		})
	}
}

// The emulator's guard and the launcher's must agree on what is inside the
// test network namespace: the namespace's identity in AO_NETISOLATE_NS, not
// an interface set a host could happen to have.
func TestAndroidEmulatorInsideAgreesWithNetisolate(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the test network namespace is Linux-only")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("ip unavailable")
	}
	f := newSmokeFixture(t)
	emulator := filepath.Join(filepath.Dir(f.smoke), "android-emulator.sh")
	own := os.Getenv(netisolate.EnvNamespace)
	for _, tc := range []struct {
		name, value string
		want        bool
	}{
		{"unset", "", false},
		{"another namespace", "net:[1]", false},
		{"inherited", own, netisolate.Inside()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", emulator, "inside")
			cmd.Env = append(append([]string{}, f.env...), netisolate.EnvNamespace+"="+tc.value)
			err := cmd.Run()
			var exitErr *exec.ExitError
			if err != nil && !errors.As(err, &exitErr) {
				t.Fatal(err)
			}
			if got := err == nil; got != tc.want {
				t.Fatalf("android-emulator.sh inside = %v, want %v", got, tc.want)
			}
		})
	}
}

// Outside the namespace the device phase stops before it takes the lock or
// touches the emulator, so it can neither listen on the host nor stop an
// emulator another run owns.
func TestAndroidSmokeDevicePhaseRefusesTheHostNetwork(t *testing.T) {
	f := newSmokeFixture(t)
	out, err := f.command("AO_ANDROID_SMOKE_PHASE=device").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "runs inside the test network namespace") {
		t.Fatalf("want the namespace refusal, got %v: %s", err, out)
	}
	if calls := f.calls(t); calls != "" {
		t.Fatalf("refused phase still acted:\n%s", calls)
	}
}

// A run that finds the emulator scope or the lock already taken refuses
// without claiming the emulator, so its exit cleanup cannot stop the scope
// the other run owns.
func TestAndroidSmokeLeavesAnotherRunsEmulator(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the emulator path is Linux-only")
	}
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock unavailable")
	}
	t.Run("active scope", func(t *testing.T) {
		f := newSmokeFixture(t)
		f.fakeEmulator(t)
		out, err := f.command("AO_ANDROID_SMOKE_PHASE=device", "AO_TEST_SCOPE_ACTIVE=1").CombinedOutput()
		if err == nil || !strings.Contains(string(out), "already running") {
			t.Fatalf("want the running-scope refusal, got %v: %s", err, out)
		}
		if calls := f.calls(t); strings.Contains(calls, "emulator start") || strings.Contains(calls, "emulator stop") || strings.Contains(calls, "kill-server") {
			t.Fatalf("a refused run touched the other run's emulator:\n%s", calls)
		}
	})
	t.Run("held lock", func(t *testing.T) {
		f := newSmokeFixture(t)
		f.fakeEmulator(t)
		lockPath := filepath.Join(f.dir, "cache", "agent-overflow", "android-emulator.lock")
		if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
			t.Fatal(err)
		}
		held, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = held.Close() })
		if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		out, err := f.command("AO_ANDROID_SMOKE_PHASE=device").CombinedOutput()
		if err == nil || !strings.Contains(string(out), "holds "+lockPath) {
			t.Fatalf("want the lock refusal, got %v: %s", err, out)
		}
		if calls := f.calls(t); strings.Contains(calls, "emulator start") || strings.Contains(calls, "emulator stop") || strings.Contains(calls, "systemctl") {
			t.Fatalf("a refused run touched the emulator:\n%s", calls)
		}
	})
}

// A signal reaches the suite and is waited out before the cleanup clears the
// PIN, stops adb and stops the emulator, in that order.
func TestAndroidSmokeCleansUpAfterTheSuiteOnSignal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the emulator path is Linux-only")
	}
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock unavailable")
	}
	f := newSmokeFixture(t)
	f.fakeEmulator(t)
	mockexec.WriteIn(t, filepath.Join(f.dir, "repo", "bin"), "ao-harness-e2e", `#!/usr/bin/env bash
trap 'echo "launcher TERM" >>"$AO_TEST_LOG"; exit 143' TERM
echo "launcher started" >>"$AO_TEST_LOG"
while :; do sleep 0.05; done
`)
	cmd := f.command("AO_ANDROID_SMOKE_PHASE=device", "AO_TEST_ADB_DEVICES=emulator-5554\tdevice")
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	// Its own process group, so a failed case can kill the fake suite too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(f.calls(t), "launcher started") {
		if time.Now().After(deadline) {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
			t.Fatalf("the suite never started:\n%s\n%s", out.String(), f.calls(t))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		t.Fatalf("the smoke did not finish after SIGTERM:\n%s\n%s", out.String(), f.calls(t))
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 143 {
		t.Fatalf("want exit 143 after SIGTERM, got %v:\n%s", err, out.String())
	}
	calls := f.calls(t)
	order := []string{
		"launcher TERM",
		"adb -s emulator-5554 shell locksettings clear --old 1234",
		"adb kill-server",
		"emulator stop",
	}
	at := -1
	for _, step := range order {
		next := strings.Index(calls, step)
		if next <= at {
			t.Fatalf("want %q after the steps before it, in:\n%s", step, calls)
		}
		at = next
	}
}
