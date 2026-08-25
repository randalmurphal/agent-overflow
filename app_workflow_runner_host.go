package main

import (
	"context"
	"encoding/json"

	"agent-overflow/internal/eventchan"
	gitops "agent-overflow/internal/git"
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
// one wide one. `*App` satisfies each implicitly, so there is no adapter and no
// wrapper method between the runner and what it calls.
//
// The store is deliberately NOT here: it is a dependency of the runner, not a
// capability of the process around it, and every other workflow collaborator in
// this package (`workflowProfileSource`, `workflowDefinitionSource`,
// `workflowSpendSource`) already holds `*store.Store` directly.

// workflowSessionHost is the provider-session lifecycle a workflow element's
// turn runs inside. Every stop goes through `stopSession` (the spelling that
// honours the App's `stopSessionFn` test seam); the schema-restart path used
// to bypass it via the exported `StopSession`, which was drift from the
// restart's original all-exported wiring, not a design choice.
type workflowSessionHost interface {
	startSession(ctx context.Context, threadID string) error
	startSessionTakingLock(ctx context.Context, threadID string) error
	stopSession(threadID string) error
	sessionManager() sessionManager
}

// workflowTurnHost is what the runner does inside a live turn: watch the
// provider's events for the envelope that ends it, and send the prompt that
// starts it.
type workflowTurnHost interface {
	subscribeThreadTurnObserver(threadID string, observer turnObserver) func()
	sendWorkflowMessage(
		ctx context.Context, threadID, content string,
		outputSchema json.RawMessage, onDispatch func(providerDispatchIdentity),
	) error
}

// workflowThreadHost is the AO thread a workflow element occupies: creating the
// one its turn runs on, and reading back what it said when an attempt ended
// without authoring its own narrative.
type workflowThreadHost interface {
	createWorkflowThread(spec workflowThreadSpec) (store.Thread, error)
	threadAssistantTexts(threadID string) ([]string, error)
}

// workflowWorktreeHost is the git working-copy provisioning a run's workspace
// needs (§9). `gitCore` is the whole Core rather than the handful of methods
// used through it because the package's own helpers take one.
type workflowWorktreeHost interface {
	gitCore() *gitops.Core
	findWorktree(project, path string) (gitops.Worktree, bool, error)
	cutWorktreeFromFreshBase(ctx context.Context, projectPath, worktreePath, baseBranch, newBranch string) error
	defaultWorktreePath(projectPath, branch string) (string, error)
	worktreeBranchPrefix() string
}

// workflowPromptHost is the two ends of an element's context: the ancestry the
// prompt is assembled from, and the campaign memory an accepted envelope
// writes back.
type workflowPromptHost interface {
	workflowPromptAncestry(itemID string, workflow def.Workflow) workflowrunner.PromptContext
	recordEnvelopeMemory(key engine.RunKey, drafts []memory.Draft)
}

// workflowEventEmitter is the runner's one channel to the frontend. Errors are
// user-facing state, so a runner failure that never reached an outcome still
// has somewhere to go.
type workflowEventEmitter interface {
	emit(name eventchan.Channel, data any)
}

// workflowEngineSource reaches the engine the runner reports acks into. It is a
// lookup rather than a held pointer because the engine is constructed with the
// runner and can be torn down under it.
type workflowEngineSource interface {
	requireWorkflowEngine() (*engine.Engine, error)
}

// workflowProcessLifetime is the process-lifetime context a tool phase's child
// process group is bounded by, so a spawned command dies with the app.
type workflowProcessLifetime interface {
	lifeCtx() context.Context
}

// workflowRunnerHost composes the seams above into the single field the runner
// holds. Composition rather than one flat interface is what keeps each
// capability nameable on its own, and is what a later move of the runner into
// its own package would carve along.
type workflowRunnerHost interface {
	workflowSessionHost
	workflowTurnHost
	workflowThreadHost
	workflowWorktreeHost
	workflowPromptHost
	workflowEventEmitter
	workflowEngineSource
	workflowProcessLifetime
}
