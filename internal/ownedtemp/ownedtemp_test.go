package ownedtemp

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	childEnv = "AO_OWNEDTEMP_CHILD_PARENT"
	// holderEnv makes the test binary a process that was passed a Shared
	// directory's lock and holds it until its stdin closes.
	holderEnv = "AO_OWNEDTEMP_HOLDER"
)

// TestMain lets the test binary stand in for a process that makes a
// directory and exits without releasing it.
func TestMain(m *testing.M) {
	if os.Getenv(holderEnv) != "" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
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

// TestSharedDirectoryOutlivesNoneOfItsHolders: a process the lock was
// passed to keeps the directory owned after this process releases it, so
// a sweep leaves it; once that process exits, the next sweep removes it.
func TestSharedDirectoryOutlivesNoneOfItsHolders(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec.Cmd.ExtraFiles is Unix-only")
	}
	parent := t.TempDir()
	shared, err := MakeShared(parent, "ownedtemp-test-")
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	holder := exec.Command(self, "-test.run=^$")
	holder.Env = append(os.Environ(), holderEnv+"=1")
	holder.ExtraFiles = []*os.File{shared.Lock}
	stdin, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = holder.Wait()
	})

	if err := shared.Release(); !errors.Is(err, ErrInUse) {
		t.Fatalf("Release while another process holds the lock = %v, want ErrInUse", err)
	}
	if err := Sweep(parent, "ownedtemp-test-"); err != nil {
		t.Fatal(err)
	}
	if !exists(t, shared.Dir) {
		t.Fatal("a sweep removed a directory a process still holds")
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := holder.Wait(); err != nil {
		t.Fatalf("holder: %v", err)
	}
	if err := Sweep(parent, "ownedtemp-test-"); err != nil {
		t.Fatal(err)
	}
	if exists(t, shared.Dir) {
		t.Fatal("the sweep after the last holder exited left the directory")
	}
}

// TestHeldCountsOnlyDirectoriesAProcessHolds: a directory this process
// holds counts, one whose owner exited is removed and not counted, and
// one without a lock file stays uncounted.
func TestHeldCountsOnlyDirectoriesAProcessHolds(t *testing.T) {
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
	first, err := MakeShared(parent, "ownedtemp-test-")
	if err != nil {
		t.Fatal(err)
	}
	second, err := MakeShared(parent, "ownedtemp-test-")
	if err != nil {
		t.Fatal(err)
	}
	unlocked := filepath.Join(parent, "ownedtemp-test-unlocked")
	if err := os.Mkdir(unlocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if n, err := Held(parent, "ownedtemp-test-"); err != nil || n != 2 {
		t.Fatalf("Held = %d, %v; want the 2 held directories", n, err)
	}
	if exists(t, exited) || !exists(t, unlocked) {
		t.Fatal("Held kept a directory whose owner exited or removed one without a lock")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if n, err := Held(parent, "ownedtemp-test-"); err != nil || n != 1 {
		t.Fatalf("Held after a release = %d, %v; want 1", n, err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestSharedReleaseRemovesAnUnsharedDirectory: with no other holder,
// Release removes the directory itself.
func TestSharedReleaseRemovesAnUnsharedDirectory(t *testing.T) {
	shared, err := MakeShared(t.TempDir(), "ownedtemp-test-")
	if err != nil {
		t.Fatal(err)
	}
	if err := shared.Release(); err != nil {
		t.Fatal(err)
	}
	if exists(t, shared.Dir) {
		t.Fatal("Release left the directory")
	}
}
