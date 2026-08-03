package worktreesetup

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// recordingObserver captures the callback sequence as comparable strings so a
// test can assert ORDER, not just occurrence. Output chunks are copied on
// receipt — the contract says the slice is only valid for the call.
type recordingObserver struct {
	mu     sync.Mutex
	steps  []Step
	events []string
	output map[int]string
}

func newRecordingObserver() *recordingObserver {
	return &recordingObserver{output: map[int]string{}}
}

func (o *recordingObserver) RunStarted(steps []Step) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.steps = steps
	o.events = append(o.events, "run-started")
}

func (o *recordingObserver) StepStarted(index int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, "step-started:"+strconv.Itoa(index))
}

func (o *recordingObserver) Output(index int, chunk []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.output[index] += string(chunk)
}

func (o *recordingObserver) StepFinished(index int, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	outcome := "ok"
	if err != nil {
		outcome = "err"
	}
	o.events = append(o.events, "step-finished:"+strconv.Itoa(index)+":"+outcome)
}

func (o *recordingObserver) RunFinished(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	outcome := "ok"
	if err != nil {
		outcome = "err"
	}
	o.events = append(o.events, "run-finished:"+outcome)
}

func TestResolveStepsOmitsTheCopyStepWhenNothingIsCopied(t *testing.T) {
	steps := ResolveSteps(Config{Run: [][]string{{"true"}, {"echo", "hi"}}})
	if len(steps) != 2 {
		t.Fatalf("steps = %+v, want 2 command steps", steps)
	}
	for index, step := range steps {
		if step.Index != index {
			t.Fatalf("step %d carries index %d", index, step.Index)
		}
		if step.Kind != StepCommand {
			t.Fatalf("step %d kind = %q", index, step.Kind)
		}
	}
	if steps[1].Label != "echo hi" {
		t.Fatalf("command label = %q", steps[1].Label)
	}
}

func TestResolveStepsPutsCopyFirst(t *testing.T) {
	steps := ResolveSteps(Config{Copy: []string{".env"}, Run: [][]string{{"true"}}})
	if len(steps) != 2 || steps[0].Kind != StepCopy || steps[0].Index != 0 {
		t.Fatalf("steps = %+v", steps)
	}
	if steps[0].Label != CopyStepLabel || steps[0].Argv != nil {
		t.Fatalf("copy step = %+v", steps[0])
	}
	if steps[1].Kind != StepCommand || steps[1].Index != 1 {
		t.Fatalf("command step = %+v", steps[1])
	}
}

func TestRunObservedReportsEveryStepInOrder(t *testing.T) {
	project := t.TempDir()
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, ".env"), []byte("TOKEN=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	observer := newRecordingObserver()
	err := RunObserved(context.Background(), project, worktree, Config{
		Copy:    []string{".env"},
		Run:     [][]string{{"/bin/sh", "-c", "echo first"}, {"/bin/sh", "-c", "echo second >&2"}},
		Timeout: "30s",
	}, observer)
	if err != nil {
		t.Fatalf("RunObserved = %v", err)
	}
	if len(observer.steps) != 3 || observer.steps[0].Kind != StepCopy {
		t.Fatalf("run-started steps = %+v", observer.steps)
	}
	want := []string{
		"run-started",
		"step-started:0", "step-finished:0:ok",
		"step-started:1", "step-finished:1:ok",
		"step-started:2", "step-finished:2:ok",
		"run-finished:ok",
	}
	if strings.Join(observer.events, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %v, want %v", observer.events, want)
	}
	if got := strings.TrimSpace(observer.output[1]); got != "first" {
		t.Fatalf("step 1 output = %q", got)
	}
	if got := strings.TrimSpace(observer.output[2]); got != "second" {
		t.Fatalf("step 2 stderr = %q", got)
	}
}

// A failing copy has to look exactly like a failing command to a subscriber:
// the panel highlights the failed step by index either way.
func TestRunObservedReportsCopyFailureAsStepZero(t *testing.T) {
	observer := newRecordingObserver()
	err := RunObserved(context.Background(), t.TempDir(), t.TempDir(), Config{
		Copy: []string{"missing-everywhere"},
		Run:  [][]string{{"/bin/sh", "-c", "true"}},
	}, observer)
	if err == nil {
		t.Fatal("missing copy glob reported success")
	}
	want := []string{"run-started", "step-started:0", "step-finished:0:err", "run-finished:err"}
	if strings.Join(observer.events, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %v, want %v", observer.events, want)
	}
}

func TestRunObservedStopsAtTheFirstFailedCommand(t *testing.T) {
	observer := newRecordingObserver()
	err := RunObserved(context.Background(), t.TempDir(), t.TempDir(), Config{
		Run: [][]string{
			{"/bin/sh", "-c", "echo boom >&2; exit 2"},
			{"/bin/sh", "-c", "echo unreachable"},
		},
	}, observer)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("failure error = %v", err)
	}
	want := []string{"run-started", "step-started:0", "step-finished:0:err", "run-finished:err"}
	if strings.Join(observer.events, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %v, want %v", observer.events, want)
	}
}

// The failure message still quotes the tail with a subscriber attached: the
// observer is a SECOND writer, not a replacement for the buffer the message
// reads.
func TestRunObservedKeepsTheFailureTail(t *testing.T) {
	err := RunObserved(context.Background(), t.TempDir(), t.TempDir(), Config{
		Run: [][]string{{"/bin/sh", "-c", "echo diagnosis >&2; exit 3"}},
	}, newRecordingObserver())
	if err == nil || !strings.Contains(err.Error(), "diagnosis") {
		t.Fatalf("failure error = %v", err)
	}
}

// An unparseable timeout is refused even when the recipe runs no commands —
// the pre-observer ordering, pinned because the workflow caller depends on it.
func TestRunObservedRefusesAnUnparseableTimeoutWithoutCommands(t *testing.T) {
	observer := newRecordingObserver()
	err := RunObserved(context.Background(), t.TempDir(), t.TempDir(), Config{Timeout: "later"}, observer)
	if err == nil || !strings.Contains(err.Error(), "invalid worktree setup timeout") {
		t.Fatalf("bad timeout error = %v", err)
	}
	want := []string{"run-started", "run-finished:err"}
	if strings.Join(observer.events, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %v, want %v", observer.events, want)
	}
}

func TestRunObservedWithNilObserverIsRun(t *testing.T) {
	if err := RunObserved(context.Background(), t.TempDir(), t.TempDir(), Config{}, nil); err != nil {
		t.Fatalf("nil observer = %v", err)
	}
}
