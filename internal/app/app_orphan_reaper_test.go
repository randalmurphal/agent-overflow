//go:build !windows

package app

import (
	"os"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/orphanreaper/orphanreapertest"
)

// The sidecar Start spawns exits cleanly on the control pipe's EOF, and Close
// returns promptly.
func TestOrphanReaperSidecarExitsOnClose(t *testing.T) {
	t.Parallel()
	a := NewApp()
	if err := a.startOrphanReaper(t.TempDir()); err != nil {
		t.Fatalf("startOrphanReaper: %v", err)
	}
	if a.orphanReaper == nil {
		t.Fatal("startOrphanReaper left no sidecar")
	}
	sidecars := orphanreapertest.Sidecars(t, os.Getpid())
	if len(sidecars) == 0 {
		t.Fatal("found no reaper sidecar among this process's children")
	}
	closed := make(chan error, 1)
	go func() { closed <- a.orphanReaper.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("sidecar exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		// Its group is its own (Setpgid), so ending it ends what it started.
		for _, pid := range sidecars {
			if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
				t.Errorf("kill sidecar group %d: %v", pid, err)
			}
		}
		t.Fatal("the sidecar did not exit on EOF")
	}
}
