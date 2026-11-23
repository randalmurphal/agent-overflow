//go:build !windows

package orphanreaper

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/testutil/helperproc"
)

// Spawn hands the sidecar its script in a pipe it fills before the sidecar
// starts, so the script must fit a new pipe: a script grown past that fails
// here and not at a launch. Text that does not fit is refused, not left to
// block a write that has no reader.
func TestTheSidecarScriptFitsANewPipe(t *testing.T) {
	script, err := loadedPipe(sidecarScript)
	if err != nil {
		t.Fatalf("load the sidecar's script: %v", err)
	}
	got, err := io.ReadAll(script)
	if err := errors.Join(err, script.Close()); err != nil || string(got) != sidecarScript {
		t.Fatalf("the pipe held %d bytes (%v), want the script's %d and EOF", len(got), err, len(sidecarScript))
	}
	if _, err := loadedPipe(strings.Repeat("x", minPipeCapacity+1)); err == nil {
		t.Fatal("text past a new pipe's capacity was loaded")
	}
}

// spawnRoleEnv makes TestSpawnRole a process of the test below: "spawner"
// stands in for a backend that starts its sidecar, watches the group
// spawnWatchEnv names and is killed at once; "watched" leads that group
// until a signal ends it or the test binary is gone.
const (
	spawnRoleEnv  = "ORPHANREAPER_TEST_SPAWN_ROLE"
	spawnWatchEnv = "ORPHANREAPER_TEST_SPAWN_WATCH"
)

// TestSpawnRole is not a test of its own.
func TestSpawnRole(t *testing.T) {
	switch os.Getenv(spawnRoleEnv) {
	case "spawner":
		pgid, err := strconv.Atoi(os.Getenv(spawnWatchEnv))
		if err != nil {
			t.Fatal(err)
		}
		// One P, so nothing Spawn leaves to another goroutine can run
		// before the kill.
		runtime.GOMAXPROCS(1)
		client, err := Spawn()
		if err != nil {
			t.Fatal(err)
		}
		client.Watch(pgid)
		fmt.Printf("sidecar %d\n", client.cmd.Process.Pid)
		if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
	case "watched":
		helperproc.Hold()
	default:
		t.Skip("runs only as a process of this file's tests")
	}
}

// A backend killed the instant Spawn returns still has the group it watched
// ended: once Spawn returns, the sidecar needs nothing more from the
// process that started it.
func TestASidecarEndsTheGroupsOfAParentKilledAsSpawnReturns(t *testing.T) {
	watched := exec.Command(os.Args[0], "-test.run=^TestSpawnRole$")
	watched.Env = append(os.Environ(), spawnRoleEnv+"=watched")
	watched.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	helperproc.Tie(t, watched)
	if err := watched.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := watched.Process.Pid
	ended := make(chan error, 1)
	go func() { ended <- watched.Wait() }()
	t.Cleanup(func() {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("end the watched group %d: %v", pgid, err)
		}
		<-ended
	})

	spawner := exec.Command(os.Args[0], "-test.run=^TestSpawnRole$")
	spawner.Env = append(os.Environ(), spawnRoleEnv+"=spawner", spawnWatchEnv+"="+strconv.Itoa(pgid))
	stdout, err := spawner.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// A file, not a pipe of this process's: the sidecar inherits it and
	// outlives the spawner.
	spawner.Stderr = os.Stderr
	helperproc.Tie(t, spawner)
	if err := spawner.Start(); err != nil {
		t.Fatal(err)
	}
	out, readErr := io.ReadAll(stdout)
	waitErr := spawner.Wait()
	sidecar := 0
	if rest, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "sidecar "); ok {
		sidecar, _ = strconv.Atoi(rest)
	}
	if exit, ok := errors.AsType[*exec.ExitError](waitErr); readErr != nil || sidecar <= 1 || !ok ||
		exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("the spawner printed %q (%v) and ended with %v, want its sidecar and SIGKILL", out, readErr, waitErr)
	}
	// The sidecar leads its own group, with the sleep of its grace.
	t.Cleanup(func() {
		if err := syscall.Kill(-sidecar, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("end the sidecar's group %d: %v", sidecar, err)
		}
		deadline := time.Now().Add(15 * time.Second)
		for syscall.Kill(-sidecar, 0) == nil {
			if time.Now().After(deadline) {
				t.Errorf("the sidecar's group %d outlived its SIGKILL", sidecar)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	select {
	case err := <-ended:
		ended <- err
		if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
			t.Fatalf("the watched group's leader ended with %v, want the sidecar's SIGTERM", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the watched group outlived a parent killed as Spawn returned")
	}
}
