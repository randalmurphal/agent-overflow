package main

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
	"agent-overflow/internal/workflowhost"
)

// workflowHostAdapter is `main`'s implementation of the workflow runner's host
// seams (`internal/workflowhost/host.go`). Every method is a forward to the
// App's own, unexported one and nothing else: an interface declared outside
// `main` cannot name an unexported method, and exporting ~19 App methods for
// this one consumer would ripple through `main` far further than these forwards
// do. Behavior belongs on the App methods; this file must stay pure glue.
type workflowHostAdapter struct{ app *App }

var _ workflowhost.Host = workflowHostAdapter{}

func (h workflowHostAdapter) StartSession(ctx context.Context, threadID string) error {
	return h.app.startSession(ctx, threadID)
}

func (h workflowHostAdapter) StartSessionTakingLock(ctx context.Context, threadID string) error {
	return h.app.startSessionTakingLock(ctx, threadID)
}

func (h workflowHostAdapter) StopSession(threadID string) error {
	return h.app.stopSession(threadID)
}

func (h workflowHostAdapter) SessionActive(threadID string) bool {
	_, active := h.app.sessionManager().get(threadID)
	return active
}

func (h workflowHostAdapter) SubscribeThreadTurnObserver(
	threadID string, observer func(string, provider.ProviderEvent),
) func() {
	return h.app.subscribeThreadTurnObserver(threadID, observer)
}

func (h workflowHostAdapter) SendWorkflowMessage(
	ctx context.Context, threadID, content string,
	outputSchema json.RawMessage, onDispatch func(workflowhost.DispatchIdentity),
) error {
	return h.app.sendWorkflowMessage(ctx, threadID, content, outputSchema, onDispatch)
}

func (h workflowHostAdapter) CreateWorkflowThread(spec workflowhost.ThreadSpec) (store.Thread, error) {
	return h.app.createWorkflowThread(spec)
}

func (h workflowHostAdapter) ThreadAssistantTexts(threadID string) ([]string, error) {
	return h.app.threadAssistantTexts(threadID)
}

func (h workflowHostAdapter) GitCore() *gitops.Core { return h.app.gitCore() }

func (h workflowHostAdapter) FindWorktree(project, path string) (gitops.Worktree, bool, error) {
	return h.app.findWorktree(project, path)
}

func (h workflowHostAdapter) CutWorktreeFromFreshBase(
	ctx context.Context, projectPath, worktreePath, baseBranch, newBranch string,
) error {
	return h.app.cutWorktreeFromFreshBase(ctx, projectPath, worktreePath, baseBranch, newBranch)
}

func (h workflowHostAdapter) DefaultWorktreePath(projectPath, branch string) (string, error) {
	return h.app.defaultWorktreePath(projectPath, branch)
}

func (h workflowHostAdapter) WorktreeBranchPrefix() string { return h.app.worktreeBranchPrefix() }

func (h workflowHostAdapter) WorkflowPromptAncestry(
	itemID string, workflow def.Workflow,
) workflowrunner.PromptContext {
	return h.app.workflowPromptAncestry(itemID, workflow)
}

func (h workflowHostAdapter) RecordEnvelopeMemory(key engine.RunKey, drafts []memory.Draft) {
	h.app.recordEnvelopeMemory(key, drafts)
}

func (h workflowHostAdapter) Emit(name eventchan.Channel, data any) { h.app.emit(name, data) }

func (h workflowHostAdapter) RequireWorkflowEngine() (*engine.Engine, error) {
	return h.app.requireWorkflowEngine()
}

func (h workflowHostAdapter) LifeCtx() context.Context { return h.app.lifeCtx() }

// newWorkflowAppRunner builds the workflow runner against this App. It is the
// one place the adapter is constructed, so nothing else in `main` has to know
// the seam exists.
func newWorkflowAppRunner(app *App, dataRoot string, profiles engine.ProfileSource) *workflowhost.Runner {
	return workflowhost.New(
		workflowHostAdapter{app: app}, app.store, dataRoot, profiles, app.interruptTurnCtx,
	)
}
