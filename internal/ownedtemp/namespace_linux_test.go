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

// TestAThreadOfAnExitedLeaderKeepsItsNamespace: a process whose leader
// thread has exited names no namespace of its own, and its other threads
// are still in it.
func TestAThreadOfAnExitedLeaderKeepsItsNamespace(t *testing.T) {
	proc := t.TempDir()
	const want = "mnt:[4026532999]"
	task := func(pid, tid, ns string) {
		t.Helper()
		dir := filepath.Join(proc, pid, "task", tid, "ns")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if ns == "" {
			return
		}
		if err := os.Symlink(ns, filepath.Join(dir, "mnt")); err != nil {
			t.Fatal(err)
		}
	}
	// The exited leader: its ns links do not resolve.
	if err := os.MkdirAll(filepath.Join(proc, "100", "ns"), 0o700); err != nil {
		t.Fatal(err)
	}
	task("100", "100", "")
	task("200", "200", "mnt:[4026531841]")
	if err := os.MkdirAll(filepath.Join(proc, "self"), 0o700); err != nil {
		t.Fatal(err)
	}
	if in, err := threadInMountNamespace(proc, want); err != nil || in {
		t.Fatalf("in use = %v, %v; want no thread in the namespace yet", in, err)
	}
	task("100", "101", want)
	if in, err := threadInMountNamespace(proc, want); err != nil || !in {
		t.Fatalf("in use = %v, %v; want the live thread of the exited leader counted", in, err)
	}
}
