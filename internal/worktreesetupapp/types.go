package worktreesetupapp

import "time"

const (
	outputTailBytes = 128 * 1024
	flushInterval   = 100 * time.Millisecond
	flushBytes      = 8 * 1024
)

const (
	runIdle      = "idle"
	runRunning   = "running"
	runFailed    = "failed"
	runSucceeded = "succeeded"
	runCancelled = "cancelled"

	stepPending   = "pending"
	stepRunning   = "running"
	stepSucceeded = "succeeded"
	stepFailed    = "failed"

	phaseStarted      = "started"
	phaseStepStarted  = "step-started"
	phaseOutput       = "output"
	phaseStepFinished = "step-finished"
	phaseFinished     = "finished"
)

type Step struct {
	Index int
	Kind  string
	Label string
	Argv  []string
}

type RunState struct {
	ThreadID     string
	RunID        string
	State        string
	Steps        []Step
	StepStatuses []string
	Output       string
	OutputSeq    uint64
	Error        string
	WorktreePath string
	StartedAt    int64
	FinishedAt   int64
}

type Event struct {
	Phase        string
	ThreadID     string
	RunID        string
	WorktreePath string
	Steps        []Step
	StepIndex    int
	Seq          uint64
	Chunk        string
	State        string
	Error        string
	StartedAt    int64
	FinishedAt   int64
}
