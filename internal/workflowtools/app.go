package workflowtools

import (
	"context"
	"encoding/json"
	"time"

	"agent-overflow/internal/workflow/catalog"
	"agent-overflow/internal/workflow/def"
)

// App is what the tools need from the application. Every method runs under
// the calling session's authority, which the app reads from ctx: the server
// is handed a Caller, and the app adapter that built it attached the same
// session's scope to the context before calling Server.Call. Results are the
// app's own documents, returned unchanged so the tool never becomes a second
// definition of what a run looks like; only the facts a wait decides on are
// typed.
type App interface {
	// ListWorkflows, ValidateWorkflow and ScaffoldWorkflow work on the calling
	// session's project.
	ListWorkflows(ctx context.Context) (catalog.Listing, error)
	ValidateWorkflow(ctx context.Context, id, path string) (def.ValidationResult, error)
	ScaffoldWorkflow(ctx context.Context, starter, id string, project bool) (catalog.ScaffoldResult, error)

	StartRun(ctx context.Context, input StartInput) (Started, error)
	RunStatus(ctx context.Context, runID string) (any, error)
	// HoldWake keeps the run's rests from waking the calling thread while a
	// call waits on it, until the hold is released. A rest the call returned
	// is delivered; any other rest the hold kept back wakes the thread on
	// release. A run bound to another thread is not held.
	HoldWake(ctx context.Context, runID string) WakeHold
	// WatchRun returns once the run has transitions after the cursor, is
	// resting, or Wait passes. A zero cursor answers at once.
	WatchRun(ctx context.Context, input WatchInput) (Watched, error)
	// RunBoundThread names the thread a run reports its rests to, empty when
	// it reports to none.
	RunBoundThread(ctx context.Context, runID string) (string, error)
	RunOutput(ctx context.Context, runID string) (any, error)
	InspectRun(ctx context.Context, input InspectInput) (any, error)
	RunNarrative(ctx context.Context, input NarrativeInput) (any, error)
	ListRuns(ctx context.Context, activeOnly bool) (any, error)
	// ControlRun acts on a run. A scheduled resume reports the moment it
	// armed; every other action reports nothing.
	ControlRun(ctx context.Context, input ControlInput) (ControlDone, error)
	AmendSeeds(ctx context.Context, runID string, seeds json.RawMessage) (any, error)
	GuideRun(ctx context.Context, runID, text string) (any, error)
	ResolveRun(ctx context.Context, input ResolveInput) error
	AddMemory(ctx context.Context, input MemoryInput) (any, error)
	ListMemory(ctx context.Context, runID, kind string) (any, error)
}

// StartInput is run_start's request. HoldWake asks for the new run's wake
// hold, taken before the run is bound to the caller's thread so no rest of it
// can wake the thread past the call that is about to wait on it.
type StartInput struct {
	WorkflowID string
	Scope      string
	Goal       string
	Seeds      json.RawMessage
	BaseBranch string
	StepMode   bool
	HoldWake   bool
}

// Started is a run start: the run's id, the app's start document, and the
// wake hold when one was asked for.
type Started struct {
	RunID  string
	Result any
	Hold   WakeHold
}

// WakeHold is a waiting call's claim on a run's wake. Release ends it exactly
// once; returned is the rest the call handed back, nil when it handed back
// none. The app counts it delivered only once the response carrying it is
// written.
type WakeHold interface {
	Release(returned *Rest)
}

// Rest is a rest a wait read: the watch cursor it read the run at, and the
// state and reason the run showed there. It names that one rest, so a later
// rest that looks the same is not mistaken for it.
type Rest struct {
	Cursor int64
	State  string
	Reason string
}

// ControlDone is what an action reports before the run is read back.
type ControlDone struct {
	// ArmedAt is the moment a scheduled resume will fire.
	ArmedAt string
}

// WatchInput is one bounded wait on a run's transitions.
type WatchInput struct {
	RunID  string
	Cursor int64
	Tree   bool
	Wait   time.Duration
}

// Watched is one wait's answer: the facts the tool loops on, and the app's
// document.
type Watched struct {
	Cursor      int64
	Transitions int
	Resting     bool
	State       string
	Reason      string
	Result      any
}

// InspectInput is run_inspect's request.
type InspectInput struct {
	RunID   string
	PhaseID string
	Attempt int
}

// NarrativeInput is run_narrative's request.
type NarrativeInput struct {
	RunID   string
	PhaseID string
	Attempt int
	UnitID  string
}

// ControlInput is run_control's request. Only the fields its action takes are
// set; the server refuses the rest before the app sees them.
type ControlInput struct {
	RunID      string
	Action     string
	PhaseID    string
	RefreshDef bool
	At         string
	Guidance   string
	UnitID     string
	Note       string
	Clear      bool
}

// ResolveInput is run_resolve's request.
type ResolveInput struct {
	RunID    string
	Decision string
	Note     string
	Answer   string
}

// MemoryInput is memory_add's request.
type MemoryInput struct {
	RunID string
	Kind  string
	Text  string
	Files []string
}
