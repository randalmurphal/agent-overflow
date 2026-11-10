//go:build unix && !noremote

package acmecert

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/procutil"
)

const hookTestName = "_acme-challenge.backend.example"

// hookPID waits for the pid the hook script wrote and kills that process at
// cleanup, so a failing test leaves nothing running.
func hookPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil && strings.HasSuffix(string(data), "\n") {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr != nil {
				t.Fatalf("pid file contains %q", data)
			}
			t.Cleanup(func() {
				if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
					t.Errorf("kill process %d: %v", pid, err)
				}
			})
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("the hook never wrote %s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// hookProcessRunning reports a process that has not exited. An exited process
// nobody has reaped yet still answers signal 0.
func hookProcessRunning(pid int) bool {
	return syscall.Kill(pid, 0) == nil && !procutil.Exited(pid)
}

// A hook that traps the stop signal and exits 0 did not do its work in time.
func TestAHookThatAnswersTheTimeoutWithACleanExitStillTimedOut(t *testing.T) {
	err := runHook(context.Background(),
		[]string{"/bin/sh", "-c", `trap 'exit 0' TERM; while :; do sleep 0.05; done`, "dns-hook"},
		300*time.Millisecond, hookSet, hookTestName, "the-value")
	requireNames(t, err, "timed out after 300ms")
}

func TestTheTimeoutStopsWhatTheHookStarted(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "pid")
	done := make(chan error, 1)
	go func() {
		done <- runHook(context.Background(),
			[]string{"/bin/sh", "-c", `sleep 300 & echo $! > "` + pidPath + `"; wait`, "dns-hook"},
			500*time.Millisecond, hookSet, hookTestName, "the-value")
	}()
	pid := hookPID(t, pidPath)
	requireNames(t, <-done, "timed out")
	if hookProcessRunning(pid) {
		t.Fatalf("process %d outlived the hook's timeout", pid)
	}
}

func TestAProcessTheHookLeftRunningIsStoppedWhenTheHookReturns(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "pid")
	err := runHook(context.Background(),
		[]string{"/bin/sh", "-c", `sleep 300 >/dev/null 2>&1 & echo $! > "` + pidPath + `"`, "dns-hook"},
		time.Minute, hookSet, hookTestName, "the-value")
	if err != nil {
		t.Fatalf("a hook that exited 0: %v", err)
	}
	if pid := hookPID(t, pidPath); hookProcessRunning(pid) {
		t.Fatalf("process %d outlived the hook", pid)
	}
}

// A cancelled renewal is not a hook that failed on its own, and not a timeout.
func TestACancelledHookReportsTheCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pidPath := filepath.Join(t.TempDir(), "pid")
	done := make(chan error, 1)
	go func() {
		done <- runHook(ctx,
			[]string{"/bin/sh", "-c", `trap 'exit 0' TERM; echo $$ > "` + pidPath + `"; while :; do sleep 0.05; done`, "dns-hook"},
			time.Minute, hookSet, hookTestName, "the-value")
	}()
	hookPID(t, pidPath)
	cancel()
	err := <-done
	requireNames(t, err, "failed: context canceled")
	if strings.Contains(err.Error(), "timed out") {
		t.Fatalf("a cancellation was reported as a timeout: %v", err)
	}
}

func stubRunGroup(t *testing.T, outcome procutil.GroupOutcome, err error) {
	t.Helper()
	original := runGroup
	runGroup = func(context.Context, procutil.GroupCommand) (procutil.GroupOutcome, error) {
		return outcome, err
	}
	t.Cleanup(func() { runGroup = original })
}

func TestAHookThatCannotBeStoppedIsReportedAsStillRunning(t *testing.T) {
	stubRunGroup(t, procutil.GroupOutcome{ExitCode: -1, Stopped: true}, procutil.ErrGroupSurvived)
	err := runHook(context.Background(), []string{"dns-hook"}, time.Minute, hookSet, hookTestName, "the-value")
	requireNames(t, err, "could not be stopped and is still running")
}

func TestAFailingHookAlsoReportsAnOutputPathFailure(t *testing.T) {
	stubRunGroup(t, procutil.GroupOutcome{ExitCode: 3, PipeErr: errors.New("the output pipe did not close")}, errors.New("exit status 3"))
	err := runHook(context.Background(), []string{"dns-hook"}, time.Minute, hookSet, hookTestName, "the-value")
	requireNames(t, err, "exit status 3")
	requireNames(t, err, "the output pipe did not close")
}

func TestAHookThatSucceededIsNotFailedForItsOutputPath(t *testing.T) {
	stubRunGroup(t, procutil.GroupOutcome{PipeErr: errors.New("the output pipe did not close")}, nil)
	if err := runHook(context.Background(), []string{"dns-hook"}, time.Minute, hookSet, hookTestName, "the-value"); err != nil {
		t.Fatalf("a hook that exited 0: %v", err)
	}
}
