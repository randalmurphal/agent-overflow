package ownedtemp

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const childEnv = "AO_OWNEDTEMP_CHILD_PARENT"

// TestMain lets the test binary stand in for a process that makes a
// directory and exits without releasing it.
func TestMain(m *testing.M) {
	if parent := os.Getenv(childEnv); parent != "" {
		dir, _, err := Make(parent, "ownedtemp-test-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Print(dir)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// TestSweepRemovesOnlyDirectoriesWhoseOwnerIsGone: a process that exits
// without its release leaves a directory the next sweep removes; a held
// directory, one without a lock file and other names stay.
func TestSweepRemovesOnlyDirectoriesWhoseOwnerIsGone(t *testing.T) {
	parent := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(self, "-test.run=^$")
	child.Env = append(os.Environ(), childEnv+"="+parent)
	out, err := child.Output()
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	exited := strings.TrimSpace(string(out))
	if !exists(t, exited) {
		t.Fatalf("child's directory %q missing", exited)
	}

	held, release, err := Make(parent, "ownedtemp-test-")
	if err != nil {
		t.Fatal(err)
	}
	unlocked := filepath.Join(parent, "ownedtemp-test-unlocked")
	other := filepath.Join(parent, "other-prefix")
	for _, dir := range []string{unlocked, other} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(parent, "ownedtemp-test-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Sweep(parent, "ownedtemp-test-"); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]bool{exited: false, held: true, unlocked: true, other: true, file: true} {
		if got := exists(t, dir); got != want {
			t.Fatalf("%s present = %v, want %v", filepath.Base(dir), got, want)
		}
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if exists(t, held) {
		t.Fatal("release left the directory")
	}
}
