//go:build !windows

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
	// The trap execs within milliseconds of SIGTERM; the grace only has to
	// outlast that so the kill meets the new executable.
	previous := teardownGrace
	teardownGrace = 500 * time.Millisecond
	t.Cleanup(func() { teardownGrace = previous })
	cmd, launched, done := startRoot(t, `trap 'exec sleep 30' TERM; while :; do sleep 0.05; done`)
	if err := terminateProcessTreeVerified(cmd, nil, launched); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	awaitExit(t, done)
}

// A signal to the launcher stops the suite's whole tree, a descendant that
// outlives its parent's death included, and is reported only after that
// teardown has finished: Playwright runs in its own process group, so
// nothing else would stop it.
func TestASignalStopsTheSuiteTreeBeforeItIsReported(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant")
	cmd, identity, done := startRoot(t, `sleep 300 & echo $! > `+pidFile+`; wait`)
	var descendant int
	deadline := time.Now().Add(teardownBudget)
	for descendant == 0 {
		if body, err := os.ReadFile(pidFile); err == nil && strings.TrimSpace(string(body)) != "" {
			descendant, _ = strconv.Atoi(strings.TrimSpace(string(body)))
		}
		if time.Now().After(deadline) {
			t.Fatal("the descendant never started")
		}
		time.Sleep(10 * time.Millisecond)
	}

	signals := make(chan os.Signal, 1)
	halted := make(chan struct{})
	got := haltOnSignal(context.Background(), signals, func(os.Signal) {
		if err := terminateProcessTreeVerified(cmd, nil, identity); err != nil {
			t.Errorf("terminate: %v", err)
		}
		close(halted)
	})
	signals <- syscall.SIGTERM
	sig := <-got
	select {
	case <-halted:
	default:
		t.Fatal("the signal was reported before its teardown finished")
	}
	if sig != syscall.SIGTERM || signalExitCode(sig) != 143 {
		t.Fatalf("reported %v with exit %d, want SIGTERM and 143", sig, signalExitCode(sig))
	}
	awaitExit(t, done)
	if !waitForProcessTree(cmd, nil, teardownBudget) {
		t.Fatal("the suite's process group survived the signal")
	}
	if err := syscall.Kill(descendant, 0); err == nil {
		t.Fatalf("descendant %d survived the signal", descendant)
	}
}

// Without a signal the watcher stops with the suite and reports nothing.
func TestNoSignalReportsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	got := haltOnSignal(ctx, make(chan os.Signal), func(os.Signal) { t.Error("halted without a signal") })
	cancel()
	if sig := <-got; sig != nil {
		t.Fatalf("reported %v without a signal", sig)
	}
}
