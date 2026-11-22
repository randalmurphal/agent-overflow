//go:build !windows

package orphanreaper

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// spawnParentHelperEnv makes TestSpawnParentHelper the parent that spawns a
// sidecar, has it watch a group and dies at once. Its value is the group.
const spawnParentHelperEnv = "ORPHANREAPER_SPAWN_PARENT_HELPER"

// TestSpawnParentHelper is not a test of its own: the test below runs this
// binary as the parent that dies the moment Spawn returns.
func TestSpawnParentHelper(t *testing.T) {
	pgid, err := strconv.Atoi(os.Getenv(spawnParentHelperEnv))
	if err != nil {
		t.Skip("runs only as the parent TestTheSidecarReapsWhenItsParentDiesAsSpawnReturns kills")
	}
	sidecar, err := Spawn()
	if err != nil {
		t.Fatal(err)
	}
	sidecar.Watch(pgid)
	if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {}
}

// Spawn's sidecar guards the parent from the moment Spawn returns: a parent
// killed right after its first Watch still has that group ended.
func TestTheSidecarReapsWhenItsParentDiesAsSpawnReturns(t *testing.T) {
	victim := startGroup(t, "echo ready; exec sleep 120")
	parent := exec.Command(os.Args[0], "-test.run=^TestSpawnParentHelper$")
	// One P: nothing Spawn leaves for this process to finish later gets to
	// run before the kill.
	parent.Env = append(os.Environ(), spawnParentHelperEnv+"="+strconv.Itoa(victim.Process.Pid), "GOMAXPROCS=1")
	out, err := parent.CombinedOutput()
	if exit, ok := errors.AsType[*exec.ExitError](err); !ok ||
		exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("the parent did not die by its own SIGKILL: %v\n%s", err, out)
	}
	reaped := make(chan struct{})
	go func() {
		_ = victim.Wait()
		close(reaped)
	}()
	select {
	case <-reaped:
	case <-time.After(5 * time.Second):
		t.Fatal("the group the dead parent watched is still running")
	}
}
