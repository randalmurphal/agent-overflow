package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

func TestStartupCrashSweepStopsAndParksInterruptedAttempt(t *testing.T) {
	workflow := onePhaseWorkflow("crash", []string{"stack"}, []def.Route{{To: "done"}})
	snapshot, err := json.Marshal(Snapshot{Workflow: workflow})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Config{}, map[string]def.Workflow{"crash": workflow}, []string{"project"}, func(database *store.Store) {
		item := testItem("crashed", "project", "crash", 0)
		if err := database.CreateWorkItem(item); err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateWorkItemRunStart(item.ID, snapshot, "", "", "", 20); err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateWorkItemState(item.ID, string(StateRunning), "", 0); err != nil {
			t.Fatal(err)
		}
		if err := database.CreateWorkItemPhase(store.WorkItemPhase{
			ItemID: item.ID, PhaseID: "work", Attempt: 1,
			InputEnvelope: json.RawMessage(`{}`), Status: "running", StartedAt: 21,
		}); err != nil {
			t.Fatal(err)
		}
	})
	requireItemState(t, h.store, "crashed", StateNeedsHuman, ReasonInterrupted)
	if h.runner.stopCount() != 1 {
		t.Fatalf("crash sweep stop count = %d, want 1", h.runner.stopCount())
	}
	if len(h.runner.started()) != 0 {
		t.Fatalf("crashed phase was auto-rerun: %+v", h.runner.started())
	}
	h.profiles.setCapacity("project", "stack", 1)
	if err := h.engine.StartItem(testItem("after-crash", "project", "crash", 1)); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 1 || starts[0].Key.ItemID != "after-crash" {
		t.Fatalf("crash sweep left a phantom resource holder: %+v", starts)
	}
	phases, err := h.store.ListWorkItemPhases("crashed")
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0].Status != "parked" || phases[0].EndedAt == 0 {
		t.Fatalf("crash-swept phase = %+v", phases)
	}
}

func TestStartupCrashSweepPreservesPersistedAnswerAttempt(t *testing.T) {
	workflow := onePhaseWorkflow("answer-crash", nil, []def.Route{{To: "done"}})
	snapshot, err := json.Marshal(Snapshot{Workflow: workflow})
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(PhaseInput{Vars: map[string]any{}, Feedback: &Feedback{Note: "persisted answer"}})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Config{}, map[string]def.Workflow{"answer-crash": workflow}, []string{"project"}, func(database *store.Store) {
		item := testItem("answer-crash", "project", "answer-crash", 0)
		if err := database.CreateWorkItem(item); err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateWorkItemRunStart(item.ID, snapshot, "", "", "", 20); err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateWorkItemState(item.ID, string(StateRunning), "", 0); err != nil {
			t.Fatal(err)
		}
		if err := database.CreateWorkItemPhase(store.WorkItemPhase{
			ItemID: item.ID, PhaseID: "work", Attempt: 2, ThreadID: "question-thread",
			InputEnvelope: input, Status: "running", StartedAt: 21,
		}); err != nil {
			t.Fatal(err)
		}
	})
	requireItemState(t, h.store, "answer-crash", StateNeedsHuman, ReasonInterrupted)
	phases, err := h.store.ListWorkItemPhases("answer-crash")
	if err != nil {
		t.Fatal(err)
	}
	var persisted PhaseInput
	if len(phases) != 1 || phases[0].Status != "parked" || json.Unmarshal(phases[0].InputEnvelope, &persisted) != nil ||
		persisted.Feedback == nil || persisted.Feedback.Note != "persisted answer" {
		t.Fatalf("crash-swept answer phase = %+v, input=%+v", phases, persisted)
	}
}

// TestStartupRebuildRestoresPersistedPause pins that the kill switch survives
// a restart: the engine comes up paused, publishes that state, and holds every
// start until it is cleared.
func TestStartupRebuildRestoresPersistedPause(t *testing.T) {
	workflow := onePhaseWorkflow("paused", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Paused: true}, map[string]def.Workflow{"paused": workflow}, []string{"project"}, nil)
	if paused, err := h.engine.Paused(); err != nil || !paused {
		t.Fatalf("restored paused = %v err=%v, want true", paused, err)
	}
	if got := h.emitter.engineStateEvents(); len(got) != 1 || !got[0].Paused {
		t.Fatalf("rebuild engine state events = %+v, want one paused event", got)
	}
	if err := h.engine.StartItem(testItem("held", "project", "paused", 0)); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "held", StateRunning, "")
	if got := len(h.runner.started()); got != 0 {
		t.Fatalf("restored pause started %d runners", got)
	}
}

// TestStartupRebuildParksRunningRowWithNoSnapshot covers the row shape a
// pre-rev-2 database produces after the direct-start migration turns an unstarted run
// into a parked one and any stale running row keeps no frozen workflow: it is
// unrunnable, so it parks with a typed reason instead of being resumed blind.
func TestStartupRebuildParksRunningRowWithNoSnapshot(t *testing.T) {
	workflow := onePhaseWorkflow("stale", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{}, map[string]def.Workflow{"stale": workflow}, []string{"project"}, func(database *store.Store) {
		if err := database.CreateWorkItem(testItem("snapshotless", "project", "stale", 0)); err != nil {
			t.Fatal(err)
		}
	})
	requireItemState(t, h.store, "snapshotless", StateNeedsHuman, ReasonWiringError)
	if got := len(h.runner.started()); got != 0 {
		t.Fatalf("snapshotless row started: %+v", h.runner.started())
	}
	if got := h.emitter.errorEvents("snapshotless"); len(got) == 0 {
		t.Fatal("snapshotless rebuild failure did not emit workflow:error")
	}
}

func TestStartupRebuildCancelsItemsWhoseProjectWasDeleted(t *testing.T) {
	h := newHarness(t, Config{}, nil, []string{"project"}, func(database *store.Store) {
		orphan := testItem("orphan", "deleted-project", "missing", 0)
		if err := database.CreateWorkItem(orphan); err != nil {
			t.Fatal(err)
		}
	})
	requireItemState(t, h.store, "orphan", StateCancelled, ReasonInterrupted)
	if starts := h.runner.started(); len(starts) != 0 {
		t.Fatalf("orphan item started: %+v", starts)
	}
	events := h.emitter.stateEvents("orphan")
	if len(events) != 1 || events[0].From != StateRunning || events[0].To != StateCancelled || events[0].Reason != ReasonInterrupted {
		t.Fatalf("orphan cancellation events = %+v", events)
	}
	// Cancelled is terminal: a second cancel is refused by naming the state the
	// item is already in rather than by pretending it is parked.
	if err := h.engine.Cancel("orphan"); err == nil || !strings.Contains(err.Error(), "state is cancelled") {
		t.Fatalf("orphan accepted a second cancel: %v", err)
	}
}

func TestStartupIgnoresParkedSetupFailureUntilResume(t *testing.T) {
	workflow := onePhaseWorkflow("retry-setup", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{}, map[string]def.Workflow{"retry-setup": workflow}, []string{"project"}, func(database *store.Store) {
		item := testItem("setup-failed", "project", "retry-setup", 0)
		item.State = string(StateNeedsHuman)
		item.Reason = string(ReasonSetupFailed)
		if err := database.CreateWorkItem(item); err != nil {
			t.Fatal(err)
		}
	})
	if err := h.engine.Resume("setup-failed", "", false); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 1 || starts[0].Key.ItemID != "setup-failed" {
		t.Fatalf("setup retry starts = %+v", starts)
	}
	requireItemState(t, h.store, "setup-failed", StateRunning, "")
}

func TestStartupRebuildKeepsTakenOverItemParkedAndCompletable(t *testing.T) {
	workflow := onePhaseWorkflow("taken-over", nil, []def.Route{{To: "done"}})
	snapshot, err := json.Marshal(Snapshot{Workflow: workflow})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Config{}, map[string]def.Workflow{"taken-over": workflow}, []string{"project"}, func(database *store.Store) {
		item := testItem("taken-over", "project", "taken-over", 0)
		item.Snapshot = snapshot
		item.State = string(StateNeedsHuman)
		item.Reason = string(ReasonTakenOver)
		item.StartedAt = 20
		item.EndedAt = 22
		if err := database.CreateWorkItem(item); err != nil {
			t.Fatal(err)
		}
		seedThread(t, database, "takeover-thread")
		if err := database.CreateWorkItemPhase(store.WorkItemPhase{
			ItemID: item.ID, PhaseID: "work", Attempt: 1, ThreadID: "takeover-thread",
			InputEnvelope: json.RawMessage(`{"vars":{}}`), Status: "parked", StartedAt: 21, EndedAt: 22,
		}); err != nil {
			t.Fatal(err)
		}
	})
	requireItemState(t, h.store, "taken-over", StateNeedsHuman, ReasonTakenOver)
	if len(h.runner.started()) != 0 || h.runner.stopCount() != 0 {
		t.Fatalf("startup touched taken-over item: starts=%+v stops=%d", h.runner.started(), h.runner.stopCount())
	}
	if err := h.engine.CompleteTakeover("taken-over"); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 1 || !starts[0].Launch.FinalizesTakeover() || starts[0].Launch.ThreadID() != "takeover-thread" {
		t.Fatalf("rebuilt takeover finalize = %+v", starts)
	}
}

func TestStartupCompletesPersistedTeardownWindow(t *testing.T) {
	workflow := onePhaseWorkflow("window", nil, []def.Route{{Human: &def.HumanRoute{Approve: "done"}}})
	tests := []struct {
		name        string
		phaseStatus string
		trace       def.GateTrace
		wantState   State
		wantReason  Reason
	}{
		{"cancelled", "cancelled", def.GateTrace{}, StateCancelled, ReasonInterrupted},
		{"failed", "failed", def.GateTrace{}, StateFailed, ReasonCheckFailedGenuine},
		{"human gate", "parked", def.GateTrace{Decision: def.RouteDecision{Kind: def.DecisionHuman, RouteIndex: 0}}, StateNeedsHuman, ReasonGate},
		{"wiring error", "parked", def.GateTrace{Decision: def.RouteDecision{Kind: def.DecisionNoMatch, RouteIndex: -1}}, StateNeedsHuman, ReasonWiringError},
		{"loop limit exhausted", "parked", def.GateTrace{Decision: def.RouteDecision{Kind: def.DecisionRetriesExhausted, RouteIndex: -1}}, StateNeedsHuman, ReasonLoopLimitExhausted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, err := json.Marshal(Snapshot{Workflow: workflow})
			if err != nil {
				t.Fatal(err)
			}
			trace, err := json.Marshal(tc.trace)
			if err != nil {
				t.Fatal(err)
			}
			h := newHarness(t, Config{}, map[string]def.Workflow{"window": workflow}, []string{"project"}, func(database *store.Store) {
				item := testItem("window", "project", "window", 0)
				if err := database.CreateWorkItem(item); err != nil {
					t.Fatal(err)
				}
				if err := database.UpdateWorkItemRunStart(item.ID, snapshot, "", "", "", 20); err != nil {
					t.Fatal(err)
				}
				if err := database.UpdateWorkItemState(item.ID, string(StateRunning), "", 0); err != nil {
					t.Fatal(err)
				}
				if err := database.CreateWorkItemPhase(store.WorkItemPhase{
					ItemID: item.ID, PhaseID: "work", Attempt: 1, InputEnvelope: json.RawMessage(`{}`),
					OutputEnvelope: doneEnvelope(true), GateTrace: trace,
					Status: tc.phaseStatus, StartedAt: 21, EndedAt: 22,
				}); err != nil {
					t.Fatal(err)
				}
			})
			requireItemState(t, h.store, "window", tc.wantState, tc.wantReason)
			if h.runner.stopCount() != 0 {
				t.Fatalf("terminal teardown window called Stop %d times", h.runner.stopCount())
			}
		})
	}
}

func TestStartupContinuesPersistedHumanRejectDecision(t *testing.T) {
	workflow := humanWorkflow()
	snapshot, err := json.Marshal(Snapshot{Workflow: workflow})
	if err != nil {
		t.Fatal(err)
	}
	trace, err := json.Marshal(def.GateTrace{Decision: def.RouteDecision{
		Kind: def.DecisionLoop, RouteIndex: 0, Target: "build",
		LoopEdge: def.GateEdgeKey("review", 0), Max: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	intervention, err := json.Marshal(HumanIntervention{Decision: HumanReject, Note: "persisted note"})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Config{}, map[string]def.Workflow{"human": workflow}, []string{"project"}, func(database *store.Store) {
		item := testItem("human-window", "project", "human", 0)
		item.State = string(StateNeedsHuman)
		item.Reason = string(ReasonGate)
		item.Snapshot = snapshot
		item.StartedAt = 20
		if err := database.CreateWorkItem(item); err != nil {
			t.Fatal(err)
		}
		if err := database.CreateWorkItemPhase(store.WorkItemPhase{
			ItemID: item.ID, PhaseID: "review", Attempt: 1,
			InputEnvelope: json.RawMessage(`{"vars":{}}`), OutputEnvelope: doneEnvelope(true),
			GateTrace: trace, Intervention: intervention,
			Status: "completed", StartedAt: 21, EndedAt: 22,
		}); err != nil {
			t.Fatal(err)
		}
	})
	starts := h.runner.started()
	if len(starts) != 1 || starts[0].Key.PhaseID != "build" || starts[0].Feedback == nil || starts[0].Feedback.Note != "persisted note" {
		t.Fatalf("recovered human decision starts = %+v", starts)
	}
	requireItemState(t, h.store, "human-window", StateRunning, "")
}

func TestPendingPersistedHumanDecisionsShareProviderCapacity(t *testing.T) {
	workflow := humanWorkflow()
	snapshot, err := json.Marshal(Snapshot{Workflow: workflow})
	if err != nil {
		t.Fatal(err)
	}
	trace, err := json.Marshal(def.GateTrace{Decision: def.RouteDecision{
		Kind: def.DecisionLoop, RouteIndex: 0, Target: "build",
		LoopEdge: def.GateEdgeKey("review", 0), Max: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	intervention := json.RawMessage(`{"decision":"reject","note":"retry"}`)
	h := newHarnessWith(t, harnessOptions{
		workflows:  map[string]def.Workflow{"human": workflow},
		projectIDs: []string{"project"},
		capacities: map[string]map[string]int{"project": {ProviderResource(testProvider): 1}},
		beforeStart: func(database *store.Store) {
			for position, id := range []string{"first", "second"} {
				item := testItem(id, "project", "human", position)
				item.State, item.Reason, item.Snapshot = string(StateNeedsHuman), string(ReasonGate), snapshot
				if err := database.CreateWorkItemPhase(store.WorkItemPhase{
					ItemID: id, PhaseID: "review", Attempt: 1,
					InputEnvelope: json.RawMessage(`{"vars":{}}`), OutputEnvelope: doneEnvelope(true),
					GateTrace: trace, Intervention: intervention,
					Status: "completed", StartedAt: int64(20 + position), EndedAt: int64(30 + position),
				}); err != nil {
					t.Fatal(err)
				}
				if err := database.CreateWorkItem(item); err != nil {
					t.Fatal(err)
				}
			}
		},
	})
	starts := h.runner.started()
	if len(starts) != 1 || starts[0].Key.ItemID != "first" {
		t.Fatalf("initial recovered starts = %+v, want only the first within provider capacity", starts)
	}
	// The second recovered decision is running with its phase held, not parked.
	requireItemState(t, h.store, "second", StateRunning, "")
	h.runner.complete(t, "first", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	starts = h.runner.started()
	if len(starts) != 2 || starts[1].Key.ItemID != "second" || starts[1].Key.PhaseID != "build" {
		t.Fatalf("freed capacity did not release the held decision: %+v", starts)
	}
}

func TestMalformedRecoveredDecisionDoesNotBlockOtherItems(t *testing.T) {
	workflow := onePhaseWorkflow("recover", nil, []def.Route{{To: "done"}})
	snapshot, err := json.Marshal(Snapshot{Workflow: workflow})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Config{}, map[string]def.Workflow{"recover": workflow}, []string{"project"}, func(database *store.Store) {
		broken := testItem("broken", "project", "recover", 0)
		if err := database.CreateWorkItem(broken); err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateWorkItemRunStart(broken.ID, snapshot, "", "", "", 20); err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateWorkItemState(broken.ID, string(StateRunning), "", 0); err != nil {
			t.Fatal(err)
		}
		if err := database.CreateWorkItemPhase(store.WorkItemPhase{
			ItemID: broken.ID, PhaseID: "work", Attempt: 1,
			InputEnvelope: json.RawMessage(`{"vars":{}}`), OutputEnvelope: doneEnvelope(true),
			GateTrace: json.RawMessage(`{"predicates":[],"decision":{"kind":"bogus","routeIndex":0}}`),
			Status:    "completed", StartedAt: 21, EndedAt: 22,
		}); err != nil {
			t.Fatal(err)
		}
		healthy := testItem("healthy", "project", "recover", 1)
		healthy.State = string(StateNeedsHuman)
		healthy.Reason = string(ReasonSetupFailed)
		if err := database.CreateWorkItem(healthy); err != nil {
			t.Fatal(err)
		}
	})
	requireItemState(t, h.store, "broken", StateNeedsHuman, ReasonWiringError)
	if starts := h.runner.started(); len(starts) != 0 {
		t.Fatalf("rebuild started a phase: %+v", starts)
	}
	// The unrelated item is untouched and still resumable after the broken one
	// failed to recover.
	if err := h.engine.Resume("healthy", "", false); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 1 || starts[0].Key.ItemID != "healthy" {
		t.Fatalf("unrelated item did not continue: %+v", starts)
	}
}
