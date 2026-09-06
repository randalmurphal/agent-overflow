//go:build unix

package main

import (
	"os/exec"
	"testing"
)

func TestInstanceLockDoesNotSurviveInAnUnrelatedChild(t *testing.T) {
	dir := newHarnessLockDir(t)
	owner, err := acquireBackendInstanceLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.file.Close() })
	child := exec.Command("/bin/sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		child.Process.Kill()
		child.Wait()
	})
	// Start returns after exec. The child is still alive, but only the
	// parent should own this lock; ending its ownership must free the root.
	owner.releaseForTest(t)
	next, err := acquireBackendInstanceLock(dir)
	if err != nil {
		t.Fatalf("child inherited the prior backend's lock and blocked restart: %v", err)
	}
	next.releaseForTest(t)
}
