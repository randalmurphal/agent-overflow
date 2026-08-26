package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// configRootFixture points the OS config lookup at a temp tree and
// returns (configRoot, realAppDataDir) — the two paths an isolated boot
// must refuse, because it seeds and wipes its data root wholesale.
func configRootFixture(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	t.Setenv("HOME", base)
	return base, filepath.Join(base, "agent-overflow")
}

func TestUpRefusesTheRealAppDataDirAndItsParent(t *testing.T) {
	configRoot, appData := configRootFixture(t)

	for _, root := range []string{configRoot, appData} {
		err := refuseUnsafeDataRoot(root)
		if err == nil {
			t.Fatalf("--data-dir %s was accepted", root)
		}
		if !strings.Contains(err.Error(), "where the real app data lives") {
			t.Errorf("refusal for %s does not say why: %v", root, err)
		}
	}
}

func TestUpRefusesASymlinkedDataRoot(t *testing.T) {
	configRootFixture(t)
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := refuseUnsafeDataRoot(link)
	if err == nil {
		t.Fatal("a symlinked data root was accepted")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error = %v", err)
	}
}

// The app directory INSIDE the root is the second planted-link shape: the
// root itself is an ordinary directory, and `<root>/agent-overflow` aims
// the seed-and-wipe somewhere else.
func TestUpRefusesASymlinkedAppDirectoryInsideTheRoot(t *testing.T) {
	configRootFixture(t)
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, appDataDirName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := refuseUnsafeDataRoot(root)
	if err == nil {
		t.Fatal("a symlinked app directory was accepted")
	}
	if !strings.Contains(err.Error(), appDataDirName) {
		t.Fatalf("error = %v", err)
	}
}

func TestUpAcceptsAScratchRootThatDoesNotExistYet(t *testing.T) {
	configRootFixture(t)
	root := filepath.Join(t.TempDir(), "not-created-yet")
	if err := refuseUnsafeDataRoot(root); err != nil {
		t.Fatalf("a fresh scratch root was refused: %v", err)
	}
}

// The refusal has to run BEFORE anything is created. `up` used to
// MkdirAll a log directory into a root the backend was about to refuse,
// so a mistyped --data-dir left a half-made tree inside the real config
// root and then failed with a message about the boot.
func TestUpCreatesNothingInsideARootItRefuses(t *testing.T) {
	_, appData := configRootFixture(t)

	e, _, _ := testEnv(t.TempDir())
	err := runUp(e, []string{"--data-dir", appData})
	if err == nil {
		t.Fatal("up booted onto the real app data dir")
	}
	if _, statErr := os.Stat(appData); statErr == nil {
		t.Fatalf("up created %s before refusing it", appData)
	}
}
