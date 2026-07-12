package engine

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"agent-overflow/internal/workflow/def"
)

func TestFSMTransitionsPersistBeforeEmitting(t *testing.T) {
	workflow := onePhaseWorkflow("basic", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: false, GlobalConcurrency: 1}, map[string]def.Workflow{"basic": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "basic", 0)
	if err := h.engine.Enqueue(item); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Cancel(item.ID); err == nil {
		t.Fatal("queued -> cancelled must be rejected")
	}
	if err := h.engine.SetQueue(true, 0); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateRunning, "")
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateDone, "")
	want := []StateEvent{
		{ItemID: item.ID, From: StateQueued, To: StateRunning},
		{ItemID: item.ID, From: StateRunning, To: StateDone},
	}
	if got := h.emitter.stateEvents(item.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("state events = %+v, want %+v", got, want)
	}
	wantPhases := []PhaseEvent{
		{ItemID: item.ID, PhaseID: "work", Attempt: 1, Status: "running"},
		{ItemID: item.ID, PhaseID: "work", Attempt: 1, Status: "completed"},
	}
	if got := h.emitter.phaseEvents(item.ID); !reflect.DeepEqual(got, wantPhases) {
		t.Fatalf("phase events = %+v, want %+v", got, wantPhases)
	}
	if err := h.engine.Resume(item.ID, ""); err == nil {
		t.Fatal("done -> running must be rejected")
	}
}

func TestEngineInstanceCannotRestartAfterClose(t *testing.T) {
	workflow := onePhaseWorkflow("basic", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: false, GlobalConcurrency: 1}, map[string]def.Workflow{"basic": workflow}, []string{"project"}, nil)
	if err := h.engine.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Start(context.Background()); err == nil {
		t.Fatal("expected one-shot engine restart rejection")
	}
}

func TestFSMTransitionTableIsClosed(t *testing.T) {
	states := []State{StateQueued, StateRunning, StateNeedsHuman, StateDone, StateFailed, StateCancelled}
	want := map[State]map[State]bool{
		StateQueued:     {StateRunning: true},
		StateRunning:    {StateNeedsHuman: true, StateDone: true, StateFailed: true, StateCancelled: true},
		StateNeedsHuman: {StateRunning: true},
		StateDone:       {},
		StateFailed:     {},
		StateCancelled:  {},
	}
	for _, from := range states {
		for _, to := range states {
			if got := transitionAllowed(from, to); got != want[from][to] {
				t.Errorf("transition %s -> %s allowed = %v, want %v", from, to, got, want[from][to])
			}
		}
	}
}

func TestNeedsHumanResumeCreatesNewAttempt(t *testing.T) {
	workflow := onePhaseWorkflow("question", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{"question": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "question", 0)
	if err := h.engine.Enqueue(item); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonQuestion)
	if err := h.engine.Resume(item.ID, ""); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].Key.Attempt != 2 {
		t.Fatalf("runner starts = %+v, want second attempt", starts)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateDone, "")
}

func TestDrainPriorityCapPauseAndProcessBound(t *testing.T) {
	workflow := onePhaseWorkflow("basic", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: false, GlobalConcurrency: 2}, map[string]def.Workflow{"basic": workflow}, []string{"project"}, nil)
	for _, item := range []struct {
		id       string
		position int
	}{{"last", 2}, {"first", 0}, {"middle", 1}} {
		if err := h.engine.Enqueue(testItem(item.id, "project", "basic", item.position)); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.engine.Reorder("project", []string{"middle", "first", "last"}); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.SetQueue(true, 2); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[0].Key.ItemID != "middle" || starts[1].Key.ItemID != "first" {
		t.Fatalf("starts = %+v, want reordered first two", starts)
	}
	h.runner.complete(t, "middle", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := len(h.runner.started()); got != 2 {
		t.Fatalf("process bound started %d items, want 2", got)
	}
	if err := h.engine.SetQueue(true, 0); err != nil {
		t.Fatal(err)
	}
	starts = h.runner.started()
	if len(starts) != 3 || starts[2].Key.ItemID != "last" {
		t.Fatalf("resumed starts = %+v", starts)
	}
}

func TestGateUsesSeedsAndNamespacedPhaseOutputs(t *testing.T) {
	workflow := def.Workflow{ID: "routing", Inputs: map[string]def.Variable{
		"enabled": {Schema: def.JSONSchema{Type: "boolean"}},
	}, Phases: []def.Phase{
		{ID: "one", Driver: def.DriverAgent, Outputs: map[string]def.Variable{
			"ok": {Schema: def.JSONSchema{Type: "boolean"}},
		}, Gate: def.Gate{Routes: []def.Route{{
			When: &def.Predicate{All: []def.Predicate{
				{Eq: &def.Comparison{Ref: "enabled", Value: true}},
				{Eq: &def.Comparison{Ref: "one.ok", Value: true}},
			}}, To: "two",
		}}}},
		{ID: "two", Driver: def.DriverAgent, Gate: def.Gate{Routes: []def.Route{{To: "done"}}}},
	}}
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{"routing": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "routing", 0)
	item.Seeds = json.RawMessage(`{"enabled":true}`)
	if err := h.engine.Enqueue(item); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].Key.PhaseID != "two" {
		t.Fatalf("routed starts = %+v", starts)
	}
	phases, err := h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 2 || len(phases[0].GateTrace) == 0 {
		t.Fatalf("phase trace not persisted: %+v", phases)
	}
}

func TestGateNoMatchParksWithWiringError(t *testing.T) {
	predicate := def.Predicate{Eq: &def.Comparison{Ref: "work.ok", Value: true}}
	workflow := onePhaseWorkflow("no-match", nil, []def.Route{{When: &predicate, To: "done"}})
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{"no-match": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "no-match", 0)
	if err := h.engine.Enqueue(item); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(false)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonWiringError)
}

func TestExplicitParkCanResumeWithoutHumanDecision(t *testing.T) {
	workflow := onePhaseWorkflow("park", nil, []def.Route{{Park: "manual-review"}})
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{"park": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "park", 0)
	if err := h.engine.Enqueue(item); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonGate)
	if err := h.engine.Resume(item.ID, ""); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].Key.Attempt != 2 {
		t.Fatalf("explicit park resume starts = %+v", starts)
	}
}

func TestGateLoopExhaustionFallsThroughToNextRoute(t *testing.T) {
	workflow := def.Workflow{ID: "fallthrough", Phases: []def.Phase{
		{ID: "build", Driver: def.DriverAgent, Outputs: map[string]def.Variable{"ok": {Schema: def.JSONSchema{Type: "boolean"}}}, Gate: def.Gate{Routes: []def.Route{{To: "review"}}}},
		{ID: "review", Driver: def.DriverAgent, Outputs: map[string]def.Variable{"ok": {Schema: def.JSONSchema{Type: "boolean"}}}, Gate: def.Gate{Routes: []def.Route{
			{Loop: "build", Max: 1},
			{To: "done"},
		}}},
	}}
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{"fallthrough": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "fallthrough", 0)
	if err := h.engine.Enqueue(item); err != nil {
		t.Fatal(err)
	}
	for step := 0; step < 4; step++ {
		h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	requireItemState(t, h.store, item.ID, StateDone, "")
	starts := h.runner.started()
	wantPhases := []string{"build", "review", "build", "review"}
	if len(starts) != len(wantPhases) {
		t.Fatalf("starts = %+v", starts)
	}
	for index, phaseID := range wantPhases {
		if starts[index].Key.PhaseID != phaseID {
			t.Fatalf("start %d phase = %q, want %q", index, starts[index].Key.PhaseID, phaseID)
		}
	}
}

func TestCancellationUsesRunnerPartialEnvelope(t *testing.T) {
	workflow := onePhaseWorkflow("cancel", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{"cancel": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "cancel", 0)
	if err := h.engine.Enqueue(item); err != nil {
		t.Fatal(err)
	}
	request := h.runner.started()[0]
	partial := json.RawMessage(`{"status":"stuck","outputs":null,"question":null,"reason":"cancelled"}`)
	h.runner.partials[runMapKey(request.Key)] = partial
	if err := h.engine.Cancel(item.ID); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateCancelled, ReasonInterrupted)
	phases, err := h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || string(phases[0].OutputEnvelope) != string(partial) || phases[0].Status != "cancelled" {
		t.Fatalf("cancelled phase = %+v", phases)
	}
	if h.runner.stopCount() != 1 {
		t.Fatalf("stop count = %d, want 1", h.runner.stopCount())
	}
}
