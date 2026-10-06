//go:build darwin

package app

import (
	"os"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/orphanreaper"

	"github.com/shirou/gopsutil/v4/process"
)

// The sidecar Start spawns re-execs this binary, so it acts as the reaper
// only when TestMain dispatches the subcommand as main() does. It then
// exits on the control pipe's EOF, and Close returns promptly.
func TestOrphanReaperSidecarRunsAsTheReaper(t *testing.T) {
	t.Parallel()
	a := NewApp()
	if err := a.startOrphanReaper(t.TempDir()); err != nil {
		t.Fatalf("startOrphanReaper: %v", err)
	}
	if a.orphanReaper == nil {
		t.Fatal("startOrphanReaper left no sidecar")
	}
	sidecars := reaperSidecarPIDs(t)
	closed := make(chan error, 1)
	go func() { closed <- a.orphanReaper.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("sidecar exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		// A sidecar that is not the reaper is running this suite. Its group
		// is its own (Setpgid), so ending it ends what it started.
		for _, pid := range sidecars {
			if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
				t.Errorf("kill sidecar group %d: %v", pid, err)
			}
		}
		t.Fatal("the sidecar did not exit on EOF; TestMain must dispatch the reaper subcommand")
	}
}

func reaperSidecarPIDs(t *testing.T) []int {
	t.Helper()
	self, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	children, err := self.Children()
	if err != nil {
		t.Fatalf("list child processes: %v", err)
	}
	var pids []int
	for _, child := range children {
		args, err := child.CmdlineSlice()
		if err != nil {
			// Another test's child that exited meanwhile is not the sidecar.
			continue
		}
		if len(args) > 1 && args[1] == orphanreaper.Subcommand() {
			pids = append(pids, int(child.Pid))
		}
	}
	if len(pids) == 0 {
		t.Fatal("found no reaper sidecar among this process's children")
	}
	return pids
}
