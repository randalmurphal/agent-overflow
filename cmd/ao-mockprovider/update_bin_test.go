package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/providerupdate/updatefake"
)

// oneShotTimeout bounds a doctor, update or --version run. These routes
// never dial control or read stdin, so they finish well inside it.
const oneShotTimeout = 5 * time.Second

// updateFixture is a private copy of the mock binary and a private home, so
// a test that changes the binary's mtime or records an install interferes
// with no other test.
type updateFixture struct {
	bin  string
	home string
}

func newUpdateFixture(t *testing.T) updateFixture {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, filepath.Base(mockBin))
	src, err := os.Open(mockBin)
	if err != nil {
		t.Fatalf("open mock binary: %v", err)
	}
	defer func() {
		if err := src.Close(); err != nil {
			t.Errorf("close mock binary: %v", err)
		}
	}()
	dst, err := os.OpenFile(bin, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatalf("create mock copy: %v", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		t.Fatalf("copy mock binary: %v", err)
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("close mock copy: %v", err)
	}
	// An old mtime makes "the update moved it" unambiguous.
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(bin, old, old); err != nil {
		t.Fatalf("age mock copy: %v", err)
	}
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatalf("make home: %v", err)
	}
	return updateFixture{bin: bin, home: home}
}

// statePath is updatefake.MockInstall's installed-version record.
func (f updateFixture) statePath() string {
	return filepath.Join(f.home, ".ao-mockprovider", "installed-version")
}

func (f updateFixture) install() updatefake.MockInstall {
	return updatefake.MockInstall{Home: f.home, Executable: f.bin}
}

// advertise makes version what the next update installs, as the harness
// does when it advertises a release.
func (f updateFixture) advertise(t *testing.T, version string) {
	t.Helper()
	if err := f.install().SetTarget(version); err != nil {
		t.Fatalf("advertise %s: %v", version, err)
	}
}

func (f updateFixture) mtime(t *testing.T) time.Time {
	t.Helper()
	info, err := os.Stat(f.bin)
	if err != nil {
		t.Fatalf("stat mock copy: %v", err)
	}
	return info.ModTime()
}

type oneShotResult struct {
	stdout, stderr string
	err            error
}

// run executes the copy with argv args under the fixture's home, with no
// harness control or scenario environment unless extraEnv supplies it.
//
// A start that fails with ETXTBSY is retried: a parallel test's fork can
// briefly hold the write descriptor of a fixture copy still being written
// (golang/go#22315), which makes executing the finished copy fail.
func (f updateFixture) run(t *testing.T, extraEnv []string, args ...string) oneShotResult {
	t.Helper()
	for attempt := 1; ; attempt++ {
		res := f.runOnce(t, extraEnv, args...)
		if !errors.Is(res.err, syscall.ETXTBSY) || attempt == 20 {
			return res
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f updateFixture) runOnce(t *testing.T, extraEnv []string, args ...string) oneShotResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), oneShotTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.bin, args...)
	cmd.Dir = t.TempDir()
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "HOME", "USERPROFILE", control.EnvAddr, control.EnvToken,
			control.EnvTranscriptHome, control.EnvWorkspaceRoot, envScenarioFile, envFixtureRoot:
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	cmd.Env = append(cmd.Env, "HOME="+f.home, "USERPROFILE="+f.home)
	cmd.Env = append(cmd.Env, extraEnv...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("%v did not exit within %s; stderr:\n%s", args, oneShotTimeout, stderr.String())
	}
	return oneShotResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

func (f updateFixture) mustRun(t *testing.T, args ...string) oneShotResult {
	t.Helper()
	res := f.run(t, nil, args...)
	if res.err != nil {
		t.Fatalf("%v: %v; stderr:\n%s", args, res.err, res.stderr)
	}
	return res
}

func TestDoctorReportsLatestChannel(t *testing.T) {
	t.Parallel()
	f := newUpdateFixture(t)
	res := f.mustRun(t, "doctor")
	for _, want := range []string{
		"Claude Code doctor\n",
		"Running: native (" + mockVersionNumber + ")\n",
		"Auto-update channel: latest\n",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Fatalf("doctor stdout missing %q:\n%s", want, res.stdout)
		}
	}
}

// updateTargetVersion is an intermediate catalogue version, so an update
// that installed the newest release instead of the advertised one fails.
const updateTargetVersion = "99.0.1"

func TestUpdateWithNothingAdvertisedChangesNothing(t *testing.T) {
	t.Parallel()
	f := newUpdateFixture(t)
	before := f.mtime(t)
	res := f.mustRun(t, "update")
	want := "Current version: " + mockVersionNumber + "\n" +
		"Checking for updates to latest version...\n" +
		"Claude Code is up to date (" + mockVersionNumber + ")\n"
	if res.stdout != want {
		t.Fatalf("update stdout = %q, want %q", res.stdout, want)
	}
	if got := f.mtime(t); !got.Equal(before) {
		t.Fatalf("update with nothing advertised moved mtime %v -> %v", before, got)
	}
	if _, err := os.Stat(f.statePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("update with nothing advertised recorded an install: %v", err)
	}
}

func TestUpdateInstallsTheAdvertisedVersion(t *testing.T) {
	t.Parallel()
	f := newUpdateFixture(t)

	if got := strings.TrimSpace(f.mustRun(t, "--version").stdout); got != versionString {
		t.Fatalf("--version before update = %q, want %q", got, versionString)
	}

	f.advertise(t, updateTargetVersion)
	before := f.mtime(t)
	res := f.mustRun(t, "update")
	want := "Current version: " + mockVersionNumber + "\n" +
		"Checking for updates to latest version...\n" +
		"Updating to " + updateTargetVersion + "...\n" +
		"Successfully updated from " + mockVersionNumber + " to version " + updateTargetVersion + "\n"
	if res.stdout != want {
		t.Fatalf("update stdout = %q, want %q", res.stdout, want)
	}
	after := f.mtime(t)
	if !after.After(before) {
		t.Fatalf("update left mtime at %v (before %v), want it moved forward", after, before)
	}

	wantVersion := strings.ReplaceAll(versionString, mockVersionNumber, updateTargetVersion)
	if got := strings.TrimSpace(f.mustRun(t, "--version").stdout); got != wantVersion {
		t.Fatalf("--version after update = %q, want %q", got, wantVersion)
	}
	if !strings.Contains(f.mustRun(t, "doctor").stdout, "Running: native ("+updateTargetVersion+")\n") {
		t.Fatalf("doctor after update does not report %s", updateTargetVersion)
	}

	// A second update is a no-op: same exit, no install, binary untouched.
	again := f.mustRun(t, "update")
	wantAgain := "Current version: " + updateTargetVersion + "\n" +
		"Checking for updates to latest version...\n" +
		"Claude Code is up to date (" + updateTargetVersion + ")\n"
	if again.stdout != wantAgain {
		t.Fatalf("second update stdout = %q, want %q", again.stdout, wantAgain)
	}
	if got := f.mtime(t); !got.Equal(after) {
		t.Fatalf("second update moved mtime %v -> %v", after, got)
	}

	// A reset restores the baseline as a new binary and advertises nothing.
	if changed, err := f.install().Reset(); err != nil || !changed {
		t.Fatalf("Reset = %v, %v; want a change", changed, err)
	}
	if got := f.mtime(t); !got.After(after) {
		t.Fatalf("reset left mtime at %v (installed at %v), want it moved forward", got, after)
	}
	if got := strings.TrimSpace(f.mustRun(t, "--version").stdout); got != versionString {
		t.Fatalf("--version after reset = %q, want %q", got, versionString)
	}
	if !strings.Contains(f.mustRun(t, "update").stdout, "Claude Code is up to date ("+mockVersionNumber+")\n") {
		t.Fatal("an update after reset installed something")
	}
}

// TestUpdateMovesAFutureMtimeForward covers a binary stamped ahead of the
// clock, where "now" would move its identity backward or not at all.
func TestUpdateMovesAFutureMtimeForward(t *testing.T) {
	t.Parallel()
	f := newUpdateFixture(t)
	future := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := os.Chtimes(f.bin, future, future); err != nil {
		t.Fatal(err)
	}
	f.advertise(t, updateTargetVersion)
	f.mustRun(t, "update")
	if got := f.mtime(t); !got.After(future) {
		t.Fatalf("update left mtime at %v, want after %v", got, future)
	}
}

// TestUpdatedVersionPassesProviderDetection reads the updated version through
// the app's real detection, for both provider names.
func TestUpdatedVersionPassesProviderDetection(t *testing.T) {
	f := newUpdateFixture(t)
	f.advertise(t, updateTargetVersion)
	f.mustRun(t, "update")
	t.Setenv("HOME", f.home)
	t.Setenv("USERPROFILE", f.home)
	for _, name := range []string{"claude", "codex"} {
		status := provider.DetectProvider(name, f.bin)
		if status.Status != "ready" || !strings.HasPrefix(status.Version, updateTargetVersion+" ") {
			t.Fatalf("DetectProvider(%q) = %+v, want ready at %s", name, status, updateTargetVersion)
		}
	}
	if got := provider.ParseCodexCLIVersion(f.mustRun(t, "--version").stdout); got != updateTargetVersion {
		t.Fatalf("codex parse of updated --version = %q, want %s", got, updateTargetVersion)
	}
}

func TestVersionFallsBackOnCorruptState(t *testing.T) {
	t.Parallel()
	f := newUpdateFixture(t)
	if err := os.MkdirAll(filepath.Dir(f.statePath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.statePath(), []byte("not a version\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := f.mustRun(t, "--version")
	if got := strings.TrimSpace(res.stdout); got != versionString {
		t.Fatalf("--version with corrupt state = %q, want %q", got, versionString)
	}
	if !strings.Contains(res.stderr, "not a version") {
		t.Fatalf("corrupt state not reported on stderr: %q", res.stderr)
	}
}

func TestVersionFallsBackOnUnreadableState(t *testing.T) {
	t.Parallel()
	f := newUpdateFixture(t)
	// A directory at the state path reads as an error on every platform.
	if err := os.MkdirAll(f.statePath(), 0o700); err != nil {
		t.Fatal(err)
	}
	res := f.mustRun(t, "--version")
	if got := strings.TrimSpace(res.stdout); got != versionString {
		t.Fatalf("--version with unreadable state = %q, want %q", got, versionString)
	}
	if !strings.Contains(res.stderr, "read installed version") {
		t.Fatalf("unreadable state not reported on stderr: %q", res.stderr)
	}
}

func TestUpdateFailsLoudlyWhenStateCannotBeWritten(t *testing.T) {
	t.Parallel()
	f := newUpdateFixture(t)
	// A directory at the state path makes the publishing rename fail after
	// the mtime change, the last step of the install.
	if err := os.MkdirAll(filepath.Join(f.statePath(), "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.advertise(t, updateTargetVersion)
	res := f.run(t, nil, "update")
	var exitErr *exec.ExitError
	if !errors.As(res.err, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("update with unwritable state: err = %v, want non-zero exit; stdout:\n%s", res.err, res.stdout)
	}
	if !strings.Contains(res.stderr, "update failed: publish") {
		t.Fatalf("update failure stderr = %q, want the publish error", res.stderr)
	}
	if strings.Contains(res.stdout, "Successfully updated") {
		t.Fatalf("failed update reported success:\n%s", res.stdout)
	}
	entries, err := os.ReadDir(filepath.Dir(f.statePath()))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if name := entry.Name(); name != "installed-version" && name != "update-target" {
			t.Fatalf("failed update left %s in the state dir", name)
		}
	}
}

func TestUpdateFailsLoudlyWithoutHome(t *testing.T) {
	t.Parallel()
	f := newUpdateFixture(t)
	f.home = ""
	before := f.mtime(t)
	res := f.run(t, nil, "update")
	if res.err == nil {
		t.Fatalf("update without HOME succeeded; stdout:\n%s", res.stdout)
	}
	if !strings.Contains(res.stderr, "resolve home") {
		t.Fatalf("update without HOME stderr = %q, want the home error", res.stderr)
	}
	if got := f.mtime(t); !got.Equal(before) {
		t.Fatalf("failed update moved mtime %v -> %v", before, got)
	}
	if got := strings.TrimSpace(f.mustRun(t, "--version").stdout); got != versionString {
		t.Fatalf("--version without HOME = %q, want %q", got, versionString)
	}
}

// TestUpdateRoutesDoNotRegister runs every route with live harness control
// in the environment and asserts none of them registers.
func TestUpdateRoutesDoNotRegister(t *testing.T) {
	t.Parallel()
	var registrations atomic.Int32
	srv, err := control.NewServer(control.ServerConfig{
		Resolve: func(control.Registration) (control.Assignment, error) {
			registrations.Add(1)
			return control.Assignment{}, errors.New("one-shot route registered")
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	env := []string{control.EnvAddr + "=" + srv.Addr(), control.EnvToken + "=" + srv.Token()}

	f := newUpdateFixture(t)
	for _, args := range [][]string{{"doctor"}, {"update"}, {"--version"}, {"update"}} {
		if res := f.run(t, env, args...); res.err != nil {
			t.Fatalf("%v: %v; stderr:\n%s", args, res.err, res.stderr)
		}
	}
	if n := registrations.Load(); n != 0 {
		t.Fatalf("one-shot routes registered %d times with control", n)
	}
}
