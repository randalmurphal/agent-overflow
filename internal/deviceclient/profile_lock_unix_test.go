//go:build !windows

package deviceclient

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A fork anywhere in the process copies the lock's descriptor until its
// exec. Releasing the lock must free it anyway.
func TestProfileLockReleaseFreesTheLockWhileAForkHoldsACopy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "locks", "session.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if locked, err := tryProfileLock(f); !locked || err != nil {
		t.Fatalf("lock = %t, %v", locked, err)
	}
	holder := exec.Command("/bin/sleep", "30")
	holder.ExtraFiles = []*os.File{f}
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
	if err := releaseProfile(f); err != nil {
		t.Fatal(err)
	}
	// The lock is free at once or not until the holder exits; the budget
	// only bounds a failure.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	unlock, err := lockProfile(ctx, dir, "session")
	if err != nil {
		t.Fatalf("the released lock is still held: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
}
