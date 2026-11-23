//go:build !windows

// Package orphanreapertest finds the orphan reaper sidecars a test causes. A
// sidecar whose backend died sends SIGTERM to the groups it watched and then
// waits its grace before the SIGKILL, so it outlives the test that killed the
// backend unless the test ends it (helperproc.EndGroup). Production code
// must not import it.
package orphanreapertest

import (
	"slices"
	"syscall"
	"testing"

	"agent-overflow/internal/orphanreaper"

	"github.com/shirou/gopsutil/v4/process"
)

// Sidecars returns the orphan reaper sidecars that are children of parent.
func Sidecars(t testing.TB, parent int) []int {
	t.Helper()
	proc, err := process.NewProcess(int32(parent))
	if err != nil {
		t.Fatal(err)
	}
	children, err := proc.Children()
	if err != nil {
		t.Fatalf("list the children of %d: %v", parent, err)
	}
	var pids []int
	for _, child := range children {
		// A child that exited meanwhile is not a sidecar.
		if args, err := child.CmdlineSlice(); err == nil && slices.Contains(args, orphanreaper.SidecarName) {
			pids = append(pids, int(child.Pid))
		}
	}
	return pids
}

// Of returns the one sidecar backend started, which leads its own group.
// Call it while backend is alive: once backend dies, its sidecar is another
// process's child.
func Of(t testing.TB, backend int) int {
	t.Helper()
	sidecars := Sidecars(t, backend)
	if len(sidecars) != 1 {
		t.Fatalf("backend %d has sidecars %v, want one", backend, sidecars)
	}
	sidecar := sidecars[0]
	// A signal to the backend's group must not end the sidecar.
	if pgid, err := syscall.Getpgid(sidecar); err != nil || pgid != sidecar {
		t.Fatalf("the sidecar is in group %d %v, want its own", pgid, err)
	}
	return sidecar
}
