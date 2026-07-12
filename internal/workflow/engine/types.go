package engine

import (
	"context"
	"encoding/json"
	"errors"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/profile"
)

// ErrSetupFailed marks runner startup failures caused by workspace
// provisioning or setup hooks. The engine maps wrapped instances to the
// setup-failed reason instead of reporting an agent failure.
var ErrSetupFailed = errors.New("workflow setup failed")

type State string

const (
	StateQueued     State = "queued"
	StateRunning    State = "running"
	StateNeedsHuman State = "needs-human"
	StateDone       State = "done"
	StateFailed     State = "failed"
	StateCancelled  State = "cancelled"
)

type Reason string

const (
	ReasonGate               Reason = "gate"
	ReasonQuestion           Reason = "question"
	ReasonStuck              Reason = "stuck"
	ReasonStalled            Reason = "stalled"
	ReasonBudgetExhausted    Reason = "budget-exhausted"
	ReasonRetriesExhausted   Reason = "retries-exhausted"
	ReasonCheckFailedGenuine Reason = "check-failed-genuine"
	ReasonAgentError         Reason = "agent-error"
	ReasonWiringError        Reason = "wiring-error"
	ReasonDisposition        Reason = "disposition"
	ReasonSetupFailed        Reason = "setup-failed"
	ReasonInterrupted        Reason = "interrupted"
	ReasonTakenOver          Reason = "taken-over"
)

type OutcomeKind string

const (
	OutcomeDone               OutcomeKind = "done"
	OutcomeQuestion           OutcomeKind = "question"
	OutcomeStuck              OutcomeKind = "stuck"
	OutcomeStalled            OutcomeKind = "stalled"
	OutcomeTransientExhausted OutcomeKind = "transient-exhausted"
	OutcomeExecutionFailure   OutcomeKind = "execution-failure"
	OutcomeStopped            OutcomeKind = "stopped"
)

// Outcome is a runner completion. Envelope has already passed provider-facing
// post-validation; execution failures may omit it.
type Outcome struct {
	Kind     OutcomeKind     `json:"kind"`
	Envelope json.RawMessage `json:"envelope,omitempty"`
}

// RunKey uniquely identifies one persisted phase attempt.
type RunKey struct {
	ItemID  string `json:"itemId"`
	PhaseID string `json:"phaseId"`
	Attempt int    `json:"attempt"`
}

// RunRequest contains the immutable workflow snapshot plus phase-local input.
type RunRequest struct {
	Key              RunKey         `json:"key"`
	Item             store.WorkItem `json:"item"`
	Workflow         def.Workflow   `json:"workflow"`
	Phase            def.Phase      `json:"phase"`
	Vars             map[string]any `json:"vars"`
	Feedback         *Feedback      `json:"feedback,omitempty"`
	PriorThreadID    string         `json:"priorThreadId,omitempty"`
	FinalizeTakeover bool           `json:"finalizeTakeover,omitempty"`
}

type Feedback struct {
	Values map[string]any `json:"values,omitempty"`
	Note   string         `json:"note,omitempty"`
}

type PhaseInput struct {
	Vars     map[string]any `json:"vars"`
	Feedback *Feedback      `json:"feedback,omitempty"`
}

type HumanDecision string

const (
	HumanApprove HumanDecision = "approve"
	HumanReject  HumanDecision = "reject"
)

type HumanIntervention struct {
	Decision HumanDecision `json:"decision"`
	Note     string        `json:"note,omitempty"`
}

type TakeoverIntervention struct {
	Kind string `json:"kind"`
	At   int64  `json:"at"`
}

// Runner starts provider work on an engine-owned worker goroutine. Start must
// call entered exactly once, immediately on entry and before any blocking work.
// Start may then block while provisioning; its result is serialized back
// through the engine command loop. Stop is idempotent and returns any partial
// control envelope.
type Runner interface {
	Start(context.Context, RunRequest, func(), func(Outcome)) error
	Stop(context.Context, RunKey) (json.RawMessage, error)
	StopForTakeover(context.Context, RunKey) (json.RawMessage, error)
}

// Emitter is implemented by the later app/channel wiring packet. Emit runs on
// the engine owner goroutine and must return promptly; it must not call back
// into Engine synchronously.
type Emitter interface {
	Emit(eventName string, payload any)
}

// DefinitionSource resolves the validated workflow that is frozen at item
// start. It is not consulted after Snapshot has been persisted.
type DefinitionSource interface {
	Resolve(context.Context, store.WorkItem) (def.Workflow, error)
}

// ProfileSource returns the live project profile at each resource acquisition.
type ProfileSource interface {
	Profile(context.Context, string) (*profile.Profile, error)
}

// Spend is the attributed provider spend accumulated by one work item.
type Spend struct {
	Tokens int64   `json:"tokens"`
	USD    float64 `json:"usd"`
}

// SpendSource supplies token and composed wire-plus-estimated USD spend.
type SpendSource interface {
	ItemSpend(context.Context, string) (Spend, error)
}

// Config supplies startup queue state. Process-N is deliberately absent: it is
// an in-memory SetQueue bound and never survives an engine restart.
type Config struct {
	Active            bool
	GlobalConcurrency int
}

const MaxSeedBytes = 64 * 1024
const MaxSnapshotBytes = 4 * 1024 * 1024
const MaxGlobalConcurrency = 32

// Snapshot is the persisted, immutable run definition.
type Snapshot struct {
	Workflow def.Workflow `json:"workflow"`
}

type StateEvent struct {
	ItemID string `json:"itemId"`
	From   State  `json:"from"`
	To     State  `json:"to"`
	Reason Reason `json:"reason,omitempty"`
}

type QueueEvent struct {
	Active            bool `json:"active"`
	GlobalConcurrency int  `json:"globalConcurrency"`
	StartsRemaining   int  `json:"startsRemaining,omitempty"`
}

type PhaseEvent struct {
	ItemID  string `json:"itemId"`
	PhaseID string `json:"phaseId"`
	Attempt int    `json:"attempt"`
	Status  string `json:"status"`
}

type ErrorEvent struct {
	ItemID          string `json:"itemId,omitempty"`
	Error           string `json:"error"`
	Spend           *Spend `json:"spend,omitempty"`
	WallClockMillis int64  `json:"wallClockMillis,omitempty"`
	detail          error
}

func (e ErrorEvent) Cause() error { return e.detail }

type persistence interface {
	CreateWorkItem(store.WorkItem) error
	GetWorkItem(string) (store.WorkItem, error)
	ListWorkItems(store.WorkItemListFilter) ([]store.WorkItem, error)
	UpdateWorkItemState(string, string, string, int64) error
	UpdateWorkItemRunStart(string, json.RawMessage, string, string, string, int64) error
	ReorderQueuedWorkItems(string, []string) error
	CreateWorkItemPhase(store.WorkItemPhase) error
	CompleteWorkItemPhase(string, string, int, json.RawMessage, json.RawMessage, string, int64) error
	ListWorkItemPhases(string) ([]store.WorkItemPhase, error)
	UpdateWorkItemPhaseIntervention(string, string, int, json.RawMessage) error
	ListProjects() ([]store.Project, error)
}
