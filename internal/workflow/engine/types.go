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

// ErrWiringFailed marks runner startup failures where the frozen definition and
// the live project profile cannot produce runnable work — a tool phase whose
// binding the profile no longer declares, an argument referencing a variable
// that is not in scope. The engine maps wrapped instances to the wiring-error
// reason, the same reason a gate that matched nothing parks with.
var ErrWiringFailed = errors.New("workflow wiring failed")

type State string

// A run has no queued state: admitting an item starts it. Contention is a
// phase waiting on resource capacity while its item stays running.
const (
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
	ReasonUnitFailed         Reason = "unit-failed"
	ReasonChildFailed        Reason = "child-failed"
	// ReasonPaused is a deliberate stop — the human pause action or the
	// graceful-quit path. It resumes exactly like ReasonInterrupted; the
	// distinct reason is what tells a human whether the run stopped on purpose
	// or because the process died (spec §12, D23).
	ReasonPaused Reason = "paused"
)

// ResumableReason reports whether a park continues on the provider session it
// parked on. Both members stopped an attempt mid-flight without the phase
// producing a result, so the next turn is a continuation of that session rather
// than a fresh attempt — the difference between them is provenance, not
// recovery.
func ResumableReason(reason Reason) bool {
	return reason == ReasonPaused || reason == ReasonInterrupted
}

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

// RunKey uniquely identifies one running piece of work: a phase attempt, or
// one unit of a fan-out phase attempt. An empty UnitID is the phase's own
// single attempt, so every existing key keeps meaning exactly what it meant.
type RunKey struct {
	ItemID  string `json:"itemId"`
	PhaseID string `json:"phaseId"`
	Attempt int    `json:"attempt"`
	UnitID  string `json:"unitId,omitempty"`
}

// UnitKind separates a fan-out's parallel workers from the join that runs after
// they rest. The join is an ordinary unit whose envelope becomes the phase's.
type UnitKind string

const (
	UnitWork UnitKind = "unit"
	UnitJoin UnitKind = "join"
)

// RunRequest contains the immutable workflow snapshot plus phase-local input.
// Unit, UnitIndex, UnitKind, and UnitAttempt are set exactly when Key.UnitID
// is: they carry the stamped unit definition a fan-out attempt expanded and the
// try number it is on, and Vars already includes the element binding a dynamic
// expansion bound to it (or, for a join, the units it consolidates).
type RunRequest struct {
	Key      RunKey         `json:"key"`
	Item     store.WorkItem `json:"item"`
	Workflow def.Workflow   `json:"workflow"`
	// WorkspaceNeed is the run's frozen workspace decision (§9) — derived at
	// start with write-need propagated through the call graph, so a read-only
	// root that calls a writing child still provisions the worktree its whole
	// tree shares. The runner provisions against this, never against a fresh
	// derivation from Workflow alone, which cannot see the child.
	WorkspaceNeed    def.WorkspaceNeed `json:"workspaceNeed,omitempty"`
	Phase            def.Phase         `json:"phase"`
	Unit             *def.Unit         `json:"unit,omitempty"`
	UnitIndex        int               `json:"unitIndex,omitempty"`
	UnitKind         UnitKind          `json:"unitKind,omitempty"`
	UnitAttempt      int               `json:"unitAttempt,omitempty"`
	Vars             map[string]any    `json:"vars"`
	Feedback         *Feedback         `json:"feedback,omitempty"`
	PriorThreadID    string            `json:"priorThreadId,omitempty"`
	FinalizeTakeover bool              `json:"finalizeTakeover,omitempty"`
}

type Feedback struct {
	Values map[string]any `json:"values,omitempty"`
	Note   string         `json:"note,omitempty"`
}

// PhaseInput is the frozen input of one phase attempt. Args is set only for a
// call phase: it is the evaluated argument map the child run was seeded with,
// which is what makes an invocation reproducible from the run record alone.
type PhaseInput struct {
	Vars     map[string]any `json:"vars"`
	Feedback *Feedback      `json:"feedback,omitempty"`
	Args     map[string]any `json:"args,omitempty"`
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

// TakeoverInterventionKind is the persisted marker that distinguishes an
// attempt detached for human steering from a human gate decision, which shares
// the intervention column.
const TakeoverInterventionKind = "taken-over"

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

// ResolvedDefinition is one validated workflow plus the two facts only a
// resolver can answer about it: which scope it came from (§8 project-over-shared
// precedence), and the workspace it needs once write-need has been propagated
// through its call graph (§9). `def` stays pure — it derives the single
// definition's need — so the propagated answer is produced where the loading is.
type ResolvedDefinition struct {
	Workflow      def.Workflow      `json:"workflow"`
	Scope         def.Scope         `json:"scope"`
	WorkspaceNeed def.WorkspaceNeed `json:"workspaceNeed"`
}

// DefinitionSource resolves the validated workflow frozen into a run record.
// Resolve answers for an item at start and is not consulted after that item's
// Snapshot is persisted. ResolveCall answers a call phase's static target by id
// at call time, under §8 scoping, so every invocation freezes the definition
// that was on disk when it was invoked.
type DefinitionSource interface {
	Resolve(context.Context, store.WorkItem) (ResolvedDefinition, error)
	ResolveCall(ctx context.Context, projectID, workflowID string) (ResolvedDefinition, error)
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

// SpendSource supplies token and composed wire-plus-estimated USD spend for a
// whole run tree. Budgets are enforced against the root item across every run
// it called (§12), so the aggregate — not one item's rows — is the number a
// ceiling is compared against.
type SpendSource interface {
	TreeSpend(ctx context.Context, rootItemID string) (Spend, error)
}

// Config supplies the persisted engine state restored at startup. Bounded
// parallelism is a resource fact (project profile capacities), not a config
// knob, so the global pause kill switch is all that survives a restart.
type Config struct {
	Paused bool
}

const MaxSeedBytes = 64 * 1024
const MaxSnapshotBytes = 4 * 1024 * 1024

// DefaultProviderCapacity bounds concurrent agent phases and fan-out units per
// provider when the project profile does not declare a `provider:<name>`
// capacity. The value is def's so the scheduler and the dry-run's width report
// can never disagree about the bound a run actually gets.
const DefaultProviderCapacity = def.DefaultProviderCapacity

// ProviderResource is the implicit resource every agent-driver phase and every
// agent-driver fan-out unit acquires in addition to the phase's declared
// resources. Capacity comes from the live project profile like any other
// resource, defaulting to DefaultProviderCapacity.
func ProviderResource(provider string) string { return def.ProviderResource(provider) }

// Snapshot is the persisted, immutable run definition. WorkspaceNeed is frozen
// with it because the answer depends on definitions outside this one (§9: write
// need propagates through call edges) — re-deriving it later from the frozen
// graph alone would silently drop a called workflow's writes. A snapshot frozen
// before this field existed leaves it empty, and the runner falls back to the
// single-definition derivation, which is exactly what it did then.
type Snapshot struct {
	Workflow      def.Workflow      `json:"workflow"`
	WorkspaceNeed def.WorkspaceNeed `json:"workspaceNeed,omitempty"`
}

type StateEvent struct {
	ItemID    string `json:"itemId"`
	ProjectID string `json:"projectId"`
	From      State  `json:"from"`
	To        State  `json:"to"`
	Reason    Reason `json:"reason,omitempty"`
}

// EngineState is both the Paused query result and the `workflow:engine-state`
// event payload, so the live flag has exactly one wire shape.
type EngineState struct {
	Paused bool `json:"paused"`
}

// PhaseEvent reports one phase attempt's status, or — when UnitID is set — one
// fan-out unit's status inside that attempt. Units ride the phase channel
// rather than a parallel one: a unit is a piece of the attempt, and a consumer
// that ignores UnitID still sees exactly the phase timeline it saw before.
type PhaseEvent struct {
	ItemID    string   `json:"itemId"`
	PhaseID   string   `json:"phaseId"`
	Attempt   int      `json:"attempt"`
	Status    string   `json:"status"`
	UnitID    string   `json:"unitId,omitempty"`
	UnitIndex int      `json:"unitIndex,omitempty"`
	UnitKind  UnitKind `json:"unitKind,omitempty"`
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
	CreateWorkItemPhase(store.WorkItemPhase) error
	CompleteWorkItemPhase(string, string, int, json.RawMessage, json.RawMessage, string, int64) error
	ReopenWorkItemPhase(string, string, int) error
	ListWorkItemPhases(string) ([]store.WorkItemPhase, error)
	ListWorkItemPhaseContexts(string) ([]store.WorkItemPhaseContext, error)
	UpdateWorkItemPhaseIntervention(string, string, int, json.RawMessage) error
	ListWorkItemChildren(string) ([]store.WorkItem, error)
	ListWorkItemCallChildren(string, string, int) ([]store.WorkItem, error)
	CreateWorkItemUnits([]store.WorkItemUnit) error
	StartWorkItemUnit(string, string, int, string, int, string, int64) error
	CompleteWorkItemUnit(string, string, int, string, string, json.RawMessage, string, int64) error
	RetryWorkItemUnit(string, string, int, string) error
	FailRunningWorkItemUnits(string, string, int, string, int64) (int64, error)
	ListWorkItemPhaseUnits(string, string, int) ([]store.WorkItemUnit, error)
	ListProjects() ([]store.Project, error)
	// ThreadExists tells a resume whether the session its parked attempt would
	// continue on is still there. Without it the engine would hand the runner a
	// dead thread id and take an agent-error park instead of the fresh attempt a
	// deleted session actually calls for.
	ThreadExists(string) (bool, error)
}
