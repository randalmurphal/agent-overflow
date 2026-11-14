//go:build unix

package worktreesetup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/procutil"
)

// recordedPID reads the process id a setup command wrote with `echo $! > pid`.
func recordedPID(t *testing.T, worktree string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(worktree, "pid"))
		if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && convErr == nil {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the setup command recorded no process id in %s", worktree)
	return 0
}

// running reports a process that has not exited. An exited process nobody
// has reaped yet still answers signal 0.
func running(pid int) bool {
	return syscall.Kill(pid, 0) == nil && !procutil.Exited(pid)
}

func waitForExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for running(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d is still running", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// killAtCleanup ends a process a test left running on purpose.
func killAtCleanup(t *testing.T, pid int) {
	t.Helper()
	t.Cleanup(func() {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("kill process %d: %v", pid, err)
		}
	})
}

// The timeout reaches a descendant that redirected its output away, which
// nothing but the group signal would find.
func TestRunTimeoutStopsADescendantThatLeftThePipe(t *testing.T) {
	worktree := t.TempDir()
	err := Run(context.Background(), t.TempDir(), worktree, Config{
		Run:     [][]string{{"/bin/sh", "-c", "sleep 60 >/dev/null 2>&1 & echo $! > pid; wait"}},
		Timeout: "300ms",
	})
	pid := recordedPID(t, worktree)
	killAtCleanup(t, pid)
	if err == nil || !strings.Contains(err.Error(), "timed out after 300ms") {
		t.Fatalf("timeout error = %v", err)
	}
	waitForExit(t, pid)
}

// A command that answers the stop signal with a clean exit was still stopped:
// the recipe did not succeed, and a deadline reads as a timeout.
func TestRunFailsACommandThatExitsCleanlyWhenStopped(t *testing.T) {
	script := `trap 'exit 0' TERM; echo ready; while :; do sleep 0.05; done`
	err := Run(context.Background(), t.TempDir(), t.TempDir(), Config{
		Run: [][]string{{"/bin/sh", "-c", script}}, Timeout: "300ms",
	})
	if err == nil || !strings.Contains(err.Error(), "timed out after 300ms") || !strings.Contains(err.Error(), "ready") {
		t.Fatalf("timeout error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observer := &outputSignal{seen: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- RunObserved(ctx, t.TempDir(), t.TempDir(), Config{Run: [][]string{{"/bin/sh", "-c", script}}}, observer)
	}()
	<-observer.seen
	cancel()
	err = <-done
	if err == nil || !strings.Contains(err.Error(), "failed: context canceled") || strings.Contains(err.Error(), "timed out") {
		t.Fatalf("cancellation error = %v", err)
	}
}

// outputSignal closes seen at a command's first output.
type outputSignal struct {
	noopObserver
	seen chan struct{}
	once sync.Once
}

func (o *outputSignal) Output(int, []byte) { o.once.Do(func() { close(o.seen) }) }

// A cancelled recipe starts no further command.
func TestRunStartsNoCommandUnderADoneContext(t *testing.T) {
	worktree := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Run(ctx, t.TempDir(), worktree, Config{
		Run: [][]string{{"/bin/sh", "-c", "echo ran > ran.txt"}},
	})
	if err == nil || !strings.Contains(err.Error(), "failed: context canceled") {
		t.Fatalf("cancelled recipe error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(worktree, "ran.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("a command ran under a done context: %v", statErr)
	}
}

// A process a successful command left running on purpose, with its output
// redirected away, survives the command and does not delay the next one.
func TestRunKeepsAProcessACommandLeftRunning(t *testing.T) {
	worktree := t.TempDir()
	started := time.Now()
	err := Run(context.Background(), t.TempDir(), worktree, Config{
		Run: [][]string{
			{"/bin/sh", "-c", "sleep 60 >/dev/null 2>&1 & echo $! > pid"},
			{"/bin/sh", "-c", "printf ran > second.txt"},
		},
	})
	elapsed := time.Since(started)
	pid := recordedPID(t, worktree)
	killAtCleanup(t, pid)
	if err != nil {
		t.Fatalf("recipe that left a process running = %v", err)
	}
	if !running(pid) {
		t.Fatal("the process the command left running was stopped")
	}
	if _, statErr := os.Stat(filepath.Join(worktree, "second.txt")); statErr != nil {
		t.Fatalf("the next command did not run: %v", statErr)
	}
	if elapsed >= time.Second {
		t.Fatalf("a leftover that does not hold the output delayed the recipe by %s", elapsed)
	}
}

// A process left running that still holds the command's output fails the
// command once the drain grace passes, and is left running.
func TestRunFailsACommandWhoseLeftoverHoldsItsOutput(t *testing.T) {
	worktree := t.TempDir()
	err := Run(context.Background(), t.TempDir(), worktree, Config{
		Run: [][]string{
			{"/bin/sh", "-c", "sleep 60 & echo $! > pid; echo leader-done"},
			{"/bin/sh", "-c", "printf ran > second.txt"},
		},
	})
	pid := recordedPID(t, worktree)
	killAtCleanup(t, pid)
	want := `failed: procutil: a process the command left running still holds its output; output tail: leader-done`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("held-output error = %v, want it to contain %q", err, want)
	}
	if !running(pid) {
		t.Fatal("the process the command left running was stopped")
	}
	if _, statErr := os.Stat(filepath.Join(worktree, "second.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("a command ran after the failure: %v", statErr)
	}
}

func TestRunReportsACommandThatCannotStart(t *testing.T) {
	err := Run(context.Background(), t.TempDir(), t.TempDir(), Config{
		Run: [][]string{{"/nonexistent/ao-setup-binary"}},
	})
	want := `worktree setup command ["/nonexistent/ao-setup-binary"] failed: ` +
		`fork/exec /nonexistent/ao-setup-binary: no such file or directory; output tail: `
	if err == nil || err.Error() != want {
		t.Fatalf("start failure = %v, want %q", err, want)
	}
}
