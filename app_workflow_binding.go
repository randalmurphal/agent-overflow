package main

import (
	"fmt"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtitle"
)

// Thread binding (decision D17).
//
// A run bound to a thread reports back into that thread: every resting
// transition composes a wake and injects it as a user message, so the agent (or
// human) that started the run is told the run finished where they were working,
// rather than having to watch an overlay.
//
// Only a ROOT run carries a binding. A called run's results reach its caller
// through the call phase, and its parks surface at the root — giving it a
// binding of its own would put two runs' results into one conversation with no
// way to tell which one the human is answering.

// WorkflowBindThread makes an existing conversation thread the run's origin.
// The run's results are delivered there from then on, replacing any previous
// binding.
//
// LocalOnly: it associates a local run record with a local provider session.
func (a *App) WorkflowBindThread(itemID, threadID string) (store.WorkItem, error) {
	item, err := a.workflowBindableItem(itemID)
	if err != nil {
		return store.WorkItem{}, err
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return store.WorkItem{}, fmt.Errorf("bind workflow run %s: thread id is required", item.ID)
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.WorkItem{}, fmt.Errorf("bind workflow run %s: %w", item.ID, err)
	}
	if thread.Archived {
		return store.WorkItem{}, fmt.Errorf("bind workflow run %s: thread %s is archived", item.ID, thread.ID)
	}
	if err := validWorkflowBindingThread(item, thread); err != nil {
		return store.WorkItem{}, fmt.Errorf("bind workflow run %s: %w", item.ID, err)
	}
	if err := a.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
		return store.WorkItem{}, err
	}
	return a.store.GetWorkItem(item.ID)
}

// WorkflowUnbindThread drops a run's origin binding. Its results go back to the
// workflows overlay and the OS notification.
//
// LocalOnly: same surface as WorkflowBindThread.
func (a *App) WorkflowUnbindThread(itemID string) (store.WorkItem, error) {
	item, err := a.workflowBindableItem(itemID)
	if err != nil {
		return store.WorkItem{}, err
	}
	if err := a.store.UpdateWorkItemOriginThread(item.ID, ""); err != nil {
		return store.WorkItem{}, err
	}
	return a.store.GetWorkItem(item.ID)
}

// WorkflowOpenInThread opens a conversation about a run and binds it, so the
// run's future results land in the same place. The thread starts with the run's
// current state as its first user message — the same composed wake a resting
// transition delivers — so the agent begins with the run's results in context
// rather than being asked about a run it knows nothing about.
//
// A run that is already bound to a usable thread returns that thread untouched:
// the point of the binding is that there is one conversation per run, and
// opening it twice must not fork that conversation.
//
// LocalOnly: it creates a local thread and starts a provider session.
func (a *App) WorkflowOpenInThread(itemID string) (store.Thread, error) {
	item, err := a.workflowBindableItem(itemID)
	if err != nil {
		return store.Thread{}, err
	}
	unlock := a.threadLocks().Lock("workflow-item-origin:" + item.ID)
	defer unlock()
	// Re-read under the lock: two clicks race here, and the second must see the
	// binding the first one wrote.
	item, err = a.store.GetWorkItem(item.ID)
	if err != nil {
		return store.Thread{}, err
	}
	if item.OriginThreadID != "" {
		if threadID, ok := a.resolveWakeThread(item); ok {
			return a.store.GetThread(threadID)
		}
	}
	message, err := a.composeWorkflowWake(item, nil)
	if err != nil {
		return store.Thread{}, fmt.Errorf("open workflow run %s in a thread: %w", item.ID, err)
	}
	opts, err := a.workflowOriginThreadOptions(item)
	if err != nil {
		return store.Thread{}, err
	}
	thread, err := a.CreateThread(opts)
	if err != nil {
		return store.Thread{}, fmt.Errorf("open workflow run %s in a thread: %w", item.ID, err)
	}
	if err := a.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
		return store.Thread{}, err
	}
	if _, err := a.sendMessageWithOptions(thread.ID, message, sendMessageOptions{PreserveDraft: true}); err != nil {
		// The thread exists and is bound; the seed turn is what failed. Leave
		// both in place — deleting a thread the user can already see would be a
		// worse outcome than an empty one they can type into — and report it.
		return thread, fmt.Errorf("open workflow run %s in a thread: kick off agent: %w", item.ID, err)
	}
	return a.store.GetThread(thread.ID)
}

// workflowOriginThreadOptions builds the new thread in the run's own workspace
// when it still has one, so the conversation can inspect the work it is about.
// A run whose worktree was disposed of falls back to the project root; that is
// an expected end state, not a failure.
func (a *App) workflowOriginThreadOptions(item store.WorkItem) (CreateThreadOptions, error) {
	project, err := a.store.GetProject(item.ProjectID)
	if err != nil {
		return CreateThreadOptions{}, err
	}
	opts := CreateThreadOptions{
		ProjectID: item.ProjectID,
		Mode:      threadmode.ModeChat,
		Title:     threadtitle.Sanitize("Workflow run: " + item.Goal),
	}
	worktreePath := strings.TrimSpace(item.WorktreePath)
	if worktreePath == "" || gitops.SameFilesystemPath(worktreePath, project.Path) {
		return opts, nil
	}
	worktree, found, err := a.findWorktree(project.Path, worktreePath)
	if err != nil {
		return CreateThreadOptions{}, fmt.Errorf("open workflow run %s in a thread: %w", item.ID, err)
	}
	if !found {
		return opts, nil
	}
	opts.WorktreePath = worktree.Path
	opts.Branch = worktree.Branch
	return opts, nil
}

// workflowBindableItem loads a run and refuses the ones a binding cannot
// describe. Refusing a called run here is what keeps "children never notify as
// themselves" structural rather than conventional.
func (a *App) workflowBindableItem(itemID string) (store.WorkItem, error) {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return store.WorkItem{}, fmt.Errorf("workflow thread binding: item id is required")
	}
	item, err := a.store.GetWorkItem(itemID)
	if err != nil {
		return store.WorkItem{}, err
	}
	if item.ParentItemID != "" {
		return store.WorkItem{}, fmt.Errorf(
			"workflow thread binding %s: this run was called by %s; bind the run that called it",
			item.ID, item.ParentItemID,
		)
	}
	return item, nil
}
