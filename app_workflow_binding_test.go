package main

import (
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/workflow/engine"
)

func TestWorkflowBindThreadRefusesThreadsARunCannotReportInto(t *testing.T) {
	h := newWakeHarness(t)
	root := h.run(t, "bind-root", engine.StateRunning, "")
	child := store.WorkItem{
		ID: "bind-child", ProjectID: defaultTestProjectID, Goal: "called", WorkflowID: "c",
		WorkflowScope: "shared", State: string(engine.StateRunning), Source: "call",
		ParentItemID: root.ID, ParentPhaseID: "call", ParentAttempt: 1, CallDepth: 1, CreatedAt: 2,
	}
	if err := h.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}
	if err := h.app.store.CreateProject(store.Project{
		ID: "other-project", Name: "other", Path: "/tmp/other", Slug: "other", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	makeThread := func(id, projectID, mode string, archived bool) store.Thread {
		thread := store.Thread{
			ID: id, ProjectID: projectID, ProjectPath: "/tmp/project", Title: id,
			Provider: string(provider.Claude), Model: "sonnet", Mode: mode,
			Archived: archived, CreatedAt: 1, UpdatedAt: 1,
		}
		if err := h.app.store.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
		return thread
	}
	makeThread("cross-project", "other-project", threadmode.ModeChat, false)
	makeThread("phase-thread", defaultTestProjectID, threadmode.ModeWorkflow, false)
	makeThread("archived-thread", defaultTestProjectID, threadmode.ModeChat, true)
	makeThread("ok-thread", defaultTestProjectID, threadmode.ModeChat, false)

	for _, tc := range []struct{ name, itemID, threadID, want string }{
		{"called run", child.ID, "ok-thread", "bind the run that called it"},
		{"unknown run", "nope", "ok-thread", "no rows in result set"},
		{"missing thread id", root.ID, "  ", "thread id is required"},
		{"unknown thread", root.ID, "ghost", "no rows in result set"},
		{"cross project", root.ID, "cross-project", "belongs to project"},
		{"workflow phase thread", root.ID, "phase-thread", "binds a conversation thread"},
		{"archived thread", root.ID, "archived-thread", "is archived"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.app.WorkflowBindThread(tc.itemID, tc.threadID); err == nil {
				t.Fatalf("bind succeeded, want a refusal containing %q", tc.want)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
			stored, err := h.app.store.GetWorkItem(root.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.OriginThreadID != "" {
				t.Fatalf("a refused bind still wrote a binding: %q", stored.OriginThreadID)
			}
		})
	}
}

func TestWorkflowBindAndUnbindThreadRoundTrip(t *testing.T) {
	h := newWakeHarness(t)
	first := h.chatThread(t, "bind-first")
	second := h.chatThread(t, "bind-second")
	item := h.run(t, "bind-run", engine.StateRunning, "")

	bound, err := h.app.WorkflowBindThread(item.ID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.OriginThreadID != first.ID {
		t.Fatalf("bound to %q, want %q", bound.OriginThreadID, first.ID)
	}
	// Rebinding replaces rather than accumulating: one run, one conversation.
	rebound, err := h.app.WorkflowBindThread(item.ID, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rebound.OriginThreadID != second.ID {
		t.Fatalf("rebound to %q, want %q", rebound.OriginThreadID, second.ID)
	}
	unbound, err := h.app.WorkflowUnbindThread(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unbound.OriginThreadID != "" {
		t.Fatalf("unbind left %q", unbound.OriginThreadID)
	}
	// An unbound run rests without waking anything.
	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateDone,
	})
	h.drain()
	if sends, queued, _, _ := h.snapshot(); len(sends) != 0 || len(queued) != 0 {
		t.Fatalf("unbound run still delivered: sends=%d queued=%d", len(sends), len(queued))
	}
}

func TestWorkflowUnbindRefusesACalledRun(t *testing.T) {
	h := newWakeHarness(t)
	root := h.run(t, "unbind-root", engine.StateRunning, "")
	child := store.WorkItem{
		ID: "unbind-child", ProjectID: defaultTestProjectID, Goal: "called", WorkflowID: "c",
		WorkflowScope: "shared", State: string(engine.StateRunning), Source: "call",
		ParentItemID: root.ID, ParentPhaseID: "call", ParentAttempt: 1, CallDepth: 1, CreatedAt: 2,
	}
	if err := h.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.WorkflowUnbindThread(child.ID); err == nil ||
		!strings.Contains(err.Error(), "bind the run that called it") {
		t.Fatalf("unbind of a called run = %v, want a refusal", err)
	}
}

// Thread deletion must clear the binding rather than leaving a dangling one:
// a run whose origin thread is gone has to fall back to the overlay, not keep
// trying to wake a thread that no longer exists.
func TestDeletingABoundThreadClearsTheBinding(t *testing.T) {
	h := newWakeHarness(t)
	item := h.run(t, "orphan-run", engine.StateRunning, "")
	thread := h.chatThread(t, "orphan-thread")
	if _, err := h.app.WorkflowBindThread(item.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.app.DeleteThread(thread.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := h.app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OriginThreadID != "" {
		t.Fatalf("deleting the bound thread left binding %q", stored.OriginThreadID)
	}
	// And the run rests silently rather than waking a thread that is gone.
	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateDone,
	})
	h.drain()
	if sends, queued, _, errorTexts := h.snapshot(); len(sends) != 0 || len(queued) != 0 || len(errorTexts) != 0 {
		t.Fatalf("orphaned run delivered: sends=%d queued=%d errors=%v", len(sends), len(queued), errorTexts)
	}
}
