package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newHarnessLockDir builds an empty data dir the lock can live in.
func newHarnessLockDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "agent-overflow")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	return dir
}

// releaseForTest drops the lock the way process death would. Production
// never calls it — the lock is held for the process's whole life on
// purpose — but a test cannot fork a backend just to kill it.
func (l *harnessInstanceLock) releaseForTest(t *testing.T) {
	t.Helper()
	if err := l.file.Close(); err != nil {
		t.Fatalf("release harness lock: %v", err)
	}
}

func TestHarnessInstanceLockRefusesASecondBootOnOneDataRoot(t *testing.T) {
	dir := newHarnessLockDir(t)

	first, err := acquireHarnessInstanceLock(dir, "harness")
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	defer first.releaseForTest(t)

	second, err := acquireHarnessInstanceLock(dir, "soak")
	if err == nil {
		second.releaseForTest(t)
		t.Fatal("second boot on the same data root was allowed; two backends would open one SQLite file")
	}
	// The message has to be actionable: an agent reading it needs the
	// lock path and the pid to go stop.
	for _, want := range []string{harnessLockFileName, "--data-dir"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
	if pid := os.Getpid(); !strings.Contains(err.Error(), "pid ") {
		t.Errorf("refusal %q does not name the holding pid (this process is %d)", err, pid)
	}
}

func TestHarnessInstanceLockIsFreeAfterTheHolderDies(t *testing.T) {
	dir := newHarnessLockDir(t)

	crashed, err := acquireHarnessInstanceLock(dir, "harness")
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	// No cleanup ran, no file was removed — exactly what a SIGKILL
	// leaves behind. The kernel drops the lock with the descriptor.
	crashed.releaseForTest(t)

	next, err := acquireHarnessInstanceLock(dir, "harness")
	if err != nil {
		t.Fatalf("boot after a crashed holder: %v (a stale lock file must not brick the next boot)", err)
	}
	defer next.releaseForTest(t)
}

func TestHarnessInstanceLockNamesTheHolder(t *testing.T) {
	dir := newHarnessLockDir(t)

	held, err := acquireHarnessInstanceLock(dir, "soak")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer held.releaseForTest(t)

	raw, err := os.ReadFile(filepath.Join(dir, harnessLockFileName))
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	if !strings.Contains(string(raw), `"mode":"soak"`) {
		t.Errorf("lock file %q does not record the boot mode", raw)
	}
	if !strings.Contains(string(raw), `"pid":`) {
		t.Errorf("lock file %q does not record the pid", raw)
	}
}

// TestHarnessBootModeNamesTheFlag pins that --soak and --harness share
// one lock but are told apart in the refusal message.
func TestHarnessBootModeNamesTheFlag(t *testing.T) {
	if got := harnessBootMode(cliFlags{harness: true}); got != "harness" {
		t.Errorf("harnessBootMode(--harness) = %q", got)
	}
	if got := harnessBootMode(cliFlags{soak: true}); got != "soak" {
		t.Errorf("harnessBootMode(--soak) = %q", got)
	}
	if got := harnessBootMode(cliFlags{soak: true, isolatedProfile: "perf"}); got != "perf" {
		t.Errorf("harnessBootMode(--soak --isolated-profile perf) = %q", got)
	}
}

// TestPrepareHarnessTakesTheInstanceLock proves the guard is on the BOOT
// path, not just available to it — `make harness` and the wails3 dev
// harness path call prepareHarness directly and have no other liveness
// check at all.
func TestPrepareHarnessTakesTheInstanceLock(t *testing.T) {
	root := t.TempDir()
	// prepareHarness fails later (no mock provider binary resolvable in a
	// bare temp dir), but the lock is taken before that — which is the
	// point: every write below it lands in a tree a live backend may own.
	_, _ = prepareHarness(cliFlags{dataDir: root, harness: true})

	dataDir := filepath.Join(root, "agent-overflow")
	held, err := acquireHarnessInstanceLock(dataDir, "harness")
	if err == nil {
		held.releaseForTest(t)
		t.Fatal("prepareHarness left the data root unlocked")
	}
	if !strings.Contains(err.Error(), harnessLockFileName) {
		t.Fatalf("unexpected error %v", err)
	}
	if heldHarnessLock != nil {
		heldHarnessLock.releaseForTest(t)
		heldHarnessLock = nil
	}
}
