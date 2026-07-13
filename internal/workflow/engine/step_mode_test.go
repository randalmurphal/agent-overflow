package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

func stepWorkflow() def.Workflow {
	outputs := map[string]def.Variable{"ok": {Schema: def.JSONSchema{Type: "boolean"}}}
	return def.Workflow{ID: "step", Phases: []def.Phase{
		{ID: "first", Driver: def.DriverAgent, Outputs: outputs, Gate: def.Gate{Routes: []def.Route{{To: "second"}}}},
		{ID: "second", Driver: def.DriverAgent, Outputs: outputs, Gate: def.Gate{Routes: []def.Route{{To: "done"}}}},
	}}
}

func TestLoopCountsIgnoreGateAttemptAbandonedByTakeover(t *testing.T) {
	edge := def.GateEdgeKey("review", 0)
	trace, err := json.Marshal(def.GateTrace{Decision: def.RouteDecision{
		Kind: def.DecisionLoop, RouteIndex: 0, Target: "build", LoopEdge: edge, Max: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	intervention, err := json.Marshal(TakeoverIntervention{Kind: "taken-over", At: 10})
	if err != nil {
		t.Fatal(err)
	}
	counts, err := loopCounts("item", []store.WorkItemPhaseContext{
		{PhaseID: "review", Attempt: 1, GateTrace: trace, Status: "completed"},
		{PhaseID: "review", Attempt: 2, GateTrace: trace, Intervention: intervention, Status: "parked"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if counts[edge] != 1 {
		t.Fatalf("loop count = %d, want only the completed route", counts[edge])
	}
}

func TestStepModeParksAtEveryAutomaticGateAndApproveContinues(t *testing.T) {
	workflow := stepWorkflow()
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{"step": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "step", 0)
	item.StepMode = true
	if err := h.engine.Enqueue(item); err != nil {
		t.Fatal(err)
	}

	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonGate)
	phases, err := h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	var trace def.GateTrace
	if len(phases) != 1 || phases[0].Status != "parked" || json.Unmarshal(phases[0].GateTrace, &trace) != nil ||
		trace.Decision.Kind != def.DecisionAdvance || trace.Decision.Target != "second" {
		t.Fatalf("first step park = %+v trace=%+v", phases, trace)
	}
	if err := h.engine.ResolveHumanGate(item.ID, HumanReject, ""); err == nil || !strings.Contains(err.Error(), "step gates support approve") {
		t.Fatalf("step reject error = %v", err)
	}
	if err := h.engine.ResolveHumanGate(item.ID, HumanApprove, "reviewed"); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].Key.PhaseID != "second" {
		t.Fatalf("starts after first approval = %+v", starts)
	}

	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonGate)
	if err := h.engine.ResolveHumanGate(item.ID, HumanApprove, ""); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateDone, "")

	phases, err = h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 2 || phases[0].Status != "completed" || phases[1].Status != "completed" || len(phases[0].Intervention) == 0 || len(phases[1].Intervention) == 0 {
		t.Fatalf("approved step phases = %+v", phases)
	}
}

func TestStepModeParkRebuildsAndApprovesRecordedDecision(t *testing.T) {
	workflow := stepWorkflow()
	snapshot, err := json.Marshal(Snapshot{Workflow: workflow})
	if err != nil {
		t.Fatal(err)
	}
	trace, err := json.Marshal(def.GateTrace{Decision: def.RouteDecision{
		Kind: def.DecisionAdvance, RouteIndex: 0, Target: "second",
	}})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Config{Active: false, GlobalConcurrency: 1}, map[string]def.Workflow{"step": workflow}, []string{"project"}, func(database *store.Store) {
		item := testItem("rebuilt-step", "project", "step", 0)
		item.StepMode = true
		item.State, item.Reason, item.Snapshot = string(StateNeedsHuman), string(ReasonGate), snapshot
		item.StartedAt = 20
		if err := database.CreateWorkItem(item); err != nil {
			t.Fatal(err)
		}
		if err := database.CreateWorkItemPhase(store.WorkItemPhase{
			ItemID: item.ID, PhaseID: "first", Attempt: 1,
			InputEnvelope: json.RawMessage(`{"vars":{}}`), OutputEnvelope: doneEnvelope(true),
			GateTrace: trace, Status: "parked", StartedAt: 21, EndedAt: 22,
		}); err != nil {
			t.Fatal(err)
		}
	})
	requireItemState(t, h.store, "rebuilt-step", StateNeedsHuman, ReasonGate)
	if len(h.runner.started()) != 0 {
		t.Fatalf("unapproved rebuilt step started: %+v", h.runner.started())
	}
	if err := h.engine.ResolveHumanGate("rebuilt-step", HumanApprove, ""); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 1 || starts[0].Key.PhaseID != "second" {
		t.Fatalf("rebuilt approval starts = %+v", starts)
	}
}

func TestStepModeApprovedTransitionRebuildPreservesFeedback(t *testing.T) {
	workflow := stepWorkflow()
	workflow.Phases[0].Gate.Routes[0].Feedback = []string{"first.ok"}
	snapshot, err := json.Marshal(Snapshot{Workflow: workflow})
	if err != nil {
		t.Fatal(err)
	}
	trace, err := json.Marshal(def.GateTrace{Decision: def.RouteDecision{
		Kind: def.DecisionAdvance, RouteIndex: 0, Target: "second", Feedback: []string{"first.ok"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	intervention := json.RawMessage(`{"decision":"approve","note":"checked"}`)
	h := newHarness(t, Config{Active: false, GlobalConcurrency: 1}, map[string]def.Workflow{"step": workflow}, []string{"project"}, func(database *store.Store) {
		item := testItem("approved-step", "project", "step", 0)
		item.StepMode, item.State, item.Snapshot, item.StartedAt = true, string(StateRunning), snapshot, 20
		if err := database.CreateWorkItem(item); err != nil {
			t.Fatal(err)
		}
		if err := database.CreateWorkItemPhase(store.WorkItemPhase{
			ItemID: item.ID, PhaseID: "first", Attempt: 1,
			InputEnvelope: json.RawMessage(`{"vars":{}}`), OutputEnvelope: doneEnvelope(true),
			GateTrace: trace, Intervention: intervention, Status: "completed", StartedAt: 21, EndedAt: 22,
		}); err != nil {
			t.Fatal(err)
		}
	})
	starts := h.runner.started()
	if len(starts) != 1 || starts[0].Key.PhaseID != "second" || starts[0].Feedback == nil ||
		starts[0].Feedback.Note != "checked" || starts[0].Feedback.Values["first.ok"] != true {
		t.Fatalf("recovered approved step starts = %+v", starts)
	}
}

func TestRunnerSetupFailureUsesTypedReason(t *testing.T) {
	workflow := onePhaseWorkflow("setup", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: false, GlobalConcurrency: 1}, map[string]def.Workflow{"setup": workflow}, []string{"project"}, nil)
	item := testItem("setup", "project", "setup", 0)
	h.runner.startErrs[item.ID] = errors.Join(ErrSetupFailed, errors.New("hook failed"))
	if err := h.engine.Enqueue(item); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.SetQueue(true, 0, 1); !errors.Is(err, ErrSetupFailed) {
		t.Fatalf("set queue error = %v, want setup failure", err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonSetupFailed)
}
