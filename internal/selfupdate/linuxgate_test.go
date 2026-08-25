package selfupdate

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestUpdaterProbeDirWritable_WritableDir covers the ordinary install: a
// directory this process may write to passes, and the probe leaves nothing
// behind.
func TestUpdaterProbeDirWritable_WritableDir(t *testing.T) {
	dir := t.TempDir()
	if err := probeDirWritable(dir); err != nil {
		t.Fatalf("probeDirWritable(%s) = %v, want nil", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("probe left litter behind: %v", entries)
	}
}

// TestUpdaterProbeDirWritable_ReadOnlyDir is the case os.Stat mode bits alone
// would also catch — kept so the negative path is pinned at all.
func TestUpdaterProbeDirWritable_ReadOnlyDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions do not gate writes on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not restrict writes")
	}
	dir := filepath.Join(t.TempDir(), "install")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	// Restore write permission so t.TempDir's cleanup can remove the tree.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := probeDirWritable(dir); err == nil {
		t.Fatal("probeDirWritable on a read-only directory = nil, want an error")
	}
}

// TestUpdaterProbeDirWritable_MissingDir pins that a nonexistent install
// directory refuses rather than reporting writable.
func TestUpdaterProbeDirWritable_MissingDir(t *testing.T) {
	if err := probeDirWritable(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("probeDirWritable on a missing directory = nil, want an error")
	}
}

// TestUpdaterProbeDirWritable_Concurrent proves the probe is collision-free:
// two launches starting at the same moment (a second --connect client) must
// not fail each other by racing on a fixed filename.
func TestUpdaterProbeDirWritable_Concurrent(t *testing.T) {
	dir := t.TempDir()
	errs := make(chan error, 8)
	for range cap(errs) {
		go func() { errs <- probeDirWritable(dir) }()
	}
	for range cap(errs) {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent probeDirWritable = %v, want nil", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("concurrent probes left litter behind: %v", entries)
	}
}

// TestUpdaterLinuxGate_AppImageRefuses pins the AppImage refusal: the marker
// alone is enough, and it is checked BEFORE the writability probe (the mount
// is read-only, so probing it would only produce a less useful reason).
func TestUpdaterLinuxGate_AppImageRefuses(t *testing.T) {
	t.Setenv("APPIMAGE", "/home/user/Apps/agent-overflow.AppImage")
	reason := LinuxUpdaterBlocked()
	if reason == "" {
		t.Fatal("LinuxUpdaterBlocked() = \"\" under an AppImage launch, want a refusal")
	}
	if !strings.Contains(reason, "AppImage") {
		t.Fatalf("LinuxUpdaterBlocked() = %q, want a reason naming the AppImage", reason)
	}
}

// TestUpdaterLinuxGate_AppDirMarkerRefuses covers the other marker the
// AppImage runtime exports — a launch that carries APPDIR without APPIMAGE is
// still running off the read-only mount.
func TestUpdaterLinuxGate_AppDirMarkerRefuses(t *testing.T) {
	t.Setenv("APPIMAGE", "")
	t.Setenv("APPDIR", "/tmp/.mount_agentXXXXXX")
	if reason := LinuxUpdaterBlocked(); reason == "" {
		t.Fatal("LinuxUpdaterBlocked() = \"\" with APPDIR set, want a refusal")
	}
}

// TestUpdaterLinuxGate_OrdinaryInstallAllowed pins the permissive side: the
// test binary lives in a writable temp directory with no AppImage markers, so
// nothing must block the updater. Without this the gate could refuse
// everything and the negative tests would still pass.
func TestUpdaterLinuxGate_OrdinaryInstallAllowed(t *testing.T) {
	t.Setenv("APPIMAGE", "")
	t.Setenv("APPDIR", "")
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}
	if err := probeDirWritable(filepath.Dir(exe)); err != nil {
		t.Skipf("test binary directory is not writable (%v); nothing to assert", err)
	}
	if reason := LinuxUpdaterBlocked(); reason != "" {
		t.Fatalf("LinuxUpdaterBlocked() = %q, want \"\" for a writable non-AppImage install", reason)
	}
}
