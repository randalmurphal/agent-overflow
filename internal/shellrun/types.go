package shellrun

import (
	"context"
	"errors"
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

	// leftoversNotice ends the output of a command that left processes
	// running in its group. They are stopped with the command, and neither
	// the person nor the model could tell that from the output alone.
	leftoversNotice = "[Background processes this command left running were stopped.]\n"
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

	PhaseStarted = "started"
	PhaseOutput  = "output"
	// PhaseTerminal carries what the command wrote to its terminal.
	PhaseTerminal = "terminal"
	PhaseFinished = "finished"
)

// ErrBusy is returned when the thread already has a running command.
var ErrBusy = errors.New("a shell command is already running in this thread")

// ErrStartedBeforeRestart is returned by Start for a run id an earlier process
// started. That process is gone and nothing knows how far the command got, so
// it is not run again under the same id.
var ErrStartedBeforeRestart = errors.New("this command was started before the app restarted and may already have run, so it was not run again. Run it once more to start it again")

// ErrNotRunning is returned by Stop when the run is not in flight.
var ErrNotRunning = errors.New("shell command is not running")

// Request starts one command. RunID is the caller's idempotency identity: a
// repeated Start with the id of the running command returns that run, with
// the id of a command that settled returns how it settled, and with the id of
// a command an earlier process started returns ErrStartedBeforeRestart.
type Request struct {
	ThreadID string
	RunID    string
	Command  string
	Dir      string
	Shell    string
	Env      []string
	// Timeout bounds the run. Zero means DefaultTimeout.
	Timeout time.Duration
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
	// Terminal is the end of what the command wrote to its terminal, with the
	// sequence of the last terminal frame. It is not output.
	Terminal    string
	TerminalSeq uint64
}

// Event is one frame of a run's live projection.
type Event struct {
	Phase    string
	ThreadID string
	RunID    string
	Command  string
	Seq      uint64
	Chunk    string
	// Terminal and TerminalSeq are set on a terminal frame: the whole
	// retained text, newest sequence wins.
	Terminal    string
	TerminalSeq uint64
	State       string
	Error       string
	ExitCode    int
	StartedAt   int64
	FinishedAt  int64
}

// Runner executes one supervised command. procutil.RunGroup is the default.
type Runner func(context.Context, procutil.GroupCommand) (procutil.GroupOutcome, error)

type Config struct {
	// Dir holds capture files for the life of a run and the markers of the
	// commands that were started. The service owns it and removes every
	// capture at construction.
	Dir string
	// Emit receives every frame in the order the run produced it.
	Emit func(Event)
	// Finish receives a settled command once, off every service lock. An error
	// settles the run as failed with that message.
	Finish func(context.Context, Result) error
	// Context is the host lifetime; its cancellation stops every run.
	Context func() context.Context
	// ShutdownError is returned by Start once Stop has begun.
	ShutdownError error
	Run           Runner
	// OpenTerminal allocates the terminal each command gets for prompts.
	// procutil.OpenTerminal is the default.
	OpenTerminal func() (*procutil.Terminal, error)
	Now          func() time.Time
}
