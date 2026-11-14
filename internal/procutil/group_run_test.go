//go:build !windows

package procutil

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func shellCommand(t *testing.T, script string, output *syncBuffer) GroupCommand {
	t.Helper()
	return GroupCommand{
		Dir:    t.TempDir(),
		Env:    []string{"PATH=" + os.Getenv("PATH")},
		Argv:   []string{"/bin/sh", "-c", script},
		Output: output,
	}
}

func waitForOutput(t *testing.T, output *syncBuffer, marker string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(output.String(), marker) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in %q", marker, output.String())
}

// recordedPID reads the process id a script wrote with `echo $! > file`.
func recordedPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && convErr == nil {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no process id was recorded at %s", path)
	return 0
}

// running reports a process that has not exited. An exited process nobody
// has reaped yet still answers signal 0.
func running(pid int) bool {
	return syscall.Kill(pid, 0) == nil && !Exited(pid)
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

func TestRunGroupReportsTheExitCodeAndCombinedOutput(t *testing.T) {
	t.Parallel()
	var output syncBuffer
	outcome, err := RunGroup(context.Background(),
		shellCommand(t, "echo to-stdout; echo to-stderr 1>&2; exit 3", &output))
	if err == nil || outcome.ExitCode != 3 || outcome.Leftovers || outcome.Stopped || outcome.OutputHeld {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	if text := output.String(); !strings.Contains(text, "to-stdout") || !strings.Contains(text, "to-stderr") {
		t.Fatalf("output lost a stream: %q", text)
	}
}

func TestRunGroupRefusesAnEmptyArgv(t *testing.T) {
	t.Parallel()
	if _, err := RunGroup(context.Background(), GroupCommand{Output: &syncBuffer{}}); err == nil {
		t.Fatal("empty argv was accepted")
	}
}

func TestRunGroupReportsAStartFailure(t *testing.T) {
	t.Parallel()
	outcome, err := RunGroup(context.Background(), GroupCommand{
		Dir: t.TempDir(), Argv: []string{"/nonexistent/ao-procutil-binary"}, Output: &syncBuffer{},
	})
	if err == nil || outcome.ExitCode != -1 {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
}

// The start failure is the caller's before anything is waited on, with
// os/exec's own text.
func TestStartGroupReportsAStartFailureSynchronously(t *testing.T) {
	t.Parallel()
	group, err := StartGroup(context.Background(), GroupCommand{
		Dir: t.TempDir(), Argv: []string{"/nonexistent/ao-procutil-binary"}, Output: &syncBuffer{},
	})
	if group != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("group=%v err=%v", group, err)
	}
	if want := "fork/exec /nonexistent/ao-procutil-binary: no such file or directory"; err.Error() != want {
		t.Fatalf("start error = %q, want %q", err, want)
	}
}

func TestStartGroupStartsNothingUnderADoneContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output syncBuffer
	command := shellCommand(t, "echo ran > ran.txt", &output)
	outcome, err := RunGroup(ctx, command)
	if !errors.Is(err, context.Canceled) || outcome.ExitCode != -1 {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	if _, statErr := os.Stat(filepath.Join(command.Dir, "ran.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the command ran under a done context: %v", statErr)
	}
}

// Output is drained from the start, so a caller that has not reached Wait
// yet still sees it, and every Wait reports the one result.
func TestStartGroupDrainsBeforeWaitAndWaitRepeatsItsResult(t *testing.T) {
	t.Parallel()
	var output syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	group, err := StartGroup(ctx, shellCommand(t,
		`trap 'exit 7' TERM; echo early; while :; do sleep 0.05; done`, &output))
	if err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, &output, "early")
	cancel()
	first, firstErr := group.Wait()
	if first.ExitCode != 7 || !first.Stopped || firstErr == nil {
		t.Fatalf("outcome=%+v err=%v", first, firstErr)
	}
	second, secondErr := group.Wait()
	if second != first || !errors.Is(secondErr, firstErr) {
		t.Fatalf("second Wait = %+v, %v; first = %+v, %v", second, secondErr, first, firstErr)
	}
}

func TestStartGroupSurvivesRepeatedStartAndStop(t *testing.T) {
	t.Parallel()
	for round := range 10 {
		var output syncBuffer
		ctx, cancel := context.WithCancel(context.Background())
		group, err := StartGroup(ctx, shellCommand(t, `echo up; sleep 60 & wait`, &output))
		if err != nil {
			cancel()
			t.Fatalf("round %d: %v", round, err)
		}
		waitForOutput(t, &output, "up")
		cancel()
		outcome, err := group.Wait()
		if err == nil || !outcome.Stopped || outcome.ExitCode != -1 || outcome.OutputHeld {
			t.Fatalf("round %d: outcome=%+v err=%v", round, outcome, err)
		}
	}
}

// A command that exits while a child it started keeps running finishes with
// its own exit code, its child's output captured and the child stopped.
func TestRunGroupStopsLeftoverProcessesAndKeepsExitCode(t *testing.T) {
	t.Parallel()
	var output syncBuffer
	started := time.Now()
	outcome, err := RunGroup(context.Background(), shellCommand(t,
		`sh -c 'echo worker-started; while :; do sleep 0.05; echo worker-alive; done' &
sleep 0.3; echo leader-done`, &output))
	if err != nil || outcome.ExitCode != 0 || !outcome.Leftovers || outcome.OutputHeld || outcome.Stopped {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	if elapsed := time.Since(started); elapsed > DefaultTerminateGrace+groupDrainGrace+5*time.Second {
		t.Fatalf("leftover sweep took %s", elapsed)
	}
	text := output.String()
	if !strings.Contains(text, "leader-done") || !strings.Contains(text, "worker-started") {
		t.Fatalf("output lost the leader or its child: %q", text)
	}
}

// KeepLeftovers leaves a process the command started on purpose running, and
// a leftover that does not hold the output pipe does not delay the result.
func TestRunGroupKeepsLeftoversWhenAsked(t *testing.T) {
	t.Parallel()
	var output syncBuffer
	command := shellCommand(t, `sleep 60 >/dev/null 2>&1 & echo $! > pid; echo leader-done`, &output)
	command.KeepLeftovers = true
	started := time.Now()
	outcome, err := RunGroup(context.Background(), command)
	elapsed := time.Since(started)
	pid := recordedPID(t, filepath.Join(command.Dir, "pid"))
	killAtCleanup(t, pid)
	if err != nil || outcome.ExitCode != 0 || !outcome.Leftovers || outcome.OutputHeld || outcome.Stopped {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	if elapsed >= groupDrainGrace {
		t.Fatalf("a leftover that does not hold the pipe delayed the result by %s", elapsed)
	}
	if !running(pid) {
		t.Fatal("the kept leftover was stopped")
	}
	if !strings.Contains(output.String(), "leader-done") {
		t.Fatalf("output = %q", output.String())
	}
}

// A kept leftover that holds the output pipe is waited on for the drain
// grace, then reported, and stays running.
func TestRunGroupReportsOutputAKeptLeftoverStillHolds(t *testing.T) {
	t.Parallel()
	var output syncBuffer
	command := shellCommand(t, `sleep 60 & echo $! > pid; echo leader-done`, &output)
	command.KeepLeftovers = true
	started := time.Now()
	outcome, err := RunGroup(context.Background(), command)
	elapsed := time.Since(started)
	pid := recordedPID(t, filepath.Join(command.Dir, "pid"))
	killAtCleanup(t, pid)
	if err != nil || outcome.ExitCode != 0 || !outcome.Leftovers || !outcome.OutputHeld || outcome.PipeErr != nil {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	if !errors.Is(outcome.OutputErr(), ErrOutputHeld) {
		t.Fatalf("OutputErr = %v, want ErrOutputHeld", outcome.OutputErr())
	}
	if elapsed < groupDrainGrace {
		t.Fatalf("the result did not wait the drain grace: %s", elapsed)
	}
	if !running(pid) {
		t.Fatal("the kept leftover was stopped")
	}
}

// What a kept leftover writes after the command exits is still the
// command's output while the pipe drains.
func TestRunGroupCapturesAKeptLeftoversLateOutput(t *testing.T) {
	t.Parallel()
	var output syncBuffer
	command := shellCommand(t, `(sleep 0.2; echo late-output) & echo leader-done`, &output)
	command.KeepLeftovers = true
	outcome, err := RunGroup(context.Background(), command)
	if err != nil || outcome.ExitCode != 0 || outcome.OutputHeld {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	if text := output.String(); !strings.Contains(text, "leader-done") || !strings.Contains(text, "late-output") {
		t.Fatalf("output = %q", text)
	}
}

// Cancellation stops the whole group whether or not leftovers are kept.
func TestRunGroupCancellationStopsAKeptGroup(t *testing.T) {
	t.Parallel()
	var output syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := shellCommand(t, `sleep 60 >/dev/null 2>&1 & echo $! > pid; echo started; wait`, &output)
	command.KeepLeftovers = true
	group, err := StartGroup(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, &output, "started")
	pid := recordedPID(t, filepath.Join(command.Dir, "pid"))
	killAtCleanup(t, pid)
	cancel()
	outcome, err := group.Wait()
	if err == nil || !outcome.Stopped || outcome.ExitCode != -1 {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	waitForExit(t, pid)
}

func TestGroupOutcomeOutputErr(t *testing.T) {
	t.Parallel()
	refused := errors.New("sink is full")
	if err := (GroupOutcome{}).OutputErr(); err != nil {
		t.Fatalf("clean outcome: %v", err)
	}
	if err := (GroupOutcome{PipeErr: refused}).OutputErr(); !errors.Is(err, refused) || errors.Is(err, ErrOutputHeld) {
		t.Fatalf("pipe failure: %v", err)
	}
	held := GroupOutcome{OutputHeld: true}.OutputErr()
	if !errors.Is(held, ErrOutputHeld) || held.Error() != ErrOutputHeld.Error() {
		t.Fatalf("held output: %v", held)
	}
	if err := (GroupOutcome{OutputHeld: true, PipeErr: refused}).OutputErr(); !errors.Is(err, refused) || !errors.Is(err, ErrOutputHeld) {
		t.Fatalf("held output with a pipe failure: %v", err)
	}
}

func TestRunGroupTerminatesBeforeKilling(t *testing.T) {
	t.Parallel()
	var output syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan GroupOutcome, 1)
	go func() {
		outcome, _ := RunGroup(ctx, shellCommand(t,
			`trap 'echo graceful-term; exit 0' TERM; echo graceful-started; while :; do sleep 0.05; done`, &output))
		done <- outcome
	}()
	waitForOutput(t, &output, "graceful-started")
	cancel()
	outcome := <-done
	if outcome.ExitCode != 0 || !outcome.Stopped || outcome.Leftovers || !strings.Contains(output.String(), "graceful-term") {
		t.Fatalf("SIGTERM did not reach the command first: outcome=%+v output=%q", outcome, output.String())
	}
}

func TestRunGroupKillsWhatIgnoresTerminate(t *testing.T) {
	t.Parallel()
	const grace = 300 * time.Millisecond
	var output syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan GroupOutcome, 1)
	go func() {
		command := shellCommand(t, `trap '' TERM; echo stubborn-started; sleep 60`, &output)
		command.TerminateGrace = grace
		outcome, _ := RunGroup(ctx, command)
		done <- outcome
	}()
	waitForOutput(t, &output, "stubborn-started")
	canceled := time.Now()
	cancel()
	select {
	case outcome := <-done:
		if outcome.ExitCode == 0 || time.Since(canceled) < grace {
			t.Fatalf("stubborn command was not killed after the grace: %+v after %s", outcome, time.Since(canceled))
		}
	case <-time.After(grace + 10*time.Second):
		t.Fatal("stubborn command outlived the kill")
	}
}

// A leader that runs as another user ignores this user's SIGKILL. RunGroup
// gives up on it after one more grace and says the command is still running.
func TestRunGroupAbandonsALeaderTheKillDoesNotEnd(t *testing.T) {
	t.Parallel()
	const grace = 200 * time.Millisecond
	var output syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := shellCommand(t, `trap '' TERM; echo immune-started; sleep 60`, &output)
	command.TerminateGrace = grace
	var leader *exec.Cmd
	command.kill = func(cmd *exec.Cmd) error {
		leader = cmd
		return nil
	}
	type result struct {
		outcome GroupOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := RunGroup(ctx, command)
		done <- result{outcome, err}
	}()
	waitForOutput(t, &output, "immune-started")
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, ErrGroupSurvived) || got.outcome.ExitCode != -1 || !got.outcome.Stopped {
			t.Fatalf("outcome %+v, error %v", got.outcome, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunGroup waited on a command it could not stop")
	}
	if err := KillConfiguredGroup(leader); err != nil {
		t.Fatalf("end the abandoned command: %v", err)
	}
}

type refusingWriter struct{ err error }

func (w refusingWriter) Write([]byte) (int, error) { return 0, w.err }

// A sink that refuses a write must not stall the command on a full pipe.
func TestRunGroupKeepsDrainingWhenTheSinkRefusesAWrite(t *testing.T) {
	t.Parallel()
	refused := errors.New("sink is full")
	command := GroupCommand{
		Dir:    t.TempDir(),
		Env:    []string{"PATH=" + os.Getenv("PATH")},
		Argv:   []string{"/bin/sh", "-c", `head -c 1000000 /dev/zero | tr '\0' x; exit 3`},
		Output: refusingWriter{refused},
	}
	type result struct {
		outcome GroupOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := RunGroup(context.Background(), command)
		done <- result{outcome, err}
	}()
	select {
	case got := <-done:
		if got.outcome.ExitCode != 3 || !errors.Is(got.outcome.PipeErr, refused) {
			t.Fatalf("outcome %+v, error %v", got.outcome, got.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the command stalled writing to a pipe nobody read")
	}
}
