package main

import (
	"fmt"
	"strings"

	"agent-overflow/internal/store"
)

// Thread binding (decision D17).
//
// A run bound to a thread reports back into that thread: every resting
// transition composes a wake and injects it as a user message, so the agent (or
// human) that started the run is told the run finished where they were working,
// rather than having to watch an overlay.
//
// A binding is always made against a thread that already exists — the CLI's
// `run start` auto-bind to its origin thread, or an explicit bind here. D32
// removed the seed-a-new-thread-and-bind-it affordance; nothing on a workflow
// surface creates a conversation any more.
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
