//go:build unix

package workflowhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/procutil"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// toolProcessRun is one tool command started through startToolRun on the
// start watchdog fixture, whose timers the test fires by hand.
type toolProcessRun struct {
	fixture       *startWatchdogFixture
	key           engine.RunKey
	workspace     string
	narrativePath string
}

func startToolProcess(t *testing.T, fixture *startWatchdogFixture, argv []string, watchdog time.Duration) (*toolProcessRun, error) {
	t.Helper()
	run := &toolProcessRun{
		fixture:       fixture,
		key:           engine.RunKey{ItemID: fixture.itemID, PhaseID: fixture.phaseID, Attempt: 1},
		workspace:     t.TempDir(),
		narrativePath: filepath.Join(t.TempDir(), "attempt.md"),
	}
	phase := def.Phase{ID: fixture.phaseID, Driver: def.DriverTool, Shape: def.ShapeSingle}
	err := fixture.runner.startToolRun(context.Background(), workflowToolRun{
		workflowCompletion: workflowCompletion{
			key: run.key, narrativePath: run.narrativePath,
			workspace: run.workspace, projectPath: run.workspace,
		},
		label: fmt.Sprintf("phase %q", fixture.phaseID), contract: def.PhaseEnvelope(phase),
		binding: `command "probe"`, argv: argv,
		envelopePath: filepath.Join(t.TempDir(), "envelope.json"),
		watchdog:     watchdog,
	}, func(outcome engine.Outcome) { fixture.outcomes <- outcome })
	return run, err
}

func (run *toolProcessRun) awaitOutcome(t *testing.T) engine.Outcome {
	t.Helper()
	select {
	case outcome := <-run.fixture.outcomes:
		return outcome
	case <-time.After(20 * time.Second):
		t.Fatal("the tool run reported no outcome")
		return engine.Outcome{}
	}
}

// awaitNarrative returns the narrative once the reaper wrote it. The start
// failure path writes it before startToolRun returns.
func (run *toolProcessRun) awaitNarrative(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(run.narrativePath); err == nil && len(data) > 0 {
			return string(data)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no narrative was written at %s", run.narrativePath)
	return ""
}

func (run *toolProcessRun) awaitFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(run.workspace, name)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && strings.HasSuffix(string(data), "\n") {
			return strings.TrimSpace(string(data))
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the command never wrote %s", path)
	return ""
}

func (run *toolProcessRun) recordedPID(t *testing.T) int {
	t.Helper()
	pid, err := strconv.Atoi(run.awaitFile(t, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("kill process %d: %v", pid, err)
		}
	})
	return pid
}

func (run *toolProcessRun) registered() bool {
	run.fixture.runner.mu.Lock()
	defer run.fixture.runner.mu.Unlock()
	return run.fixture.runner.tools[workflowRunKey(run.key)] != nil
}

// toolProcessRunning reports a process that has not exited. An exited process
// nobody has reaped yet still answers signal 0.
func toolProcessRunning(pid int) bool {
	return syscall.Kill(pid, 0) == nil && !procutil.Exited(pid)
}

func awaitToolProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for toolProcessRunning(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d is still running", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestToolRunReportsAStartFailureFromStart(t *testing.T) {
	fixture := newStartWatchdogFixture(t)
	run, err := startToolProcess(t, fixture, []string{"/nonexistent/ao-tool-binary"}, time.Hour)
	want := `workflow runner: start tool phase "work" command ["/nonexistent/ao-tool-binary"]: ` +
		`fork/exec /nonexistent/ao-tool-binary: no such file or directory`
	if !errors.Is(err, engine.ErrSetupFailed) || !strings.Contains(err.Error(), want) {
		t.Fatalf("start error = %v, want a setup failure containing %q", err, want)
	}
	narrative := run.awaitNarrative(t)
	if !strings.Contains(narrative, "- Outcome: the command could not be started: fork/exec /nonexistent/ao-tool-binary: no such file or directory\n") {
		t.Fatalf("narrative did not record the start failure:\n%s", narrative)
	}
	if run.registered() {
		t.Fatal("a failed start left its attempt registered")
	}
	fixture.refuteOutcome("a failed start also reported an outcome")
}

// A process a command left running on purpose, with its output redirected
// away, survives the command, and the command's own exit is the result.
func TestToolRunKeepsAProcessTheCommandLeftRunning(t *testing.T) {
	fixture := newStartWatchdogFixture(t)
	run, err := startToolProcess(t, fixture,
		[]string{"/bin/sh", "-c", "sleep 60 >/dev/null 2>&1 & echo $! > pid; echo leader-done"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if outcome := run.awaitOutcome(t); outcome.Kind != engine.OutcomeDone {
		t.Fatalf("outcome = %+v, want done", outcome)
	}
	pid := run.recordedPID(t)
	if !toolProcessRunning(pid) {
		t.Fatal("the process the command left running was stopped")
	}
	narrative := run.awaitNarrative(t)
	if !strings.Contains(narrative, "- Outcome: the command exited\n- Exit code: 0\n") || !strings.Contains(narrative, "leader-done") {
		t.Fatalf("narrative:\n%s", narrative)
	}
	if run.registered() {
		t.Fatal("a finished attempt stayed registered")
	}
}

// A process left running that still holds the command's output means the
// output never completed: the attempt fails, and the process is left running.
func TestToolRunFailsACommandWhoseLeftoverHoldsItsOutput(t *testing.T) {
	fixture := newStartWatchdogFixture(t)
	run, err := startToolProcess(t, fixture,
		[]string{"/bin/sh", "-c", "sleep 60 & echo $! > pid; echo leader-done"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if outcome := run.awaitOutcome(t); outcome.Kind != engine.OutcomeExecutionFailure {
		t.Fatalf("outcome = %+v, want an execution failure", outcome)
	}
	pid := run.recordedPID(t)
	if !toolProcessRunning(pid) {
		t.Fatal("the process the command left running was stopped")
	}
	narrative := run.awaitNarrative(t)
	if !strings.Contains(narrative, "- Outcome: the process did not complete: procutil: a process the command left running still holds its output\n") {
		t.Fatalf("narrative:\n%s", narrative)
	}
}

// stoppableToolScript backgrounds a child that left the output pipe, records
// it, and answers SIGTERM with a clean exit: the shape whose exit status must
// never be read as the command's result.
const stoppableToolScript = `trap 'exit 0' TERM
sleep 60 >/dev/null 2>&1 &
echo $! > pid
while :; do sleep 0.05; done`

// Teardown stops the whole group, writes the narrative and reports nothing,
// and the same attempt can be started and stopped again.
func TestToolRunTeardownStopsTheGroupAndReportsNothing(t *testing.T) {
	fixture := newStartWatchdogFixture(t)
	for round := range 3 {
		run, err := startToolProcess(t, fixture, []string{"/bin/sh", "-c", stoppableToolScript}, time.Hour)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		pid := run.recordedPID(t)
		if _, err := fixture.runner.Stop(context.Background(), run.key); err != nil {
			t.Fatalf("round %d: stop: %v", round, err)
		}
		if run.registered() {
			t.Fatalf("round %d: the stopped attempt stayed registered", round)
		}
		narrative := run.awaitNarrative(t)
		if !strings.Contains(narrative, "- Outcome: the process was killed by workflow teardown\n") {
			t.Fatalf("round %d: narrative:\n%s", round, narrative)
		}
		if strings.Contains(narrative, "Exit code") {
			t.Fatalf("round %d: a stopped command's exit status was recorded as its result:\n%s", round, narrative)
		}
		awaitToolProcessExit(t, pid)
		fixture.refuteOutcome("teardown reported an outcome")
	}
}

// The watchdog stops the group and parks the attempt as stalled.
func TestToolRunWatchdogStopsTheGroupAndReportsAStall(t *testing.T) {
	fixture := newStartWatchdogFixture(t)
	const watchdog = 50 * time.Millisecond
	run, err := startToolProcess(t, fixture, []string{"/bin/sh", "-c", stoppableToolScript}, watchdog)
	if err != nil {
		t.Fatal(err)
	}
	pid := run.recordedPID(t)
	time.Sleep(watchdog)
	fixture.awaitTimer(watchdog).fire()
	if outcome := run.awaitOutcome(t); outcome.Kind != engine.OutcomeStalled {
		t.Fatalf("outcome = %+v, want stalled", outcome)
	}
	narrative := run.awaitNarrative(t)
	if !strings.Contains(narrative, "- Outcome: the process was killed after 50ms without output\n") || strings.Contains(narrative, "Exit code") {
		t.Fatalf("narrative:\n%s", narrative)
	}
	awaitToolProcessExit(t, pid)
	fixture.refuteOutcome("the stalled attempt reported twice")
}

// The application's life context ending stops the command without a stop
// reason. Its clean exit in answer to the signal is not a passed check.
func TestToolRunStoppedByTheLifeContextIsNotAResult(t *testing.T) {
	fixture := newStartWatchdogFixture(t)
	life, endLife := context.WithCancel(context.Background())
	defer endLife()
	fixture.host.lifeCtx = life
	run, err := startToolProcess(t, fixture, []string{"/bin/sh", "-c", stoppableToolScript}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pid := run.recordedPID(t)
	endLife()
	if outcome := run.awaitOutcome(t); outcome.Kind != engine.OutcomeExecutionFailure {
		t.Fatalf("outcome = %+v, want an execution failure", outcome)
	}
	narrative := run.awaitNarrative(t)
	if !strings.Contains(narrative, "- Outcome: the process did not complete: it was stopped before it finished (exit code 0)\n") ||
		strings.Contains(narrative, "Exit code:") {
		t.Fatalf("narrative:\n%s", narrative)
	}
	awaitToolProcessExit(t, pid)
}

func TestWorkflowToolStopOutcomeSaysWhenTheProcessSurvived(t *testing.T) {
	if got := workflowToolStopOutcome("by workflow teardown", errors.New("signal: terminated")); got != "the process was killed by workflow teardown" {
		t.Fatalf("stopped outcome = %q", got)
	}
	survived := fmt.Errorf("wait: %w", procutil.ErrGroupSurvived)
	want := "the process was signaled by workflow teardown but could not be stopped and is still running"
	if got := workflowToolStopOutcome("by workflow teardown", survived); got != want {
		t.Fatalf("survived outcome = %q, want %q", got, want)
	}
}

func TestStopToolsReturnsOnlyOnceEveryToolProcessIsReapedAndRefusesLaterStarts(t *testing.T) {
	fixture := newStartWatchdogFixture(t)
	// The command answers SIGTERM slowly, so a stop that did not wait would
	// return with it still running and its narrative unwritten.
	run, err := startToolProcess(t, fixture, []string{"/bin/sh", "-c",
		`trap 'sleep 0.4; echo gone > done; exit 0' TERM; echo $$ > pid; while :; do sleep 0.05; done`,
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pid := run.recordedPID(t)

	fixture.runner.StopTools()

	if toolProcessRunning(pid) {
		t.Fatalf("StopTools returned with process %d still running", pid)
	}
	if data, err := os.ReadFile(filepath.Join(run.workspace, "done")); err != nil || string(data) != "gone\n" {
		t.Fatalf("the command did not finish its SIGTERM handler: %q, %v", data, err)
	}
	narrative, err := os.ReadFile(run.narrativePath)
	if err != nil || !strings.Contains(string(narrative), "- Outcome: the process was killed by workflow teardown\n") {
		t.Fatalf("StopTools returned before the narrative was written: %q, %v", narrative, err)
	}
	if run.registered() {
		t.Fatal("the stopped attempt is still registered")
	}

	fixture.runner.StopTools()
	later, err := startToolProcess(t, fixture, []string{"/bin/sh", "-c", `echo ran > ran`}, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "the app is shutting down") {
		t.Fatalf("a tool start after StopTools: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, statErr := os.Stat(filepath.Join(later.workspace, "ran")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the refused command ran: %v", statErr)
	}
}
