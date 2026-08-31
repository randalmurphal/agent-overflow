package workflowhost

import (
	"context"
	"encoding/json"

	"agent-overflow/internal/eventchan"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/memory"
	workflowrunner "agent-overflow/internal/workflow/runner"
)

// The workflow app runner's host seam.
//
// The runner used to hold `*App` and reach 19 of its members, which made every
// one of them part of the runner's contract by accident: nothing said what the
// runner actually needed, and a test could only fake the whole App. These are
// consumer-side interfaces in the style `internal/workflow/engine` already uses
// for its own boundaries: several narrow, capability-named seams rather than
// one wide one.
//
// `internal/app` satisfies them through one adapter (`workflowHostAdapter` in
// `internal/app/app_workflow_host.go`) whose methods forward to the App's
// unexported ones. The adapter is glue and nothing else: an interface declared
// outside `internal/app` cannot name an unexported method, and renaming ~19 App
// methods to exported would ripple further through that package than the
// forwards do.
//
// The store is deliberately NOT here: it is a dependency of the runner, not a
// capability of the process around it, and every other workflow collaborator in
// `internal/app` (`workflowProfileSource`, `workflowDefinitionSource`,
// `workflowSpendSource`) already holds `*store.Store` directly.

// SessionHost is the provider-session lifecycle a workflow element's turn runs
// inside. Every stop goes through `StopSession` (the spelling that honours the
// App's `stopSessionFn` test seam); the schema-restart path used to bypass it
// via the App's own exported `StopSession` binding, which was drift from the
// restart's original all-exported wiring, not a design choice.
//
// `SessionActive` is a bare liveness question rather than the App's session
// handle: both callers here discard the handle and read only the boolean, so
// the seam asks exactly what the runner decides on.
type SessionHost interface {
	StartSession(ctx context.Context, threadID string) error
	StartSessionTakingLock(ctx context.Context, threadID string) error
	StopSession(threadID string) error
	SessionActive(threadID string) bool
}

// TurnHost is what the runner does inside a live turn: watch the provider's
// events for the envelope that ends it, and send the prompt that starts it.
type TurnHost interface {
	SubscribeThreadTurnObserver(threadID string, observer func(string, provider.ProviderEvent)) func()
	SendWorkflowMessage(
		ctx context.Context, threadID, content string,
		outputSchema json.RawMessage, onDispatch func(DispatchIdentity),
	) error
}

// ThreadHost is the AO thread a workflow element occupies: creating the one its
// turn runs on, and reading back what it said when an attempt ended without
// authoring its own narrative.
type ThreadHost interface {
	CreateWorkflowThread(spec ThreadSpec) (store.Thread, error)
	ThreadAssistantTexts(threadID string) ([]string, error)
}

// WorktreeHost is the git working-copy provisioning a run's workspace needs
// (§9). `GitCore` is the whole Core rather than the handful of methods used
// through it because the package's own helpers take one.
type WorktreeHost interface {
	GitCore() *gitops.Core
	FindWorktree(project, path string) (gitops.Worktree, bool, error)
	CutWorktreeFromFreshBase(ctx context.Context, projectPath, worktreePath, baseBranch, newBranch string) error
	DefaultWorktreePath(projectPath, branch string) (string, error)
	WorktreeBranchPrefix() string
}

// PromptHost is the two ends of an element's context: the ancestry the prompt
// is assembled from, and the campaign memory an accepted envelope writes back.
type PromptHost interface {
	WorkflowPromptAncestry(itemID string, workflow def.Workflow) workflowrunner.PromptContext
	RecordEnvelopeMemory(key engine.RunKey, drafts []memory.Draft)
}

// EventEmitter is the runner's one channel to the frontend. Errors are
// user-facing state, so a runner failure that never reached an outcome still
// has somewhere to go.
type EventEmitter interface {
	Emit(name eventchan.Channel, data any)
}

// EngineSource reaches the engine the runner reports acks into. It is a lookup
// rather than a held pointer because the engine is constructed with the runner
// and can be torn down under it.
type EngineSource interface {
	RequireWorkflowEngine() (*engine.Engine, error)
}

// ProcessLifetime is the process-lifetime context a tool phase's child process
// group is bounded by, so a spawned command dies with the app.
type ProcessLifetime interface {
	LifeCtx() context.Context
}

// ProviderHomeSource answers where the process's provider state lives. It is a
// capability of the process, not of this package: an isolated boot (--harness /
// --soak) and a test fixture both pin a provider home that is not $HOME, and a
// runner that resolved `~/.claude/projects` itself would read the developer's
// real transcripts there. See App.providerHome.
type ProviderHomeSource interface {
	ClaudeProjectsDir() (string, error)
}

// Host composes the seams above into the single field the runner holds.
// Composition rather than one flat interface is what keeps each capability
// nameable on its own, and is what the move of the runner into this package
// carved along.
type Host interface {
	SessionHost
	TurnHost
	ThreadHost
	WorktreeHost
	PromptHost
	EventEmitter
	EngineSource
	ProcessLifetime
	ProviderHomeSource
}

// DispatchIdentity is the credential identity held stable across one provider
// write. Workflow usage-limit attribution consumes it at the same boundary as
// the send; it is not exposed on the wire and never gates a send.
type DispatchIdentity struct {
	Provider             string
	AccountID            string
	CredentialGeneration uint64
}
