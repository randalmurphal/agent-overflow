package engine

import (
	"encoding/json"
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
	h := newHarness(t, Config{Active: false, GlobalConcurrency: 1}, map[string]def.Workflow{"crash": workflow}, []string{"project"}, func(database *store.Store) {
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
	if err := h.engine.SetQueue(true, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Enqueue(testItem("after-crash", "project", "crash", 1)); err != nil {
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

func TestStartupRebuildDrainsPersistedQueuedItems(t *testing.T) {
	workflow := onePhaseWorkflow("queued", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{"queued": workflow}, []string{"project"}, func(database *store.Store) {
		if err := database.CreateWorkItem(testItem("persisted", "project", "queued", 0)); err != nil {
			t.Fatal(err)
		}
	})
	starts := h.runner.started()
	if len(starts) != 1 || starts[0].Key.ItemID != "persisted" {
		t.Fatalf("rebuilt queue starts = %+v", starts)
	}
	requireItemState(t, h.store, "persisted", StateRunning, "")
}

func TestStartupIgnoresParkedSetupFailureUntilResume(t *testing.T) {
	workflow := onePhaseWorkflow("retry-setup", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: false, GlobalConcurrency: 1}, map[string]def.Workflow{"retry-setup": workflow}, []string{"project"}, func(database *store.Store) {
		item := testItem("setup-failed", "project", "retry-setup", 0)
		item.State = string(StateNeedsHuman)
		item.Reason = string(ReasonSetupFailed)
		if err := database.CreateWorkItem(item); err != nil {
			t.Fatal(err)
		}
	})
	if err := h.engine.Resume("setup-failed", ""); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 1 || starts[0].Key.ItemID != "setup-failed" {
		t.Fatalf("setup retry starts = %+v", starts)
	}
	requireItemState(t, h.store, "setup-failed", StateRunning, "")
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
		{"retries exhausted", "parked", def.GateTrace{Decision: def.RouteDecision{Kind: def.DecisionRetriesExhausted, RouteIndex: -1}}, StateNeedsHuman, ReasonRetriesExhausted},
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
			h := newHarness(t, Config{Active: false, GlobalConcurrency: 1}, map[string]def.Workflow{"window": workflow}, []string{"project"}, func(database *store.Store) {
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
	h := newHarness(t, Config{Active: false, GlobalConcurrency: 1}, map[string]def.Workflow{"human": workflow}, []string{"project"}, func(database *store.Store) {
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

func TestPendingPersistedHumanDecisionRetriesWhenSlotFrees(t *testing.T) {
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
	h := newHarness(t, Config{Active: false, GlobalConcurrency: 1}, map[string]def.Workflow{"human": workflow}, []string{"project"}, func(database *store.Store) {
		for position, id := range []string{"first", "second"} {
			item := testItem(id, "project", "human", position)
			item.State, item.Reason, item.Snapshot = string(StateNeedsHuman), string(ReasonGate), snapshot
			if err := database.CreateWorkItem(item); err != nil {
				t.Fatal(err)
			}
			if err := database.CreateWorkItemPhase(store.WorkItemPhase{
				ItemID: id, PhaseID: "review", Attempt: 1,
				InputEnvelope: json.RawMessage(`{"vars":{}}`), OutputEnvelope: doneEnvelope(true),
				GateTrace: trace, Intervention: intervention,
				Status: "completed", StartedAt: int64(20 + position), EndedAt: int64(30 + position),
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
	starts := h.runner.started()
	if len(starts) != 1 || starts[0].Key.ItemID != "first" {
		t.Fatalf("initial recovered starts = %+v", starts)
	}
	h.runner.complete(t, "first", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, "first", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	starts = h.runner.started()
	if len(starts) != 3 || starts[2].Key.ItemID != "second" || starts[2].Key.PhaseID != "build" {
		t.Fatalf("pending human decision did not resume: %+v", starts)
	}
}

func TestMalformedRecoveredDecisionDoesNotBlockOtherItems(t *testing.T) {
	workflow := onePhaseWorkflow("recover", nil, []def.Route{{To: "done"}})
	snapshot, err := json.Marshal(Snapshot{Workflow: workflow})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{"recover": workflow}, []string{"project"}, func(database *store.Store) {
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
		if err := database.CreateWorkItem(testItem("queued", "project", "recover", 1)); err != nil {
			t.Fatal(err)
		}
	})
	requireItemState(t, h.store, "broken", StateNeedsHuman, ReasonWiringError)
	starts := h.runner.started()
	if len(starts) != 1 || starts[0].Key.ItemID != "queued" {
		t.Fatalf("unrelated queue did not continue: %+v", starts)
	}
}
