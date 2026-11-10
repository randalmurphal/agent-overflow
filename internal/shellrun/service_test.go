//go:build !windows

package shellrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/procutil"
)

type recorder struct {
	mu      sync.Mutex
	events  []Event
	results []Result
	outputs []string
	finish  func(Result) error
}

func (r *recorder) emit(event Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recorder) finished(_ context.Context, result Result) error {
	data, err := os.ReadFile(result.OutputPath)
	r.mu.Lock()
	r.results = append(r.results, result)
	r.outputs = append(r.outputs, string(data))
	hook := r.finish
	r.mu.Unlock()
	if err != nil {
		return err
	}
	if hook != nil {
		return hook(result)
	}
	return nil
}

func (r *recorder) snapshot() ([]Event, []Result, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...), append([]Result(nil), r.results...), append([]string(nil), r.outputs...)
}

func (r *recorder) terminal(t *testing.T) Event {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		events, _, _ := r.snapshot()
		for _, event := range events {
			if event.Phase == PhaseFinished {
				return event
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the terminal frame")
	return Event{}
}

func newService(t *testing.T, rec *recorder, mutate func(*Config)) *Service {
	t.Helper()
	config := Config{Dir: filepath.Join(t.TempDir(), "captures"), Emit: rec.emit, Finish: rec.finished}
	if mutate != nil {
		mutate(&config)
	}
	service, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Stop)
	return service
}

func shellRequest(t *testing.T, runID, command string) Request {
	t.Helper()
	return Request{
		ThreadID: "thread-1", RunID: runID, Command: command, Dir: t.TempDir(),
		Shell: "/bin/sh", Env: []string{"PATH=" + os.Getenv("PATH")},
	}
}

func liveOutput(events []Event) string {
	var out strings.Builder
	for _, event := range events {
		if event.Phase == PhaseOutput {
			out.WriteString(event.Chunk)
		}
	}
	return out.String()
}

func TestRunDeliversOutputExitCodeAndOrderedFrames(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	service := newService(t, rec, nil)
	state, err := service.Start(shellRequest(t, "run-1", "echo one; echo two 1>&2; exit 3"))
	if err != nil || state.State != StateRunning || state.Command != "echo one; echo two 1>&2; exit 3" {
		t.Fatalf("start: %+v %v", state, err)
	}
	final := rec.terminal(t)
	if final.State != StateSent || final.ExitCode != 3 || final.Error != "" {
		t.Fatalf("terminal frame: %+v", final)
	}
	events, results, outputs := rec.snapshot()
	if len(results) != 1 || results[0].ExitCode != 3 || results[0].TimedOut || results[0].Truncated {
		t.Fatalf("results: %+v", results)
	}
	if !strings.Contains(outputs[0], "one\n") || !strings.Contains(outputs[0], "two\n") {
		t.Fatalf("capture: %q", outputs[0])
	}
	if events[0].Phase != PhaseStarted || events[len(events)-1].Phase != PhaseFinished {
		t.Fatalf("frame order: %+v", events)
	}
	if live := liveOutput(events); live != outputs[0] {
		t.Fatalf("live output %q differs from capture %q", live, outputs[0])
	}
	var seq uint64
	for _, event := range events {
		if event.Phase == PhaseOutput {
			seq++
			if event.Seq != seq {
				t.Fatalf("output sequence %d at position %d", event.Seq, seq)
			}
		}
	}
	if _, err := os.Stat(results[0].OutputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capture retained after finish: %v", err)
	}
	if active := service.Active("thread-1"); len(active) != 0 {
		t.Fatalf("settled run still active: %+v", active)
	}
}

func TestStopDiscardsTheRun(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	service := newService(t, rec, nil)
	if _, err := service.Start(shellRequest(t, "run-1", "echo started; sleep 60")); err != nil {
		t.Fatal(err)
	}
	waitActiveOutput(t, service, "started")
	if err := service.StopRun("thread-1", "other"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stop of another run id: %v", err)
	}
	if err := service.StopRun("thread-1", "run-1"); err != nil {
		t.Fatal(err)
	}
	if final := rec.terminal(t); final.State != StateStopped {
		t.Fatalf("terminal frame: %+v", final)
	}
	if _, results, _ := rec.snapshot(); len(results) != 0 {
		t.Fatalf("stopped run was delivered: %+v", results)
	}
	if err := service.StopRun("thread-1", "run-1"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("second stop: %v", err)
	}
}

func waitActiveOutput(t *testing.T, service *Service, marker string) RunState {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if active := service.Active("thread-1"); len(active) == 1 && strings.Contains(active[0].Output, marker) {
			return active[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q", marker)
	return RunState{}
}

func TestOneRunPerThreadAndRepeatedStartIsIdempotent(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	service := newService(t, rec, nil)
	first, err := service.Start(shellRequest(t, "run-1", "echo started; sleep 60"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := waitActiveOutput(t, service, "started")
	if snapshot.OutputSeq == 0 || snapshot.RunID != "run-1" {
		t.Fatalf("snapshot: %+v", snapshot)
	}
	again, err := service.Start(shellRequest(t, "run-1", "echo started; sleep 60"))
	if err != nil || again.RunID != first.RunID || again.StartedAt != first.StartedAt {
		t.Fatalf("repeated start: %+v %v", again, err)
	}
	if _, err := service.Start(shellRequest(t, "run-2", "echo other")); !errors.Is(err, ErrBusy) {
		t.Fatalf("second command: %v", err)
	}
	other := shellRequest(t, "run-3", "echo other-thread")
	other.ThreadID = "thread-2"
	if _, err := service.Start(other); err != nil {
		t.Fatalf("another thread was refused: %v", err)
	}
	service.CancelThread("thread-1")
	if final := rec.terminal(t); final.Phase != PhaseFinished {
		t.Fatalf("terminal frame: %+v", final)
	}
}

func TestFinishFailureAndStartFailureReachTheTerminalFrame(t *testing.T) {
	t.Parallel()
	rec := &recorder{finish: func(Result) error { return errors.New("send refused") }}
	service := newService(t, rec, nil)
	if _, err := service.Start(shellRequest(t, "run-1", "echo hi")); err != nil {
		t.Fatal(err)
	}
	if final := rec.terminal(t); final.State != StateFailed || !strings.HasSuffix(final.Error, "was not sent: send refused") {
		t.Fatalf("finish failure frame: %+v", final)
	}

	rec2 := &recorder{}
	service2 := newService(t, rec2, nil)
	request := shellRequest(t, "run-1", "echo hi")
	request.Shell = "/nonexistent/ao-shell"
	if _, err := service2.Start(request); err != nil {
		t.Fatal(err)
	}
	final := rec2.terminal(t)
	if final.State != StateFailed || !strings.Contains(final.Error, "could not be started") {
		t.Fatalf("start failure frame: %+v", final)
	}
	if _, results, _ := rec2.snapshot(); len(results) != 0 {
		t.Fatalf("a command that never ran was delivered: %+v", results)
	}
}

func TestTimeoutDeliversWhatTheCommandPrinted(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	service := newService(t, rec, nil)
	request := shellRequest(t, "run-1", "echo before; sleep 60")
	request.Timeout = 300 * time.Millisecond
	if _, err := service.Start(request); err != nil {
		t.Fatal(err)
	}
	if final := rec.terminal(t); final.State != StateSent {
		t.Fatalf("terminal frame: %+v", final)
	}
	_, results, outputs := rec.snapshot()
	if len(results) != 1 || !results[0].TimedOut || !strings.Contains(outputs[0], "before") {
		t.Fatalf("results=%+v outputs=%q", results, outputs)
	}
}

func TestStopJoinsRunsAndRefusesNewOnes(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	shutdown := errors.New("shutting down")
	service := newService(t, rec, func(config *Config) { config.ShutdownError = shutdown })
	if _, err := service.Start(shellRequest(t, "run-1", "echo started; sleep 60")); err != nil {
		t.Fatal(err)
	}
	waitActiveOutput(t, service, "started")
	service.Stop()
	events, results, _ := rec.snapshot()
	if last := events[len(events)-1]; last.Phase != PhaseFinished || last.State != StateStopped {
		t.Fatalf("stop returned before the run settled: %+v", last)
	}
	if len(results) != 0 {
		t.Fatalf("a run stopped by shutdown was delivered: %+v", results)
	}
	if _, err := service.Start(shellRequest(t, "run-2", "echo hi")); !errors.Is(err, shutdown) {
		t.Fatalf("start after stop: %v", err)
	}
}

func TestStartValidatesTheRequest(t *testing.T) {
	t.Parallel()
	service := newService(t, &recorder{}, nil)
	for name, mutate := range map[string]func(*Request){
		"empty command":  func(r *Request) { r.Command = "  \n" },
		"nul byte":       func(r *Request) { r.Command = "echo a\x00b" },
		"no thread":      func(r *Request) { r.ThreadID = "" },
		"no shell":       func(r *Request) { r.Shell = "" },
		"no directory":   func(r *Request) { r.Dir = "" },
		"path as run id": func(r *Request) { r.RunID = "../escape" },
		"dotted run id":  func(r *Request) { r.RunID = ".hidden" },
	} {
		request := shellRequest(t, "run-1", "echo hi")
		mutate(&request)
		if _, err := service.Start(request); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// A flood past the capture cap keeps the head, says how much it dropped and
// ends with the stream's own tail; live frames stay bounded and whole-rune.
func TestCaptureCapKeepsHeadMarkerAndTail(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	line := []byte(strings.Repeat("é", 511) + "\n")
	total := 0
	service := newService(t, rec, func(config *Config) {
		config.Run = func(_ context.Context, command procutil.GroupCommand) (procutil.GroupOutcome, error) {
			_, _ = command.Output.Write([]byte("HEAD-MARK\n"))
			for total < CaptureLimitBytes+(1<<20) {
				// Split each line mid-rune to prove frames re-join them.
				_, _ = command.Output.Write(line[:101])
				_, _ = command.Output.Write(line[101:])
				total += len(line)
			}
			_, _ = command.Output.Write([]byte("TAIL-MARK\n"))
			return procutil.GroupOutcome{ExitCode: 0}, nil
		}
	})
	if _, err := service.Start(shellRequest(t, "run-1", "flood")); err != nil {
		t.Fatal(err)
	}
	if final := rec.terminal(t); final.State != StateSent {
		t.Fatalf("terminal frame: %+v", final)
	}
	events, results, outputs := rec.snapshot()
	if !results[0].Truncated {
		t.Fatal("truncation was not reported")
	}
	capture := outputs[0]
	if !strings.HasPrefix(capture, "HEAD-MARK\n") || !strings.HasSuffix(capture, "TAIL-MARK\n") {
		t.Fatalf("capture lost its head or tail: %q … %q", capture[:20], capture[len(capture)-20:])
	}
	if !strings.Contains(capture, "bytes of output omitted ...]") {
		t.Fatal("capture has no omission marker")
	}
	if len(capture) > CaptureLimitBytes+tailBytes+128 {
		t.Fatalf("capture is %d bytes", len(capture))
	}
	for _, event := range events {
		if event.Phase == PhaseOutput && !utf8.ValidString(event.Chunk) {
			t.Fatalf("frame %d splits a rune", event.Seq)
		}
	}
}

func TestNewClearsStaleCaptures(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "captures")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "old.log")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := New(Config{Dir: dir, Finish: func(context.Context, Result) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop()
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale capture survived: %v", err)
	}
}

// A start that arrives again after its run settled is answered with how it
// settled, whether or not anything was delivered.
func TestARepeatedStartOfASettledRunIsAnsweredNotRun(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	service := newService(t, rec, nil)
	marker := filepath.Join(t.TempDir(), "ran")
	sent := shellRequest(t, "run-sent", "echo once >> "+marker)
	if _, err := service.Start(sent); err != nil {
		t.Fatal(err)
	}
	rec.terminal(t)
	if again, err := service.Start(sent); err != nil || again.State != StateSent {
		t.Fatalf("repeat of a sent run: %+v %v", again, err)
	}

	stopped := shellRequest(t, "run-stopped", "echo once >> "+marker+"; echo started; sleep 60")
	if _, err := service.Start(stopped); err != nil {
		t.Fatal(err)
	}
	waitActiveOutput(t, service, "started")
	if err := service.StopRun("thread-1", "run-stopped"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for service.HasActive() {
		if time.Now().After(deadline) {
			t.Fatal("the stopped run never settled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if again, err := service.Start(stopped); err != nil || again.State != StateStopped {
		t.Fatalf("repeat of a stopped run: %+v %v", again, err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "once\nonce\n" {
		t.Fatalf("each command ran once, marker holds %q %v", data, err)
	}
}

// Stop answers for what it did: once the outcome is decided and the result is
// on its way, there is nothing left to stop.
func TestStopAfterTheOutcomeIsDecidedReportsNotRunning(t *testing.T) {
	t.Parallel()
	inFinish, release := make(chan struct{}), make(chan struct{})
	rec := &recorder{finish: func(Result) error {
		close(inFinish)
		<-release
		return nil
	}}
	service := newService(t, rec, nil)
	if _, err := service.Start(shellRequest(t, "run-1", "echo done")); err != nil {
		t.Fatal(err)
	}
	<-inFinish
	if err := service.StopRun("thread-1", "run-1"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stop during delivery: %v", err)
	}
	close(release)
	if final := rec.terminal(t); final.State != StateSent {
		t.Fatalf("terminal frame: %+v", final)
	}
}

// Background processes are stopped with the command, and the output says so.
func TestLeftoverProcessesAreNamedInTheOutput(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	service := newService(t, rec, nil)
	if _, err := service.Start(shellRequest(t, "run-1", "sleep 60 & echo started")); err != nil {
		t.Fatal(err)
	}
	if final := rec.terminal(t); final.State != StateSent {
		t.Fatalf("terminal frame: %+v", final)
	}
	_, _, outputs := rec.snapshot()
	if len(outputs) != 1 || outputs[0] != "started\n"+leftoversNotice {
		t.Fatalf("output: %q", outputs)
	}
}

// Removing a directory stops the commands running inside it, whichever
// thread they belong to, and no others.
func TestCancelDirStopsOnlyCommandsInsideTheDirectory(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	service := newService(t, rec, nil)
	root := t.TempDir()
	inside, sibling := filepath.Join(root, "tree", "sub"), filepath.Join(root, "tree-other")
	for _, dir := range []string{inside, sibling} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	start := func(threadID, runID, dir string) {
		request := shellRequest(t, runID, "sleep 60")
		request.ThreadID, request.Dir = threadID, dir
		if _, err := service.Start(request); err != nil {
			t.Fatal(err)
		}
	}
	start("thread-inside", "run-inside", inside)
	start("thread-sibling", "run-sibling", sibling)
	service.CancelDir(filepath.Join(root, "tree"))
	// CancelDir returns when the process group is gone. The run leaves the
	// registry when it settles, which the terminal frame follows.
	if final := rec.terminal(t); final.RunID != "run-inside" || final.State != StateStopped {
		t.Fatalf("terminal frame: %+v", final)
	}
	if service.ActiveRunID("thread-inside") != "" {
		t.Fatal("the command inside the directory is still registered as running")
	}
	if service.ActiveRunID("thread-sibling") != "run-sibling" {
		t.Fatal("a command in a sibling directory was stopped")
	}
}

// A command the kill could not end is reported as still running, even to the
// person who pressed Stop.
func TestACommandThatSurvivesTheKillIsReported(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	service := newService(t, rec, func(config *Config) {
		config.Run = func(ctx context.Context, _ procutil.GroupCommand) (procutil.GroupOutcome, error) {
			<-ctx.Done()
			return procutil.GroupOutcome{ExitCode: -1}, procutil.ErrGroupSurvived
		}
	})
	if _, err := service.Start(shellRequest(t, "run-1", "sudo tail -f /dev/null")); err != nil {
		t.Fatal(err)
	}
	if err := service.StopRun("thread-1", "run-1"); err != nil {
		t.Fatal(err)
	}
	if final := rec.terminal(t); final.State != StateFailed || !strings.Contains(final.Error, "still running") {
		t.Fatalf("terminal frame: %+v", final)
	}
}

// Two runs with one id cannot share a capture: the second fails, and the
// first keeps its output.
func TestAFailedRunDoesNotRemoveAnotherRunsCapture(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	service := newService(t, rec, nil)
	first := shellRequest(t, "run-shared", "echo first; sleep 1; echo done")
	if _, err := service.Start(first); err != nil {
		t.Fatal(err)
	}
	waitActiveOutput(t, service, "first")
	second := shellRequest(t, "run-shared", "echo second")
	second.ThreadID = "thread-2"
	if _, err := service.Start(second); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for service.HasActive() {
		if time.Now().After(deadline) {
			t.Fatal("the runs never settled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, results, outputs := rec.snapshot()
	if len(results) != 1 || results[0].ThreadID != "thread-1" || outputs[0] != "first\ndone\n" {
		t.Fatalf("delivered: %+v %q", results, outputs)
	}
}
