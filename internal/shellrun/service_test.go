//go:build !windows

package shellrun

import (
	"bytes"
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
	if final := rec.terminal(t); final.State != StateFailed || final.Error != "send refused" {
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

func TestCompletePrefix(t *testing.T) {
	t.Parallel()
	euro := []byte("€") // three bytes
	for name, tc := range map[string]struct {
		in   []byte
		want int
	}{
		"empty":            {nil, 0},
		"ascii":            {[]byte("abc"), 3},
		"whole rune":       {append([]byte("a"), euro...), 4},
		"one byte of rune": {append([]byte("a"), euro[:1]...), 1},
		"two bytes":        {append([]byte("a"), euro[:2]...), 1},
		"only partial":     {euro[:2], 0},
		"invalid tail":     {[]byte{'a', 0xff}, 2},
	} {
		if got := completePrefix(tc.in); got != tc.want {
			t.Errorf("%s: completePrefix(%q) = %d, want %d", name, tc.in, got, tc.want)
		}
	}
	if !bytes.Equal(euro, []byte("€")) {
		t.Fatal("input mutated")
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
