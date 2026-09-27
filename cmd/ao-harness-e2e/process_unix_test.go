//go:build !windows

package main

import (
	"os/exec"
	"testing"
	"time"

	"agent-overflow/internal/harness/instanceinfo"
)

const teardownBudget = 15 * time.Second

// startRoot starts script as a process-group root the way the launcher
// starts Playwright, records its identity at once, and reaps it
// concurrently as run does.
func startRoot(t *testing.T, script string) (*exec.Cmd, instanceinfo.ProcessIdentity, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	configureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := instanceinfo.CaptureProcessIdentity(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	return cmd, identity, done
}

func awaitExit(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(teardownBudget):
		t.Fatal("the root outlived the verified teardown")
	}
}

// awaitExec waits until the root runs a different executable than the one
// recorded at launch.
func awaitExec(t *testing.T, pid int, launched instanceinfo.ProcessIdentity) {
	t.Helper()
	deadline := time.Now().Add(teardownBudget)
	for time.Now().Before(deadline) {
		current, err := instanceinfo.CaptureProcessIdentity(pid)
		if err != nil {
			t.Fatal(err)
		}
		if current.Executable != launched.Executable {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the root never exec'd")
}

// The safety stop must reach a root that exec'd after its identity was
// recorded, as pnpm's `env node` chain does.
func TestVerifiedTeardownStopsARootThatExecdAfterLaunch(t *testing.T) {
	cmd, launched, done := startRoot(t, "sleep 0.3; exec sleep 30")
	awaitExec(t, cmd.Process.Pid, launched)
	if err := terminateProcessTreeVerified(cmd, nil, launched); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	awaitExit(t, done)
}

// A root that execs while the teardown is in progress, here from its TERM
// trap, is still stopped under its new executable.
func TestVerifiedTeardownFollowsARootThatExecsDuringTeardown(t *testing.T) {
	cmd, launched, done := startRoot(t, `trap 'exec sleep 30' TERM; while :; do sleep 0.05; done`)
	if err := terminateProcessTreeVerified(cmd, nil, launched); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	awaitExit(t, done)
}
