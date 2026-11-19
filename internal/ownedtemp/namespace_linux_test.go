//go:build linux

package ownedtemp

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestMountNamespaceKeepsADirectory: a directory recording a mount
// namespace that has a process, this test's own, stays owned once its
// lock is free; one recording a namespace nobody is in is swept.
func TestMountNamespaceKeepsADirectory(t *testing.T) {
	parent := t.TempDir()
	kept, err := MakeShared(parent, "ownedtemp-test-")
	if err != nil {
		t.Fatal(err)
	}
	if err := KeepForMountNamespace(kept.Dir); err != nil {
		t.Fatal(err)
	}
	if err := kept.Release(); !errors.Is(err, ErrInUse) {
		t.Fatalf("Release while this process is in the recorded namespace = %v, want ErrInUse", err)
	}
	gone, err := MakeShared(parent, "ownedtemp-test-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gone.Dir, namespaceName), []byte("mnt:[1]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := gone.Lock.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := Held(parent, "ownedtemp-test-"); err != nil || n != 1 {
		t.Fatalf("Held = %d, %v; want only the directory whose namespace has a process", n, err)
	}
	if !exists(t, kept.Dir) || exists(t, gone.Dir) {
		t.Fatal("the sweep removed the directory a namespace keeps, or kept the one nobody owns")
	}
	if err := os.Remove(filepath.Join(kept.Dir, namespaceName)); err != nil {
		t.Fatal(err)
	}
	if err := Sweep(parent, "ownedtemp-test-"); err != nil || exists(t, kept.Dir) {
		t.Fatalf("Sweep = %v, directory kept %v; want it removed without its namespace", err, exists(t, kept.Dir))
	}
}
