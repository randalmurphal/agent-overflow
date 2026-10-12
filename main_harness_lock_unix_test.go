//go:build unix

package main

import (
	"context"
	"errors"
	"os"
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

// A fork anywhere in the process copies the update lock's descriptor until
// its exec. The update command's release must free the data root anyway.
func TestUpdateLockReleaseFreesTheRootWhileAForkHoldsACopy(t *testing.T) {
	prev := dataDirRoot
	dataDirRoot = t.TempDir()
	t.Cleanup(func() { dataDirRoot = prev })
	if err := os.MkdirAll(bootSettingsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	lock, release, err := acquireUpdateLock(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	holder := exec.Command("/bin/sleep", "30")
	holder.ExtraFiles = []*os.File{lock}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := holder.Process.Kill(); err != nil {
			t.Errorf("stop the descriptor holder: %v", err)
		}
		var exit *exec.ExitError
		if err := holder.Wait(); !errors.As(err, &exit) {
			t.Errorf("reap the descriptor holder: %v", err)
		}
	})
	release()
	next, err := acquireBackendInstanceLock(bootSettingsDir())
	if err != nil {
		t.Fatalf("the released update lock still holds the data root: %v", err)
	}
	next.releaseForTest(t)
}
