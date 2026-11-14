//go:build unix

package worktreesetup

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// copyWhileAForkHoldsTheLock copies one file of the given mode while the
// test holds ForkLock exclusively, as a fork does. It reports whether the
// copy waited for the lock: the copy either finishes or parks acquiring it,
// and the goroutine dump says which, so the answer does not depend on how
// long anything takes. The lock is released before it returns and the copy
// has finished.
//
// Callers are not parallel: no other test may take the lock meanwhile.
func copyWhileAForkHoldsTheLock(t *testing.T, mode os.FileMode) (waited bool, worktree string) {
	t.Helper()
	project := t.TempDir()
	worktree = t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "tool"), []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
		t.Fatal(err)
	}
	syscall.ForkLock.Lock()
	done := make(chan error, 1)
	go func() { done <- copyEntries(context.Background(), project, worktree, []string{"tool"}) }()
	buf := make([]byte, 1<<20)
	for {
		select {
		case err := <-done:
			syscall.ForkLock.Unlock()
			if err != nil {
				t.Fatalf("copy: %v", err)
			}
			return waited, worktree
		default:
		}
		if !waited && copyParkedOnForkLock(buf[:runtime.Stack(buf, true)]) {
			waited = true
			syscall.ForkLock.Unlock()
			if err := <-done; err != nil {
				t.Fatalf("copy: %v", err)
			}
			return waited, worktree
		}
		runtime.Gosched()
	}
}

// copyParkedOnForkLock reports a goroutine parked acquiring an RWMutex for
// reading from inside the copy step.
func copyParkedOnForkLock(stacks []byte) bool {
	for _, stack := range strings.Split(string(stacks), "\n\n") {
		if strings.Contains(stack, "worktreesetup.copyPath") &&
			strings.Contains(stack, "sync.(*RWMutex).RLock") {
			return true
		}
	}
	return false
}

// A setup command can run a copied executable right after the copy. A
// process started while the file was open for writing would keep it busy
// and the command would fail with "text file busy", so the copy holds
// process starts off.
func TestCopyWritesAnExecutableWithForksHeldOff(t *testing.T) {
	waited, worktree := copyWhileAForkHoldsTheLock(t, 0o755)
	if !waited {
		t.Fatal("an executable was copied while a fork held ForkLock")
	}
	info, err := os.Stat(filepath.Join(worktree, "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("copied mode = %v, want 0755", info.Mode().Perm())
	}
}

// A file nothing can execute is copied without making process starts wait.
func TestCopyWritesOtherFilesWithoutHoldingForks(t *testing.T) {
	if waited, _ := copyWhileAForkHoldsTheLock(t, 0o644); waited {
		t.Fatal("a non-executable copy waited for ForkLock")
	}
}
