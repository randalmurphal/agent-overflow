package main

import (
	"context"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/engine"
)

// `run guide`'s app half. The engine's rules are its own (see
// internal/workflow/engine/guidance_test.go); what this asserts is that the verb
// reaches them through a real engine, that the author is the caller's rather than
// the request's, and that the read verbs afterwards report the entry — guidance
// no surface showed would be guidance an operator cannot confirm.

// The author is stamped from the AUTHENTICATED caller. An entry that could claim
// "a human said this" would make the attribution in the delivered prompt worth
// nothing, so the request has no field for it at all.
func TestGuidanceAuthorComesFromTheCallerNotTheRequest(t *testing.T) {
	interactive := guidanceDraftFor(transport.WithCallerScope(context.Background(),
		transport.CallerScope{Kind: transport.ScopeKindInteractive, ThreadID: "thread-1"}), "steer")
	if interactive.By != engine.GuidanceByHuman || interactive.ByRun != "" {
		t.Fatalf("interactive draft = %+v, want a human author", interactive)
	}

	phase := guidanceDraftFor(transport.WithCallerScope(context.Background(),
		transport.CallerScope{
			Kind: transport.ScopeKindPhase, ThreadID: "babysitter",
			ItemID: "supervisor", PhaseID: "watch",
		}), "steer")
	if phase.By != engine.GuidanceByPhase || phase.ByRun != "supervisor" {
		t.Fatalf("phase draft = %+v, want an agent author naming its run", phase)
	}

	// No scope at all is the desktop UI calling in process. It is a person at a
	// keyboard, which is what the human stamp means.
	if bare := guidanceDraftFor(context.Background(), "steer"); bare.By != engine.GuidanceByHuman {
		t.Fatalf("unscoped draft = %+v, want a human author", bare)
	}
}

func TestWorkflowAgentGuideRunIsVisibleToTheReadVerbs(t *testing.T) {
	fixture, item := newAmendFixture(t)
	ctx := transport.WithCallerScope(context.Background(), interactiveScope(fixture, "thread-1"))

	result, err := fixture.app.WorkflowAgentGuideRun(ctx,
		WorkflowAgentGuideRunInput{ItemID: item.ID, Text: "prefer the smaller diff"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Pending != 1 || result.MaxPending != engine.MaxGuidanceEntries {
		t.Fatalf("guide result = %+v", result)
	}
	if result.By != string(engine.GuidanceByHuman) {
		t.Fatalf("author = %q, want the human stamp", result.By)
	}
	if result.DeliversNote == "" {
		t.Fatal("the result did not say when the run reads it")
	}
	if result.CallerNote != "" {
		t.Fatalf("a root run's guidance named a caller: %q", result.CallerNote)
	}

	// `run status` carries the COUNT — what a reader of a run's state needs.
	status, err := fixture.app.WorkflowAgentRunStatus(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.PendingGuidance != 1 {
		t.Fatalf("run status pending guidance = %d, want 1", status.PendingGuidance)
	}

	// `run inspect` carries the entries, with who left each one and how long it
	// has been waiting.
	inspected, err := fixture.app.WorkflowAgentInspectRun(ctx,
		WorkflowAgentInspectInput{ItemID: item.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(inspected.Guidance) != 1 {
		t.Fatalf("inspected guidance = %+v, want one entry", inspected.Guidance)
	}
	entry := inspected.Guidance[0]
	if entry.Text != "prefer the smaller diff" || entry.By != string(engine.GuidanceByHuman) {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.At == 0 || entry.AgeSeconds < 0 {
		t.Fatalf("entry has no usable age: %+v", entry)
	}
}

// The app's refusal reaches the caller with the engine's own words: the numbers
// live in one place, and a run that cannot be steered has to say why.
func TestWorkflowAgentGuideRunForwardsTheEnginesRefusal(t *testing.T) {
	fixture, item := newAmendFixture(t)
	ctx := transport.WithCallerScope(context.Background(), interactiveScope(fixture, "thread-1"))

	_, err := fixture.app.WorkflowAgentGuideRun(ctx,
		WorkflowAgentGuideRunInput{ItemID: item.ID, Text: "   "})
	if err == nil || !strings.Contains(err.Error(), "cannot be empty") {
		t.Fatalf("empty guidance refusal = %v", err)
	}
	// Nothing was written, so the read verbs still report an unguided run.
	status, err := fixture.app.WorkflowAgentRunStatus(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.PendingGuidance != 0 {
		t.Fatalf("a refused guide left %d entries pending", status.PendingGuidance)
	}
}

// An attempt that ran on a session an earlier attempt of the same phase started
// is reported as continued. No column records the mode: reusing the thread is
// what a continuation IS, and the two rows' shared thread id is the evidence.
func TestPhaseAttemptsReportAContinuedSession(t *testing.T) {
	h := newInspectHarness(t)
	item := h.run(t, store.WorkItem{
		ID: "root", WorkflowID: "flow", State: string(engine.StateRunning),
	})
	// fix/1 starts the session, fix/2 continues it, fix/3 is a cold re-entry, and
	// review/1 shares no phase with any of them.
	for _, attempt := range []struct {
		phaseID  string
		attempt  int
		threadID string
		started  int64
	}{
		{"fix", 1, "fix-thread", 10},
		{"fix", 2, "fix-thread", 20},
		{"fix", 3, "fix-thread-2", 30},
		{"review", 1, "review-thread", 40},
	} {
		h.phase(t, store.WorkItemPhase{
			ItemID: item.ID, PhaseID: attempt.phaseID, Attempt: attempt.attempt,
			ThreadID: attempt.threadID, StartedAt: attempt.started,
		})
	}

	attempts, err := h.app.workflowAgentPhaseAttempts(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"", workflowSessionContinued, "", ""}
	if len(attempts) != len(want) {
		t.Fatalf("attempts = %#v", attempts)
	}
	for index, attempt := range attempts {
		if attempt.Session != want[index] {
			t.Fatalf("%s attempt %d session = %q, want %q",
				attempt.PhaseID, attempt.Attempt, attempt.Session, want[index])
		}
	}
}

// An attempt with no thread at all — a tool-driver phase runs a command — is
// never reported as continued, and never as fresh either: it has no session for
// the field to describe.
func TestPhaseAttemptsWithoutAThreadCarryNoSession(t *testing.T) {
	h := newInspectHarness(t)
	item := h.run(t, store.WorkItem{
		ID: "root", WorkflowID: "flow", State: string(engine.StateRunning),
	})
	h.phase(t, store.WorkItemPhase{ItemID: item.ID, PhaseID: "check", Attempt: 1, StartedAt: 10})
	h.phase(t, store.WorkItemPhase{ItemID: item.ID, PhaseID: "check", Attempt: 2, StartedAt: 20})

	attempts, err := h.app.workflowAgentPhaseAttempts(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range attempts {
		if attempt.Session != "" {
			t.Fatalf("threadless attempt %d reported session %q", attempt.Attempt, attempt.Session)
		}
	}
}
