//go:build unix

package textgen

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/procutil"
	"agent-overflow/internal/procutil/procutiltest"
)

// groupRun runs script as a CLI whose processes are checked as its admission
// is released: the script writes its pid, the group's id, to "leader" in dir
// and the pid of the child it starts to "child". It returns the run's error,
// its result and what signal 0 to the group and then to the child answered at
// the release.
func groupRun(t *testing.T, script string, stopWhen string) (CLIResult, error, error) {
	t.Helper()
	dir := t.TempDir()
	leader := filepath.Join(dir, "leader")
	child := filepath.Join(dir, "child")
	t.Cleanup(func() {
		// A failing run leaves the child behind; the test does not.
		if pgid, err := readPID(leader); err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
		if pid, err := readPID(child); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stopped := make(chan func(error), 1)
	atRelease := make(chan error, 1)
	type answer struct {
		result CLIResult
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		result, err := ExecCLI(ctx, procutiltest.Reaper(), CLISpec{
			Binary: "/bin/sh",
			Args:   []string{"-c", script, dir},
			Admit: func(_ context.Context, stop func(error)) (func(), error) {
				stopped <- stop
				return func() {
					pgid, err := readPID(leader)
					if err != nil {
						atRelease <- err
						return
					}
					if err := procutil.SignalGroup(pgid, 0); !errors.Is(err, os.ErrProcessDone) {
						atRelease <- err
						return
					}
					pid, err := readPID(child)
					if err != nil {
						atRelease <- err
						return
					}
					// The child is checked by its own pid too: a run that is
					// not its own group has no group of the leader's id.
					if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
						atRelease <- os.ErrProcessDone
						return
					}
					atRelease <- fmt.Errorf("the child %d is alive", pid)
				}, nil
			},
		})
		done <- answer{result, err}
	}()
	stop := <-stopped
	if stopWhen != "" {
		waitForFile(t, filepath.Join(dir, stopWhen))
		stop(errMovedForTest)
	}
	got := <-done
	return got.result, got.err, <-atRelease
}

var errMovedForTest = errors.New("moved")

func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

// A wrapper that starts a long-lived child without exec: the admission's stop
// ends the child with the wrapper, and the run releases its admission only
// once no process of its group is left.
func TestExecCLI_TheStopEndsTheWholeGroupBeforeTheRelease(t *testing.T) {
	_, err, group := groupRun(t,
		`echo $$ > "$0/leader"; sleep 300 & echo $! > "$0/child"; : > "$0/started"; wait`, "started")
	if !errors.Is(err, errMovedForTest) {
		t.Fatalf("ExecCLI = %v, want the stop's cause", err)
	}
	if !errors.Is(group, os.ErrProcessDone) {
		t.Fatalf("a process of the run's group was alive at the release (signal 0: %v)", group)
	}
}

// A helper the CLI leaves running when it exits on its own is stopped before
// the run returns, and the run keeps the CLI's own answer.
func TestExecCLI_AHelperTheCLILeavesEndsBeforeTheRelease(t *testing.T) {
	result, err, group := groupRun(t,
		`echo $$ > "$0/leader"; sleep 300 & echo $! > "$0/child"; echo answer`, "")
	if err != nil || result.ExitCode != 0 || strings.TrimSpace(result.Stdout) != "answer" {
		t.Fatalf("ExecCLI = %+v, %v; want the CLI's answer", result, err)
	}
	if !errors.Is(group, os.ErrProcessDone) {
		t.Fatalf("a process of the run's group was alive at the release (signal 0: %v)", group)
	}
}

// A process that left the CLI's group still holding its output means what the
// run captured may be cut short, so the run fails instead of returning it.
func TestExecCLI_OutputAnEscapedProcessHoldsFailsTheRun(t *testing.T) {
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("perl, which this test leaves the group with, is not installed")
	}
	dir := t.TempDir()
	escaped := filepath.Join(dir, "escaped")
	t.Cleanup(func() {
		if pid, err := readPID(escaped); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	_, err := ExecCLI(context.Background(), procutiltest.Reaper(), CLISpec{
		Binary: "/bin/sh",
		Args: []string{"-c",
			`perl -MPOSIX -e 'POSIX::setsid(); open(F, ">", $ARGV[0]); close F; sleep 300' "$0/left" &
echo $! > "$0/escaped"
while [ ! -e "$0/left" ]; do sleep 0.01; done
echo answer`, dir},
	})
	if !errors.Is(err, procutil.ErrOutputHeld) {
		t.Fatalf("ExecCLI = %v, want the run failed for its held output", err)
	}
}

// The prompt reaches the CLI on its standard input.
func TestExecCLI_FeedsThePromptOnStdin(t *testing.T) {
	result, err := ExecCLI(context.Background(), procutiltest.Reaper(), CLISpec{
		Binary: "/bin/sh",
		Args:   []string{"-c", "cat"},
		Stdin:  "the prompt",
	})
	if err != nil || result.Stdout != "the prompt" {
		t.Fatalf("ExecCLI = %+v, %v; want the prompt echoed from stdin", result, err)
	}
}
