// Package shellrun runs the shell commands a person starts from a thread's
// composer. It owns each run's process group, its bounded output capture and
// its live projection. What a finished command becomes (an attachment and a
// user message) belongs to the host through Config.Finish.
package shellrun

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/procutil"
)

const (
	// CaptureLimitBytes bounds the head of a command's output kept on disk.
	// Output past it is dropped except for the retained tail, which is
	// appended after an omission marker when the run settles.
	CaptureLimitBytes = 8 << 20
	// tailBytes is the end of the output kept in memory: the snapshot a late
	// client reads, and the tail appended to a capped capture.
	tailBytes = 64 << 10
	// DefaultTimeout bounds a command the person never stops.
	DefaultTimeout = time.Hour

	flushInterval = 100 * time.Millisecond
	flushBytes    = 8 << 10
)

const (
	StateRunning = "running"
	// StateSent means the command settled and the host accepted its result.
	StateSent = "sent"
	// StateStopped means the person or a teardown stopped it; nothing is sent.
	StateStopped = "stopped"
	// StateFailed means the command could not start or the host refused the
	// result; Error says which.
	StateFailed = "failed"

	PhaseStarted  = "started"
	PhaseOutput   = "output"
	PhaseFinished = "finished"
)

// ErrBusy is returned when the thread already has a running command.
var ErrBusy = errors.New("a shell command is already running in this thread")

// ErrNotRunning is returned by Stop when the run is not in flight.
var ErrNotRunning = errors.New("shell command is not running")

// Request starts one command. RunID is the caller's idempotency identity: a
// repeated Start with the id of the running command returns that run.
type Request struct {
	ThreadID string
	RunID    string
	Command  string
	Dir      string
	Shell    string
	Env      []string
	// Timeout bounds the run. Zero means DefaultTimeout.
	Timeout time.Duration
	// Payload is opaque to the service and handed back on the Result.
	Payload any
}

// Result is a settled command the person did not stop.
type Result struct {
	ThreadID string
	RunID    string
	Command  string
	// ExitCode is the command's own, or -1 when it did not exit on its own
	// terms (TimedOut).
	ExitCode int
	Duration time.Duration
	// OutputPath is the capture file. Finish may move it; the service removes
	// whatever is still there once Finish returns.
	OutputPath string
	// Truncated reports output past CaptureLimitBytes: the capture holds the
	// head, an omission marker and the retained tail.
	Truncated bool
	TimedOut  bool
	// Leftovers reports background processes the command left in its group;
	// they were stopped.
	Leftovers bool
	// Payload is the request's.
	Payload any
}

// RunState is the snapshot of a running command.
type RunState struct {
	ThreadID  string
	RunID     string
	Command   string
	State     string
	Output    string
	OutputSeq uint64
	// OutputTruncated reports that Output is the end of a longer stream.
	OutputTruncated bool
	StartedAt       int64
}

// Event is one frame of a run's live projection.
type Event struct {
	Phase      string
	ThreadID   string
	RunID      string
	Command    string
	Seq        uint64
	Chunk      string
	State      string
	Error      string
	ExitCode   int
	StartedAt  int64
	FinishedAt int64
}

// Runner executes one supervised command. procutil.RunGroup is the default.
type Runner func(context.Context, procutil.GroupCommand) (procutil.GroupOutcome, error)

type Config struct {
	// Dir holds capture files for the life of a run. The service owns it and
	// empties it at construction.
	Dir string
	// Emit receives every frame in the order the run produced it.
	Emit func(Event)
	// Finish receives a settled command once, off every service lock. An error
	// settles the run as failed with that message.
	Finish func(context.Context, Result) error
	// Context is the host lifetime; its cancellation stops every run.
	Context func() context.Context
	// BeginWork takes the host's restart lease for the life of a run.
	BeginWork func(context.Context) (func(), error)
	// ShutdownError is returned by Start once Stop has begun.
	ShutdownError error
	Run           Runner
	Now           func() time.Time
}

// Service owns every run, the registry lock, the stopped gate and the
// WaitGroup that joins execution before the host's stores close.
type Service struct {
	config Config

	mu      sync.Mutex
	runs    map[string]*run // by thread id
	stopped bool
	wg      sync.WaitGroup
}

type run struct {
	request   Request
	startedAt time.Time
	cancel    context.CancelFunc
	tail      *procutil.TailBuffer
	seq       atomic.Uint64
	// exited closes once the process group is gone, before any Finish work.
	exited chan struct{}
	// stopReason is set under Service.mu by whoever cancels the run.
	stopReason string
}

const (
	stopByPerson   = "person"
	stopByTeardown = "teardown"
)

// New prepares the capture directory and returns a service with no runs.
func New(config Config) (*Service, error) {
	if strings.TrimSpace(config.Dir) == "" {
		return nil, errors.New("shellrun: capture directory is required")
	}
	if config.Finish == nil {
		return nil, errors.New("shellrun: finish hook is required")
	}
	if config.Emit == nil {
		config.Emit = func(Event) {}
	}
	if config.Context == nil {
		config.Context = context.Background
	}
	if config.BeginWork == nil {
		config.BeginWork = func(ctx context.Context) (func(), error) { return func() {}, ctx.Err() }
	}
	if config.ShutdownError == nil {
		config.ShutdownError = errors.New("shell command service stopped")
	}
	if config.Run == nil {
		config.Run = procutil.RunGroup
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	// A capture outlives its run only when the process died mid-run, and a
	// run exists only inside a live process.
	if err := os.RemoveAll(config.Dir); err != nil {
		return nil, fmt.Errorf("shellrun: clear capture directory: %w", err)
	}
	if err := os.MkdirAll(config.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("shellrun: create capture directory: %w", err)
	}
	return &Service{config: config, runs: make(map[string]*run)}, nil
}

// Start launches the command and returns once it is registered. The process
// starts on the run's own goroutine; a start failure arrives as a failed
// terminal frame.
func (s *Service) Start(request Request) (RunState, error) {
	request.Command = strings.TrimSpace(request.Command)
	if request.ThreadID == "" || request.RunID == "" {
		return RunState{}, errors.New("shell command: thread and run ids are required")
	}
	if request.Command == "" {
		return RunState{}, errors.New("shell command: command is empty")
	}
	if strings.ContainsRune(request.Command, 0) {
		return RunState{}, errors.New("shell command: command contains a NUL byte")
	}
	if request.Shell == "" || request.Dir == "" {
		return RunState{}, errors.New("shell command: shell and working directory are required")
	}
	if request.RunID != filepath.Base(request.RunID) || strings.HasPrefix(request.RunID, ".") {
		return RunState{}, errors.New("shell command: run id is not a plain name")
	}
	if request.Timeout <= 0 {
		request.Timeout = DefaultTimeout
	}
	endWork, err := s.config.BeginWork(s.config.Context())
	if err != nil {
		return RunState{}, err
	}

	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		endWork()
		return RunState{}, s.config.ShutdownError
	}
	if existing := s.runs[request.ThreadID]; existing != nil {
		state := existing.snapshot()
		s.mu.Unlock()
		endWork()
		if existing.request.RunID == request.RunID {
			return state, nil
		}
		return RunState{}, ErrBusy
	}
	ctx, cancel := context.WithTimeout(s.config.Context(), request.Timeout)
	current := &run{
		request: request, startedAt: s.config.Now(), cancel: cancel,
		tail: procutil.NewTailBuffer(tailBytes), exited: make(chan struct{}),
	}
	s.runs[request.ThreadID] = current
	s.wg.Add(1)
	state := current.snapshot()
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		defer endWork()
		defer cancel()
		s.execute(ctx, current)
	}()
	return state, nil
}

// Active returns the thread's running command, if any.
func (s *Service) Active(threadID string) []RunState {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.runs[threadID]
	if current == nil {
		return nil
	}
	return []RunState{current.snapshot()}
}

// ActiveRunID is the id of the thread's running command, or empty. It is the
// cheap form of Active for a caller that only asks whether to read it.
func (s *Service) ActiveRunID(threadID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current := s.runs[threadID]; current != nil {
		return current.request.RunID
	}
	return ""
}

// StopRun stops the named run at the person's request. Its output is
// discarded and nothing is sent.
func (s *Service) StopRun(threadID, runID string) error {
	s.mu.Lock()
	current := s.runs[threadID]
	if current == nil || current.request.RunID != runID {
		s.mu.Unlock()
		return ErrNotRunning
	}
	if current.stopReason == "" {
		current.stopReason = stopByPerson
	}
	s.mu.Unlock()
	current.cancel()
	return nil
}

// CancelThread stops whatever the thread is running, for a teardown that
// removes the thread or its workspace, and returns once the process group is
// gone. It never waits on Finish, so a caller may hold locks a send needs.
func (s *Service) CancelThread(threadID string) {
	s.mu.Lock()
	current := s.runs[threadID]
	if current != nil && current.stopReason == "" {
		current.stopReason = stopByTeardown
	}
	s.mu.Unlock()
	if current != nil {
		current.cancel()
		<-current.exited
	}
}

// Stop cancels every run and joins them. Called at shutdown before the stores
// a Finish hook writes to are closed. Idempotent.
func (s *Service) Stop() {
	s.mu.Lock()
	s.stopped = true
	cancels := make([]context.CancelFunc, 0, len(s.runs))
	for _, current := range s.runs {
		if current.stopReason == "" {
			current.stopReason = stopByTeardown
		}
		cancels = append(cancels, current.cancel)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	s.wg.Wait()
}

// snapshot reads the sequence before the tail, so a chunk racing the read is
// one the client already holds rather than one it never sees.
func (r *run) snapshot() RunState {
	seq := r.seq.Load()
	return RunState{
		ThreadID: r.request.ThreadID, RunID: r.request.RunID, Command: r.request.Command,
		State: StateRunning, Output: r.tail.String(), OutputSeq: seq,
		OutputTruncated: r.tail.Truncated(), StartedAt: r.startedAt.UnixMilli(),
	}
}

func (s *Service) execute(ctx context.Context, current *run) {
	request := current.request
	s.config.Emit(Event{
		Phase: PhaseStarted, ThreadID: request.ThreadID, RunID: request.RunID,
		Command: request.Command, State: StateRunning, StartedAt: current.startedAt.UnixMilli(),
	})

	capturePath := filepath.Join(s.config.Dir, request.RunID+".log")
	defer func() {
		if err := os.Remove(capturePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			// The directory is emptied at the next start, so the leak is bounded.
			log.Printf("shellrun: remove capture %s: %v", capturePath, err)
		}
	}()
	settle := func(state, message string, exitCode int) {
		s.mu.Lock()
		delete(s.runs, request.ThreadID)
		s.mu.Unlock()
		s.config.Emit(Event{
			Phase: PhaseFinished, ThreadID: request.ThreadID, RunID: request.RunID,
			Command: request.Command, State: state, Error: message, ExitCode: exitCode,
			StartedAt: current.startedAt.UnixMilli(), FinishedAt: s.config.Now().UnixMilli(),
		})
	}

	capture, err := newCapture(capturePath, current, s.config.Emit)
	if err != nil {
		close(current.exited)
		settle(StateFailed, "Could not capture the command's output: "+err.Error(), -1)
		return
	}
	outcome, runErr := s.config.Run(ctx, procutil.GroupCommand{
		Dir: request.Dir, Env: request.Env,
		Argv: []string{request.Shell, "-c", request.Command}, Output: capture,
	})
	close(current.exited)
	closeErr := errors.Join(capture.close(), outcome.PipeErr)
	duration := s.config.Now().Sub(current.startedAt)

	s.mu.Lock()
	stopReason := current.stopReason
	s.mu.Unlock()
	timedOut := stopReason == "" && errors.Is(ctx.Err(), context.DeadlineExceeded)
	// A command that died to a signal reports -1 through an ExitError; any
	// other error with -1 means the process never ran.
	var exitErr *exec.ExitError
	started := runErr == nil || errors.As(runErr, &exitErr)
	switch {
	case stopReason != "", ctx.Err() != nil && !timedOut:
		settle(StateStopped, "", outcome.ExitCode)
		return
	case !started:
		settle(StateFailed, "The command could not be started: "+runErr.Error(), -1)
		return
	case closeErr != nil:
		settle(StateFailed, "Could not capture the command's output: "+closeErr.Error(), outcome.ExitCode)
		return
	}

	// The result is the person's to see whether or not the host is going
	// away, so Finish runs on a context that outlives the run's own.
	finishErr := s.config.Finish(context.WithoutCancel(ctx), Result{
		ThreadID: request.ThreadID, RunID: request.RunID, Command: request.Command,
		ExitCode: outcome.ExitCode, Duration: duration, OutputPath: capturePath,
		Truncated: capture.truncated(), TimedOut: timedOut, Leftovers: outcome.Leftovers,
		Payload: request.Payload,
	})
	if finishErr != nil {
		settle(StateFailed, finishErr.Error(), outcome.ExitCode)
		return
	}
	settle(StateSent, "", outcome.ExitCode)
}
