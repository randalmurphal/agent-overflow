//go:build !windows

package orphanreaper

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The sidecar's stdin holds the whole script before the sidecar starts:
// reading it to EOF needs nothing else to write, so nothing this process
// does after the start, or fails to do because it died, can cut it short.
// A script larger than a pipe would hold is handed over the same way, and
// the file leaves no name behind.
func TestTheSidecarsInputHoldsItsWholeScriptBeforeTheStart(t *testing.T) {
	for name, script := range map[string]string{
		"the sidecar's script":        sidecarScript,
		"a script larger than a pipe": strings.Repeat("#", 2<<20), // Linux pipes default to 64 KiB.
	} {
		t.Run(name, func(t *testing.T) {
			input, err := scriptInput(script)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = input.Close() })
			if _, err := os.Stat(input.Name()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the script file is still named %s: %v", input.Name(), err)
			}
			cmd := sidecarCommand(input, nil, time.Second)
			if cmd.Stdin != input {
				t.Fatalf("stdin = %T, want the script's file", cmd.Stdin)
			}
			got, err := io.ReadAll(input)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != script {
				t.Fatalf("the input holds %d bytes, want the script's %d", len(got), len(script))
			}
		})
	}
}

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
// killed right after its first Watch still has the sidecar end that group.
func TestTheSidecarReapsWhenItsParentDiesAsSpawnReturns(t *testing.T) {
	victim := startGroup(t, "echo ready; exec sleep 120")
	// A file, not a pipe: the sidecar inherits the parent's stderr and
	// outlives the parent, so a pipe would end only with the sidecar.
	outputPath := filepath.Join(t.TempDir(), "parent-output")
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
	parent := exec.Command(os.Args[0], "-test.run=^TestSpawnParentHelper$")
	// One P: nothing Spawn leaves for this process to finish later gets to
	// run before the kill.
	parent.Env = append(os.Environ(), spawnParentHelperEnv+"="+strconv.Itoa(victim.Process.Pid), "GOMAXPROCS=1")
	parent.Stdout, parent.Stderr = output, output
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parent.Process.Kill() })
	parentDone := make(chan error, 1)
	go func() { parentDone <- parent.Wait() }()
	victimDone := make(chan error, 1)
	go func() { victimDone <- victim.Wait() }()

	deadline := time.After(10 * time.Second)
	for parentDone != nil || victimDone != nil {
		select {
		case err := <-parentDone:
			parentDone = nil
			if sig, ok := endingSignal(err); !ok || sig != syscall.SIGKILL {
				out, _ := os.ReadFile(outputPath)
				t.Fatalf("the parent did not die by its own SIGKILL: %v\n%s", err, out)
			}
		case err := <-victimDone:
			victimDone = nil
			// Only the sidecar signals the group before this test ends.
			if sig, ok := endingSignal(err); !ok || (sig != syscall.SIGTERM && sig != syscall.SIGKILL) {
				t.Fatalf("the watched group's leader ended by itself (%v), not by the sidecar", err)
			}
		case <-deadline:
			out, _ := os.ReadFile(outputPath)
			t.Fatalf("after 10s: parent exited %v, the group it watched still running %v\n%s",
				parentDone == nil, victimDone != nil, out)
		}
	}
}

// endingSignal returns the signal that ended a process, from its Wait error.
func endingSignal(err error) (syscall.Signal, bool) {
	exit, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		return 0, false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return 0, false
	}
	return status.Signal(), true
}
