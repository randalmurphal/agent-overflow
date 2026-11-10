//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
		"AO_EMULATOR_LOCK="+filepath.Join(dir, "lock", "ao-emulator.lock"),
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

// signalAfterFork arms the named script to send itself SIGTERM at the first
// command after it forks one matching the glob fork while its positional
// arguments match the glob args: the moment between a fork and reading `$!`,
// which no signal from outside can be timed to hit. It returns the BASH_ENV
// entry that arms it.
func (f smokeFixture) signalAfterFork(t *testing.T, script, fork, args string) string {
	t.Helper()
	path := filepath.Join(f.dir, "signal-after-fork.bash")
	body := `[[ $0 == */` + script + ` ]] || return 0
ao_fork=` + shellQuote(fork) + `
ao_args=` + shellQuote(args) + `
set -o functrace
trap 'if [[ -n ${ao_forked:-} ]]; then trap - DEBUG; kill -TERM $$; fi; [[ $BASH_COMMAND == $ao_fork && $* == $ao_args ]] && ao_forked=1' DEBUG
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return "BASH_ENV=" + path
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
		lockPath := filepath.Join(f.dir, "lock", "ao-emulator.lock")
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
	// A signal while the suite runs, and one that lands right after the
	// suite is forked, before the smoke could read its PID.
	for _, afterFork := range []bool{false, true} {
		name := "while the suite runs"
		if afterFork {
			name = "right after the suite is forked"
		}
		t.Run(name, func(t *testing.T) {
			f := newSmokeFixture(t)
			f.fakeEmulator(t)
			mockexec.WriteIn(t, filepath.Join(f.dir, "repo", "bin"), "ao-harness-e2e", `#!/usr/bin/env bash
trap 'echo "launcher TERM" >>"$AO_TEST_LOG"; exit 143' TERM
echo "launcher started" >>"$AO_TEST_LOG"
while :; do sleep 0.05; done
`)
			env := []string{"AO_ANDROID_SMOKE_PHASE=device", "AO_TEST_ADB_DEVICES=emulator-5554\tdevice"}
			if afterFork {
				env = append(env, f.signalAfterFork(t, "android-smoke.sh", `"$BASH" -c *`, "*ao-harness-e2e*"))
			}
			cmd := f.command(env...)
			var out strings.Builder
			cmd.Stdout, cmd.Stderr = &out, &out
			// Its own process group, so a failed case can kill the fake suite too.
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
			if !afterFork {
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
			// A suite the smoke failed to stop would go on logging after the
			// cleanup; give one that survived time to show itself.
			time.Sleep(time.Second)
			calls := f.calls(t)
			const cleared = "adb -s emulator-5554 shell locksettings clear --old 1234"
			order := []string{
				cleared,
				"adb kill-server",
				"emulator stop",
			}
			if !afterFork {
				// Stopped while running, the suite sees the TERM itself; just
				// after the fork it may die before its trap exists.
				order = append([]string{"launcher TERM"}, order...)
			}
			at := -1
			for _, step := range order {
				next := strings.Index(calls, step)
				if next <= at {
					t.Fatalf("want %q after the steps before it, in:\n%s", step, calls)
				}
				at = next
			}
			if last := strings.LastIndex(calls, "launcher "); last > strings.Index(calls, cleared) {
				t.Fatalf("the suite outlived the cleanup:\n%s", calls)
			}
		})
	}
}

// The lock the host phase holds reaches the device phase as fd 9, and is
// recognized by the file it is, not by how its path is spelled: a symlinked
// runtime directory must not make the run refuse its own lock.
func TestAndroidSmokeRecognizesItsInheritedLock(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the emulator path is Linux-only")
	}
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock unavailable")
	}
	f := newSmokeFixture(t)
	f.fakeEmulator(t)
	mockexec.WriteIn(t, filepath.Join(f.dir, "repo", "bin"), "ao-harness-e2e", "#!/bin/sh\nexit 0\n")
	real := filepath.Join(f.dir, "lock")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(f.dir, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(filepath.Join(real, "ao-emulator.lock"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	cmd := f.command(
		"AO_ANDROID_SMOKE_PHASE=device",
		"AO_TEST_ADB_DEVICES=emulator-5554\tdevice",
		"AO_EMULATOR_LOCK="+filepath.Join(alias, "ao-emulator.lock"),
	)
	// fd 9, as the host phase hands it across exec and bin/ao-netns.
	cmd.ExtraFiles = []*os.File{nil, nil, nil, nil, nil, nil, held}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the device phase refused the lock it inherited: %v\n%s", err, out)
	}
	if calls := f.calls(t); !strings.Contains(calls, "emulator start") || !strings.Contains(calls, "emulator stop") {
		t.Fatalf("the run did not start and stop its emulator:\n%s", calls)
	}
}

// A signal during boot undoes the launch whether or not the scope exists
// yet: the launcher is stopped and joined, so it cannot make the scope after
// the cleanup, and a scope it already made is stopped. Scope creation is
// held behind a barrier so each order is deterministic.
func TestAndroidEmulatorStartUndoesAnInterruptedLaunch(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the emulator path is Linux-only")
	}
	if !netisolate.Inside() {
		t.Skip("start runs only inside the test network namespace (make go-test)")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("start requires /dev/kvm")
	}
	for _, need := range []string{"flock", "pgrep"} {
		if _, err := exec.LookPath(need); err != nil {
			t.Skip(need + " unavailable")
		}
	}
	for _, tc := range []struct {
		name         string
		scopeCreated bool
		afterFork    bool
		// held keeps the launcher from marking itself started until the
		// signal has been waiting on it for seconds.
		held bool
		// died kills the held launcher while the signal waits on it.
		died bool
		// again sends a second signal while start waits for the mark.
		again bool
	}{
		{name: "before the scope exists"},
		{name: "after the scope exists", scopeCreated: true},
		// Before start could read the launcher's PID.
		{name: "right after the launcher is forked", afterFork: true},
		{name: "before the launcher has marked itself started", held: true},
		{name: "when the launcher dies before it has marked itself started", held: true, died: true},
		{name: "when a second signal arrives while it waits for the mark", held: true, again: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSmokeFixture(t)
			barrier := filepath.Join(f.dir, "barrier")
			scope := filepath.Join(f.dir, "scope")
			bin := filepath.Join(f.dir, "bin")
			mockexec.WriteIn(t, bin, "bwrap", "#!/bin/sh\nexit 0\n")
			// The launcher makes the scope only once the barrier exists, then
			// runs as the emulator would.
			mockexec.WriteIn(t, bin, "systemd-run", `#!/usr/bin/env bash
echo "launcher started" >>"$AO_TEST_LOG"
while [[ ! -e "$AO_TEST_BARRIER" ]]; do sleep 0.02; done
echo "scope created" >>"$AO_TEST_LOG"
touch "$AO_TEST_SCOPE"
exec sleep 300
`)
			mockexec.WriteIn(t, bin, "systemctl", `#!/usr/bin/env bash
case "$*" in
  *is-active*) [[ -e "$AO_TEST_SCOPE" ]] ;;
  *stop*) echo "scope stopped" >>"$AO_TEST_LOG"; rm -f "$AO_TEST_SCOPE" ;;
esac
`)
			mockexec.WriteIn(t, filepath.Join(f.dir, "emulator"), "emulator", "#!/bin/sh\nexit 0\n")
			hold := ""
			if tc.held {
				hold = f.holdStartMark(t, 1)
			}
			for _, dir := range []string{filepath.Join(f.dir, "avd", "ao.avd"), filepath.Join(f.dir, "cache", "agent-overflow", "emulator-rootfs")} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(f.dir, "cache", "agent-overflow", "emulator-rootfs", ".ao-runtime"), []byte("test\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", filepath.Join(filepath.Dir(f.smoke), "android-emulator.sh"), "start")
			cmd.Env = append(append([]string{}, f.env...),
				netisolate.EnvNamespace+"="+os.Getenv(netisolate.EnvNamespace),
				"AO_TEST_BARRIER="+barrier,
				"AO_TEST_SCOPE="+scope,
			)
			if tc.afterFork {
				cmd.Env = append(cmd.Env, f.signalAfterFork(t, "android-emulator.sh", `"$BASH" -c *systemd-run *`, "*"))
			}
			// A file, not a pipe: a killed launcher's held touch outlives it
			// and would keep a pipe open.
			output, err := os.Create(filepath.Join(f.dir, "start.out"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = output.Close() })
			printed := func() string {
				body, _ := os.ReadFile(output.Name())
				return string(body)
			}
			cmd.Stdout, cmd.Stderr = output, output
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
			waitFor := func(what string) {
				t.Helper()
				deadline := time.Now().Add(30 * time.Second)
				for !strings.Contains(f.calls(t), what) {
					if time.Now().After(deadline) {
						t.Fatalf("never saw %q:\n%s\n%s", what, printed(), f.calls(t))
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			switch {
			case tc.died:
				waitFor("mark asked")
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				// Start takes the signal once its boot poll's sleep ends; after
				// this long it is waiting for the mark.
				select {
				case err := <-done:
					t.Fatalf("start finished while its launcher was unmarked: %v\n%s\n%s", err, printed(), f.calls(t))
				case <-time.After(4 * time.Second):
				}
				// The launcher is the shell that marks itself and then runs
				// systemd-run; the held touch is its child.
				found, err := exec.Command("pgrep", "-g", strconv.Itoa(cmd.Process.Pid), "-f", "touch .*systemd-run").Output()
				if err != nil {
					t.Fatalf("the unmarked launcher was not found: %v\n%s", err, printed())
				}
				launcher, err := strconv.Atoi(strings.TrimSpace(string(found)))
				if err != nil {
					t.Fatalf("want one unmarked launcher, got %q", found)
				}
				if err := syscall.Kill(launcher, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				// The mark stays held: nothing will ever leave it.
				t.Cleanup(func() { _ = os.Remove(hold) })
			case tc.held:
				waitFor("mark asked")
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				// Longer than any wait that would give up on the mark.
				select {
				case err := <-done:
					t.Fatalf("start finished while its launcher was unmarked: %v\n%s\n%s", err, printed(), f.calls(t))
				case <-time.After(4 * time.Second):
				}
				if calls := f.calls(t); strings.Contains(calls, "scope stopped") || strings.Contains(calls, "launcher started") {
					t.Fatalf("start acted on a launcher that had not started:\n%s", calls)
				}
				if tc.again {
					if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
						t.Fatal(err)
					}
					select {
					case err := <-done:
						t.Fatalf("a second signal cut the undoing short: %v\n%s\n%s", err, printed(), f.calls(t))
					case <-time.After(time.Second):
					}
				}
				if err := os.Remove(hold); err != nil {
					t.Fatal(err)
				}
			case !tc.afterFork:
				waitFor("launcher started")
				if tc.scopeCreated {
					if err := os.WriteFile(barrier, nil, 0o600); err != nil {
						t.Fatal(err)
					}
					waitFor("scope created")
				}
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			var waitErr error
			select {
			case waitErr = <-done:
			case <-time.After(30 * time.Second):
				t.Fatalf("start did not finish after SIGTERM:\n%s\n%s", printed(), f.calls(t))
			}
			var exitErr *exec.ExitError
			if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 143 {
				t.Fatalf("want exit 143 after SIGTERM, got %v:\n%s", waitErr, printed())
			}
			// Release a launcher that was still waiting; a joined one is gone
			// and creates nothing.
			if err := os.WriteFile(barrier, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			time.Sleep(time.Second)
			calls := f.calls(t)
			stopped := strings.LastIndex(calls, "scope stopped")
			if stopped < 0 || strings.LastIndex(calls, "scope created") > stopped {
				t.Fatalf("a scope outlived the interrupted start:\n%s", calls)
			}
			if _, err := os.Stat(scope); err == nil {
				t.Fatalf("the scope is still active:\n%s", calls)
			}
		})
	}
}

// prepareFixture adds what `android-emulator.sh prepare` reads: the
// Dockerfile in the copied repository and a container runtime that records
// its calls and exports one file.
func prepareFixture(t *testing.T) (f smokeFixture, rootfs, stamp string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the emulator path is Linux-only")
	}
	for _, need := range []string{"flock", "sha256sum", "tar"} {
		if _, err := exec.LookPath(need); err != nil {
			t.Skip(need + " unavailable")
		}
	}
	f = newSmokeFixture(t)
	android := filepath.Join(f.dir, "repo", "e2e", "android")
	if err := os.MkdirAll(android, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(android, "emulator.Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sha256sum", filepath.Join(android, "emulator.Dockerfile")).Output()
	if err != nil {
		t.Fatal(err)
	}
	stamp, _, _ = strings.Cut(string(out), " ")
	mockexec.WriteIn(t, filepath.Join(f.dir, "bin"), "docker", `#!/usr/bin/env bash
echo "docker $1" >>"$AO_TEST_LOG"
case "$1" in
  build) [[ "${AO_TEST_BUILD_FAILS:-}" != 1 ]] ;;
  create) echo container-1 ;;
  export)
    work="$(mktemp -d)"
    echo built >"$work/built"
    tar -c -C "$work" built
    rm -rf "$work"
    ;;
esac
`)
	return f, filepath.Join(f.dir, "cache", "agent-overflow", "emulator-rootfs"), stamp
}

// withoutContainerRuntime removes the fixture's docker and answers a PATH
// of the fixture's tools and only the system tools prepare runs, so a
// container runtime installed on the host is not found either.
func (f smokeFixture) withoutContainerRuntime(t *testing.T) string {
	t.Helper()
	if err := os.Remove(filepath.Join(f.dir, "bin", "docker")); err != nil {
		t.Fatal(err)
	}
	tools := filepath.Join(f.dir, "tools")
	if err := os.MkdirAll(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bash", "cat", "cut", "dirname", "flock", "id", "mkdir", "sha256sum"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Skip(name + " unavailable")
		}
		if err := os.Symlink(path, filepath.Join(tools, name)); err != nil {
			t.Fatal(err)
		}
	}
	return "PATH=" + filepath.Join(f.dir, "bin") + string(os.PathListSeparator) + tools
}

func (f smokeFixture) prepare(env ...string) (string, error) {
	cmd := exec.Command("bash", filepath.Join(filepath.Dir(f.smoke), "android-emulator.sh"), "prepare")
	cmd.Env = append(append([]string{}, f.env...), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// stampRuntime writes a runtime of one file, stamped as built from the
// Dockerfile whose hash is stamp.
func stampRuntime(t *testing.T, rootfs, stamp string) {
	t.Helper()
	if err := os.MkdirAll(rootfs, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"kept": "kept\n", ".ao-runtime": stamp + "\n"} {
		if err := os.WriteFile(filepath.Join(rootfs, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Runtime files exported from the current Dockerfile are reused as they
// are: no image is built or looked up, so a pruned image, or a host with no
// container runtime at all, costs nothing.
func TestAndroidEmulatorPrepareReusesTheRuntimeOfAnUnchangedDockerfile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		docker bool
	}{
		{"with a container runtime", true},
		{"without one", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, rootfs, stamp := prepareFixture(t)
			var env []string
			if !tc.docker {
				env = []string{f.withoutContainerRuntime(t)}
			}
			stampRuntime(t, rootfs, stamp)
			out, err := f.prepare(env...)
			if err != nil {
				t.Fatalf("prepare failed: %v\n%s", err, out)
			}
			if calls := f.calls(t); strings.Contains(calls, "docker") {
				t.Fatalf("a current runtime was rebuilt:\n%s", calls)
			}
			if _, err := os.Stat(filepath.Join(rootfs, "kept")); err != nil {
				t.Fatalf("the runtime was replaced: %v", err)
			}
		})
	}
}

// A runtime from another version of the Dockerfile, or none, is built and
// swapped in whole, stamped with the Dockerfile it came from.
func TestAndroidEmulatorPrepareBuildsTheRuntimeOfAChangedDockerfile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing bool
	}{
		{"over an older runtime", true},
		{"with no runtime", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, rootfs, stamp := prepareFixture(t)
			if tc.existing {
				stampRuntime(t, rootfs, "another-dockerfile")
			}
			out, err := f.prepare()
			if err != nil {
				t.Fatalf("prepare failed: %v\n%s", err, out)
			}
			if want := "docker build\ndocker create\ndocker export\ndocker rm\n"; !strings.HasSuffix(f.calls(t), want) {
				t.Fatalf("calls = %q, want them to end %q", f.calls(t), want)
			}
			got, err := os.ReadFile(filepath.Join(rootfs, ".ao-runtime"))
			if err != nil || strings.TrimSpace(string(got)) != stamp {
				t.Fatalf("stamp = %q, %v; want %q", got, err, stamp)
			}
			if _, err := os.Stat(filepath.Join(rootfs, "built")); err != nil {
				t.Fatalf("the built runtime is missing: %v", err)
			}
			if _, err := os.Stat(filepath.Join(rootfs, "kept")); err == nil {
				t.Fatal("the older runtime's files survived the swap")
			}
			// The next prepare finds it current.
			if err := os.Remove(f.log); err != nil {
				t.Fatal(err)
			}
			if out, err := f.prepare(); err != nil || strings.Contains(f.calls(t), "docker") {
				t.Fatalf("the built runtime was not reused: %v\n%s\n%s", err, out, f.calls(t))
			}
		})
	}
}

// A build that cannot run leaves the older runtime and its stamp, and says
// why: no container runtime, a failed build, or an emulator running on the
// files that would be replaced.
func TestAndroidEmulatorPrepareKeepsTheOlderRuntimeWhenItCannotBuild(t *testing.T) {
	for _, tc := range []struct {
		name, env, want string
		noDocker        bool
	}{
		{name: "the build fails", env: "AO_TEST_BUILD_FAILS=1"},
		{name: "an emulator is running", env: "AO_TEST_SCOPE_ACTIVE=1", want: "stop it first"},
		{name: "no container runtime", noDocker: true, want: "missing docker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, rootfs, _ := prepareFixture(t)
			env := []string{tc.env}
			if tc.noDocker {
				env = []string{f.withoutContainerRuntime(t)}
			}
			stampRuntime(t, rootfs, "another-dockerfile")
			out, err := f.prepare(env...)
			if err == nil {
				t.Fatalf("prepare succeeded:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("output = %q, want it to say %q", out, tc.want)
			}
			got, readErr := os.ReadFile(filepath.Join(rootfs, ".ao-runtime"))
			if readErr != nil || strings.TrimSpace(string(got)) != "another-dockerfile" {
				t.Fatalf("stamp = %q, %v; the older runtime was touched", got, readErr)
			}
			if _, err := os.Stat(filepath.Join(rootfs, "kept")); err != nil {
				t.Fatalf("the older runtime's files are gone: %v", err)
			}
			if tc.env == "AO_TEST_SCOPE_ACTIVE=1" && strings.Contains(f.calls(t), "docker") {
				t.Fatalf("a runtime was built under a running emulator:\n%s", f.calls(t))
			}
		})
	}
}

// holdStartMark replaces touch, which a job marks itself started with, by
// one that says each time it is asked and, the at-th time, waits while the
// answered file exists. A job held there is a process of its own that has
// not marked itself yet: what a fork that has not run its command looks
// like to the script.
func (f smokeFixture) holdStartMark(t *testing.T, at int) (hold string) {
	t.Helper()
	touch, err := exec.LookPath("touch")
	if err != nil {
		t.Skip("touch unavailable")
	}
	hold = filepath.Join(f.dir, "hold-start-mark")
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	mockexec.WriteIn(t, filepath.Join(f.dir, "bin"), "touch", `#!/usr/bin/env bash
echo "mark asked" >>"$AO_TEST_LOG"
if [[ "$(grep -c "mark asked" "$AO_TEST_LOG")" == `+strconv.Itoa(at)+` ]]; then
  while [[ -e `+shellQuote(hold)+` ]]; do sleep 0.02; done
fi
exec `+shellQuote(touch)+` "$@"
`)
	return hold
}

// A job that has not marked itself started is not signalled, however long
// that takes: a signal sent before it is a process of its own is lost and
// its command runs anyway. The smoke waits for the mark, then stops the job
// and cleans up.
func TestAndroidSmokeSignalsAJobOnlyOnceItHasStarted(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the emulator path is Linux-only")
	}
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock unavailable")
	}
	f := newSmokeFixture(t)
	f.fakeEmulator(t)
	// The second job is the suite; the first starts the emulator.
	hold := f.holdStartMark(t, 2)
	mockexec.WriteIn(t, filepath.Join(f.dir, "repo", "bin"), "ao-harness-e2e", `#!/usr/bin/env bash
trap 'echo "launcher TERM" >>"$AO_TEST_LOG"; exit 143' TERM
echo "launcher started" >>"$AO_TEST_LOG"
while :; do sleep 0.05; done
`)
	marks := filepath.Join(f.dir, "marks")
	if err := os.Mkdir(marks, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := f.command("AO_ANDROID_SMOKE_PHASE=device", "AO_TEST_ADB_DEVICES=emulator-5554\tdevice", "TMPDIR="+marks)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.Now().Add(30 * time.Second)
	for strings.Count(f.calls(t), "mark asked") < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the suite was never started:\n%s\n%s", out.String(), f.calls(t))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// Longer than any wait that would give up on the mark.
	select {
	case err := <-done:
		t.Fatalf("the smoke finished while its job was unmarked: %v\n%s\n%s", err, out.String(), f.calls(t))
	case <-time.After(4 * time.Second):
	}
	if calls := f.calls(t); strings.Contains(calls, "emulator stop") || strings.Contains(calls, "launcher") {
		t.Fatalf("the smoke acted on a job that had not started:\n%s", calls)
	}
	// The first job's mark went when it finished: a later job can be given
	// its process id.
	if left, _ := filepath.Glob(filepath.Join(marks, "*", "*")); len(left) > 0 {
		t.Fatalf("a finished job's start mark was kept: %v", left)
	}
	// More signals while it waits change nothing: the job is still stopped
	// and joined, and the first signal's status is kept.
	for _, again := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		if err := cmd.Process.Signal(again); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("a second signal cut the stop short: %v\n%s\n%s", err, out.String(), f.calls(t))
	case <-time.After(time.Second):
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("the smoke did not finish once its job was marked:\n%s\n%s", out.String(), f.calls(t))
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 143 {
		t.Fatalf("want exit 143 after SIGTERM, got %v:\n%s", err, out.String())
	}
	// A suite the smoke failed to stop would go on logging.
	time.Sleep(time.Second)
	calls := f.calls(t)
	const cleared = "adb -s emulator-5554 shell locksettings clear --old 1234"
	if !strings.Contains(calls, cleared) || strings.Index(calls, "emulator stop") < strings.Index(calls, cleared) {
		t.Fatalf("the cleanup did not run in order:\n%s", calls)
	}
	if last := strings.LastIndex(calls, "launcher "); last > strings.Index(calls, cleared) {
		t.Fatalf("the suite outlived the cleanup:\n%s", calls)
	}
	if out, err := exec.Command("pgrep", "-g", strconv.Itoa(cmd.Process.Pid), "-f", "ao-harness-e2e").Output(); err == nil && len(out) > 0 {
		t.Fatalf("the suite is still running: %s", out)
	}
	// The marks went with the run.
	if left, err := os.ReadDir(marks); err != nil || len(left) > 0 {
		t.Fatalf("start marks were left behind: %v, %v", left, err)
	}
}

// More signals while the smoke waits for a stopped job to end do not cut
// the wait short: the job is joined before the cleanup, and the first
// signal's status is kept.
func TestAndroidSmokeJoinsItsJobWhenSignalledAgain(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the emulator path is Linux-only")
	}
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock unavailable")
	}
	f := newSmokeFixture(t)
	f.fakeEmulator(t)
	// The suite takes TERM and ends only once the barrier exists.
	barrier := filepath.Join(f.dir, "suite-barrier")
	mockexec.WriteIn(t, filepath.Join(f.dir, "repo", "bin"), "ao-harness-e2e", `#!/usr/bin/env bash
trap 'echo "launcher TERM" >>"$AO_TEST_LOG"; while [[ ! -e `+shellQuote(barrier)+` ]]; do sleep 0.02; done; echo "launcher gone" >>"$AO_TEST_LOG"; exit 143' TERM
echo "launcher started" >>"$AO_TEST_LOG"
while :; do sleep 0.05; done
`)
	cmd := f.command("AO_ANDROID_SMOKE_PHASE=device", "AO_TEST_ADB_DEVICES=emulator-5554\tdevice")
	output, err := os.Create(filepath.Join(f.dir, "smoke.out"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
	printed := func() string {
		body, _ := os.ReadFile(output.Name())
		return string(body)
	}
	cmd.Stdout, cmd.Stderr = output, output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waitFor := func(what string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !strings.Contains(f.calls(t), what) {
			if time.Now().After(deadline) {
				t.Fatalf("never saw %q:\n%s\n%s", what, printed(), f.calls(t))
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor("launcher started")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitFor("launcher TERM")
	for _, again := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		if err := cmd.Process.Signal(again); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("the smoke finished while its suite was still stopping: %v\n%s\n%s", err, printed(), f.calls(t))
	case <-time.After(time.Second):
	}
	if calls := f.calls(t); strings.Contains(calls, "emulator stop") {
		t.Fatalf("the cleanup ran while the suite was still stopping:\n%s", calls)
	}
	if err := os.WriteFile(barrier, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("the smoke did not finish:\n%s\n%s", printed(), f.calls(t))
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 143 {
		t.Fatalf("want the first signal's exit 143, got %v:\n%s", err, printed())
	}
	calls := f.calls(t)
	gone, stopped := strings.Index(calls, "launcher gone"), strings.Index(calls, "emulator stop")
	if gone < 0 || stopped < gone {
		t.Fatalf("want the suite gone before the emulator is stopped:\n%s", calls)
	}
}

// A job that dies while the smoke waits for it to mark itself started is not
// waited on for a mark it will never leave.
func TestAndroidSmokeDoesNotWaitOnAJobThatDiedUnmarked(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the emulator path is Linux-only")
	}
	for _, need := range []string{"flock", "pgrep"} {
		if _, err := exec.LookPath(need); err != nil {
			t.Skip(need + " unavailable")
		}
	}
	f := newSmokeFixture(t)
	f.fakeEmulator(t)
	// The second job is the suite; the first starts the emulator.
	hold := f.holdStartMark(t, 2)
	mockexec.WriteIn(t, filepath.Join(f.dir, "repo", "bin"), "ao-harness-e2e", "#!/bin/sh\nexec sleep 300\n")
	cmd := f.command("AO_ANDROID_SMOKE_PHASE=device", "AO_TEST_ADB_DEVICES=emulator-5554\tdevice")
	// A file, not a pipe: the killed job's held touch outlives it and would
	// keep a pipe open.
	output, err := os.Create(filepath.Join(f.dir, "smoke.out"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
	printed := func() string {
		body, _ := os.ReadFile(output.Name())
		return string(body)
	}
	cmd.Stdout, cmd.Stderr = output, output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.Now().Add(30 * time.Second)
	for strings.Count(f.calls(t), "mark asked") < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the suite was never started:\n%s\n%s", printed(), f.calls(t))
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The job is the shell that marks itself and then runs the suite; the
	// held touch is its child.
	found, err := exec.Command("pgrep", "-g", strconv.Itoa(cmd.Process.Pid), "-f", "touch .*ao-harness-e2e").Output()
	if err != nil {
		t.Fatalf("the unmarked job was not found: %v\n%s", err, printed())
	}
	job, err := strconv.Atoi(strings.TrimSpace(string(found)))
	if err != nil {
		t.Fatalf("want one unmarked job, got %q", found)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// The smoke is now waiting for the mark.
	select {
	case err := <-done:
		t.Fatalf("the smoke finished while its job was unmarked: %v\n%s\n%s", err, printed(), f.calls(t))
	case <-time.After(time.Second):
	}
	if err := syscall.Kill(job, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// The mark stays held: nothing will ever leave it.
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("the smoke waited on a job that was gone:\n%s\n%s", printed(), f.calls(t))
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 143 {
		t.Fatalf("want exit 143 after SIGTERM, got %v:\n%s", err, printed())
	}
	if calls := f.calls(t); !strings.Contains(calls, "emulator stop") {
		t.Fatalf("the cleanup did not run:\n%s", calls)
	}
}

// A second signal that arrives during the exit cleanup does not cut it
// short: the emulator is stopped, the first signal's status is kept and no
// mark directory is left.
func TestAndroidSmokeFinishesItsCleanupWhenSignalledAgain(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the emulator path is Linux-only")
	}
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock unavailable")
	}
	f := newSmokeFixture(t)
	// The emulator's stop is held until the barrier exists.
	barrier := filepath.Join(f.dir, "stop-barrier")
	mockexec.Write(t, filepath.Join(filepath.Dir(f.smoke), "android-emulator.sh"), `#!/usr/bin/env bash
echo "emulator $1" >>"$AO_TEST_LOG"
if [[ "$1" == stop ]]; then
  while [[ ! -e `+shellQuote(barrier)+` ]]; do sleep 0.02; done
  echo "emulator stopped" >>"$AO_TEST_LOG"
fi
`)
	mockexec.WriteIn(t, filepath.Join(f.dir, "repo", "bin"), "ao-harness-e2e", `#!/usr/bin/env bash
trap 'exit 143' TERM
echo "launcher started" >>"$AO_TEST_LOG"
while :; do sleep 0.05; done
`)
	marks := filepath.Join(f.dir, "marks")
	if err := os.MkdirAll(marks, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := f.command("AO_ANDROID_SMOKE_PHASE=device", "AO_TEST_ADB_DEVICES=emulator-5554\tdevice", "TMPDIR="+marks)
	output, err := os.Create(filepath.Join(f.dir, "smoke.out"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
	printed := func() string {
		body, _ := os.ReadFile(output.Name())
		return string(body)
	}
	cmd.Stdout, cmd.Stderr = output, output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waitFor := func(what string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !strings.Contains(f.calls(t), what) {
			if time.Now().After(deadline) {
				t.Fatalf("never saw %q:\n%s\n%s", what, printed(), f.calls(t))
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor("launcher started")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitFor("emulator stop")
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("the smoke finished while its emulator was still stopping: %v\n%s\n%s", err, printed(), f.calls(t))
	case <-time.After(time.Second):
	}
	if err := os.WriteFile(barrier, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("the smoke did not finish:\n%s\n%s", printed(), f.calls(t))
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 143 {
		t.Fatalf("want the first signal's exit 143, got %v:\n%s", err, printed())
	}
	if calls := f.calls(t); !strings.Contains(calls, "emulator stopped") {
		t.Fatalf("the emulator was not stopped:\n%s", calls)
	}
	if left, _ := filepath.Glob(filepath.Join(marks, "*")); len(left) > 0 {
		t.Fatalf("the smoke left its start marks behind: %v", left)
	}
}

// The host phase hands over to the device phase with exec, which runs no
// exit cleanup: its mark directory is gone before that.
func TestAndroidSmokeHostPhaseLeavesNoMarks(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the emulator path is Linux-only")
	}
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock unavailable")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("the host phase requires /dev/kvm")
	}
	f := newSmokeFixture(t)
	f.fakeEmulator(t)
	bin := filepath.Join(f.dir, "bin")
	for _, tool := range []string{"node", "pnpm"} {
		mockexec.WriteIn(t, bin, tool, "#!/usr/bin/env bash\necho \""+tool+" $*\" >>\"$AO_TEST_LOG\"\n")
	}
	mockexec.WriteIn(t, filepath.Join(f.dir, "emulator"), "emulator", "#!/bin/sh\nexit 0\n")
	// The device phase is not run: its launcher only records the handover.
	mockexec.WriteIn(t, filepath.Join(f.dir, "repo", "bin"), "ao-netns", "#!/usr/bin/env bash\necho \"netns $AO_ANDROID_SMOKE_PHASE\" >>\"$AO_TEST_LOG\"\n")
	apk := filepath.Join(f.dir, "repo", "mobile", "android", "app", "build", "outputs", "apk", "debug", "app-debug.apk")
	for _, dir := range []string{filepath.Join(f.dir, "avd", "ao.avd"), filepath.Dir(apk)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(apk, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	marks := filepath.Join(f.dir, "marks")
	if err := os.MkdirAll(marks, 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := f.command("TMPDIR=" + marks).CombinedOutput()
	if err != nil {
		t.Fatalf("the host phase failed: %v\n%s\n%s", err, out, f.calls(t))
	}
	calls := f.calls(t)
	for _, step := range []string{"node ", "pnpm ", "emulator prepare", "netns device"} {
		if !strings.Contains(calls, step) {
			t.Fatalf("the host phase did not reach %q:\n%s\n%s", step, calls, out)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(marks, "*")); len(left) > 0 {
		t.Fatalf("the host phase left its start marks behind: %v", left)
	}
}
