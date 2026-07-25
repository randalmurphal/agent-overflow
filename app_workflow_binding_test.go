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
	if _, err := h.app.WorkflowOpenInThread(child.ID); err == nil ||
		!strings.Contains(err.Error(), "bind the run that called it") {
		t.Fatalf("open-in-thread of a called run = %v, want a refusal", err)
	}
}

func TestWorkflowOpenInThreadSeedsTheRunStateAndBinds(t *testing.T) {
	h := newWakeHarness(t)
	item := h.run(t, "open-run", engine.StateNeedsHuman, engine.ReasonQuestion)
	h.phase(t, item.ID, "plan", 1, "parked", "phase-thread",
		[]byte(`{"status":"question","question":"Which base branch?"}`))

	thread, err := h.app.WorkflowOpenInThread(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if thread.Mode != threadmode.ModeChat {
		t.Fatalf("opened a %q thread, want a chat thread", thread.Mode)
	}
	if !strings.Contains(thread.Title, "Ship open-run") {
		t.Fatalf("thread title = %q, want the run goal in it", thread.Title)
	}
	stored, err := h.app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OriginThreadID != thread.ID {
		t.Fatalf("run bound to %q, want the new thread %q", stored.OriginThreadID, thread.ID)
	}
	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("seed messages = %d, want one", len(sends))
	}
	for _, want := range []string{`Run "open-run"`, "needs-human (question)", "Which base branch?"} {
		if !strings.Contains(sends[0], want) {
			t.Fatalf("seed message missing %q:\n%s", want, sends[0])
		}
	}

	// Opening again is the same conversation, not a fork of it.
	again, err := h.app.WorkflowOpenInThread(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != thread.ID {
		t.Fatalf("second open created thread %q, want the bound %q", again.ID, thread.ID)
	}
	if sends, _, _, _ := h.snapshot(); len(sends) != 1 {
		t.Fatalf("second open sent another seed: %d sends", len(sends))
	}
}

// A binding whose thread was deleted must not strand the run: opening it again
// creates a fresh conversation instead of resolving to a thread that is gone.
func TestWorkflowOpenInThreadReplacesADeletedBinding(t *testing.T) {
	h := newWakeHarness(t)
	item := h.run(t, "open-orphan", engine.StateDone, "")
	first, err := h.app.WorkflowOpenInThread(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.app.DeleteThread(first.ID); err != nil {
		t.Fatal(err)
	}
	// Thread deletion clears the binding rather than leaving a dangling one.
	stored, err := h.app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OriginThreadID != "" {
		t.Fatalf("deleting the bound thread left binding %q", stored.OriginThreadID)
	}

	second, err := h.app.WorkflowOpenInThread(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatal("re-opened the deleted thread")
	}
	rebound, err := h.app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rebound.OriginThreadID != second.ID {
		t.Fatalf("run bound to %q, want %q", rebound.OriginThreadID, second.ID)
	}
}
