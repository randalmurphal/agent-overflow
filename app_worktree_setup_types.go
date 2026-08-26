package main

import "time"

// The wire contract of the chat-thread worktree setup surface: the channel
// name, the vocabularies a frame's `phase` / `state` fields draw from, and the
// two payload shapes. Split out of app_worktree_setup.go because both halves
// of the feature — the run manager and the observer that emits for it — read
// exactly this, and "what does worktree:setup carry" is a question worth
// answering in one place.
//
// The Wails binding generator picks these up wherever they live in the main
// package; they stay here rather than in internal/worktreesetup because that
// package has no frontend and no opinion about JSON.

const (
	// worktreeSetupOutputTailBytes bounds the output one run retains in
	// memory for the snapshot RPC. Eight times the per-command tail the
	// failure message quotes: the panel shows a whole run, and a failing
	// install's useful end can be several screens.
	worktreeSetupOutputTailBytes = 128 * 1024

	// worktreeSetupFlushInterval bounds how long a partial line waits before
	// it is pushed. Output is coalesced to whole lines where possible — one
	// frame per byte would swamp the event ring on a chatty install.
	worktreeSetupFlushInterval = 100 * time.Millisecond

	// worktreeSetupFlushBytes forces a flush for output that never emits a
	// newline (progress bars redrawing with \r), so the panel still moves.
	worktreeSetupFlushBytes = 8 * 1024
)

// Run states, as the panel sees them. "succeeded" and "cancelled" are terminal
// event states only — neither is ever retained, so a snapshot never reports
// them (see finishThreadWorktreeSetup).
const (
	worktreeSetupRunIdle      = "idle"
	worktreeSetupRunRunning   = "running"
	worktreeSetupRunFailed    = "failed"
	worktreeSetupRunSucceeded = "succeeded"
	worktreeSetupRunCancelled = "cancelled"
)

// Per-step statuses.
const (
	worktreeSetupStepPending   = "pending"
	worktreeSetupStepRunning   = "running"
	worktreeSetupStepSucceeded = "succeeded"
	worktreeSetupStepFailed    = "failed"
)

// Event phases.
const (
	worktreeSetupPhaseStarted      = "started"
	worktreeSetupPhaseStepStarted  = "step-started"
	worktreeSetupPhaseOutput       = "output"
	worktreeSetupPhaseStepFinished = "step-finished"
	worktreeSetupPhaseFinished     = "finished"
)

// WorktreeSetupStep is one resolved step of a run, in execution order. Mirrors
// worktreesetup.Step; declared here because frontend-facing shapes belong
// beside the bound method, not in the pure package.
type WorktreeSetupStep struct {
	Index int      `json:"index"`
	Kind  string   `json:"kind"`
	Label string   `json:"label"`
	Argv  []string `json:"argv,omitempty"`
}

// WorktreeSetupRunState is the whole panel state for one thread — the
// reconnect companion to the event stream, in the same contract shape as
// GetThreadLiveState. A client that saw every event and a client that saw none
// land on the same value.
type WorktreeSetupRunState struct {
	ThreadID string `json:"threadId"`
	// RunID is empty when State is "idle", or when a durable failure outlived
	// the process that produced it (the run's output is gone; the failure is
	// not).
	RunID        string              `json:"runId"`
	State        string              `json:"state"`
	Steps        []WorktreeSetupStep `json:"steps"`
	StepStatuses []string            `json:"stepStatuses"`
	Output       string              `json:"output"`
	// OutputSeq is the highest chunk sequence folded into Output. A client
	// applies live chunks above it and ignores the rest, so a snapshot that
	// races the stream cannot double-append.
	OutputSeq    uint64 `json:"outputSeq"`
	Error        string `json:"error,omitempty"`
	WorktreePath string `json:"worktreePath,omitempty"`
	StartedAt    int64  `json:"startedAt,omitempty"`
	FinishedAt   int64  `json:"finishedAt,omitempty"`
}

// WorktreeSetupEvent is one frame on the worktree:setup channel. Phase names
// which fields are meaningful; StepIndex and Seq carry no omitempty because
// zero is a real step and a real sequence floor.
type WorktreeSetupEvent struct {
	Phase        string              `json:"phase"`
	ThreadID     string              `json:"threadId"`
	RunID        string              `json:"runId"`
	WorktreePath string              `json:"worktreePath,omitempty"`
	Steps        []WorktreeSetupStep `json:"steps,omitempty"`
	StepIndex    int                 `json:"stepIndex"`
	Seq          uint64              `json:"seq"`
	Chunk        string              `json:"chunk,omitempty"`
	State        string              `json:"state,omitempty"`
	Error        string              `json:"error,omitempty"`
	StartedAt    int64               `json:"startedAt,omitempty"`
	FinishedAt   int64               `json:"finishedAt,omitempty"`
}
