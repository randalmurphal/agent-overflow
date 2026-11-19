//go:build unix

package main

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
)

// hookChild reads the pid a hook wrote of the child it started.
func hookChild(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the child's pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("child pid %q: %v", raw, err)
	}
	return pid
}

// requireGone waits a bounded time for pid to stop existing.
func requireGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the hook's child %d is still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A hook's timeout ends what it started too, and a child holding its output
// does not hold the run open: a shell waiting on such a child returns at the
// timeout, and one that exits leaving it behind returns at once with its
// output, the child stopped.
func TestAHookRunEndsWhatItStarted(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		script  string
		timeout time.Duration
		bound   time.Duration
		stdout  string
		err     error
	}{
		{"waits on its child", "sleep 300 & echo $! > child; wait", time.Second, 4 * time.Second, "", context.DeadlineExceeded},
		{"leaves its child", "sleep 300 & echo $! > child; echo done", 30 * time.Second, 4 * time.Second, "done\n", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var runs hookRuns
			dir := t.TempDir()
			started := time.Now()
			run := runs.run(sessionStartHook{command: test.script, timeout: test.timeout}, dir, map[string]string{"source": "startup"})
			if took := time.Since(started); took > test.bound {
				t.Errorf("the run took %s, want under %s", took, test.bound)
			}
			if !errors.Is(run.err, test.err) || run.stdout != test.stdout {
				t.Errorf("run = err %v, stdout %q; want %v, %q", run.err, run.stdout, test.err, test.stdout)
			}
			requireGone(t, hookChild(t, filepath.Join(dir, "child")))
		})
	}
}

// Stopping the runs, as terminate does before the process exits, ends one
// in flight and refuses the next.
func TestStoppingHookRunsEndsThemBeforeTheExit(t *testing.T) {
	t.Parallel()
	var runs hookRuns
	dir := t.TempDir()
	ran := make(chan sessionStartRun, 1)
	go func() {
		ran <- runs.run(sessionStartHook{command: "sleep 300 & echo $! > child; wait", timeout: time.Minute}, dir, nil)
	}()
	childFile := filepath.Join(dir, "child")
	for deadline := time.Now().Add(5 * time.Second); ; {
		if raw, err := os.ReadFile(childFile); err == nil && strings.HasSuffix(string(raw), "\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the hook did not start its child")
		}
		time.Sleep(20 * time.Millisecond)
	}
	started := time.Now()
	runs.stop()
	if took := time.Since(started); took > 4*time.Second {
		t.Errorf("stop took %s", took)
	}
	select {
	case run := <-ran:
		if !errors.Is(run.err, context.Canceled) {
			t.Errorf("stopped run err = %v, want cancelled", run.err)
		}
	default:
		t.Fatal("stop returned before the run ended")
	}
	requireGone(t, hookChild(t, childFile))
	if run := runs.run(sessionStartHook{command: "true", timeout: time.Second}, dir, nil); !errors.Is(run.err, errHooksStopped) {
		t.Errorf("a run after the stop = %v, want refused", run.err)
	}
}
