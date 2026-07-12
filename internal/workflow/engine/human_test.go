package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"agent-overflow/internal/workflow/def"
)

func humanWorkflow() def.Workflow {
	output := map[string]def.Variable{"ok": {Schema: def.JSONSchema{Type: "boolean"}}}
	return def.Workflow{ID: "human", Phases: []def.Phase{
		{ID: "build", Driver: def.DriverAgent, Outputs: output, Gate: def.Gate{Routes: []def.Route{{To: "review"}}}},
		{ID: "review", Driver: def.DriverAgent, Outputs: output, Gate: def.Gate{Routes: []def.Route{{Human: &def.HumanRoute{
			Approve: "done",
			Reject:  &def.LoopTarget{Loop: "build", Max: 1, Feedback: []string{"review.ok"}},
		}}}}},
	}}
}

func parkAtHumanGate(t *testing.T, h *testHarness, itemID string) {
	t.Helper()
	for step := 0; step < 2; step++ {
		h.runner.complete(t, itemID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	requireItemState(t, h.store, itemID, StateNeedsHuman, ReasonGate)
}

func TestHumanGateApprovalRoutesToDone(t *testing.T) {
	workflow := humanWorkflow()
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{"human": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "human", 0)
	if err := h.engine.Enqueue(item); err != nil {
		t.Fatal(err)
	}
	parkAtHumanGate(t, h, item.ID)
	if err := h.engine.Resume(item.ID, "done"); err == nil {
		t.Fatal("generic resume bypassed human gate decision")
	}
	if err := h.engine.ResolveHumanGate(item.ID, HumanApprove, ""); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateDone, "")
}

func TestHumanGateRejectPersistsFeedbackAndEnforcesBound(t *testing.T) {
	workflow := humanWorkflow()
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{"human": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "human", 0)
	if err := h.engine.Enqueue(item); err != nil {
		t.Fatal(err)
	}
	parkAtHumanGate(t, h, item.ID)
	if err := h.engine.ResolveHumanGate(item.ID, HumanReject, "fix the edge case"); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 3 || starts[2].Key.PhaseID != "build" {
		t.Fatalf("reject route starts = %+v", starts)
	}
	feedback := starts[2].Feedback
	if feedback == nil || feedback.Note != "fix the edge case" || feedback.Values["review.ok"] != true {
		t.Fatalf("reject feedback = %+v", feedback)
	}
	phases, err := h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	var intervention HumanIntervention
	if err := json.Unmarshal(phases[1].Intervention, &intervention); err != nil {
		t.Fatal(err)
	}
	if intervention.Decision != HumanReject || intervention.Note != "fix the edge case" {
		t.Fatalf("intervention = %+v", intervention)
	}

	parkAtHumanGate(t, h, item.ID)
	if err := h.engine.ResolveHumanGate(item.ID, HumanReject, "again"); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonRetriesExhausted)
	if got := len(h.runner.started()); got != 4 {
		t.Fatalf("exhausted human reject started another phase: %d starts", got)
	}
}

func TestFeedbackForResolvesNestedValues(t *testing.T) {
	feedback := feedbackFor(map[string]any{
		"review.result": map[string]any{"detail": map[string]any{"ok": true}},
	}, []string{"review.result.detail.ok"}, "")
	if feedback == nil || feedback.Values["review.result.detail.ok"] != true {
		t.Fatalf("nested feedback = %+v", feedback)
	}
}

func TestPersistedGateTraceDecodePreservesLargeInteger(t *testing.T) {
	payload := json.RawMessage(`{"predicates":[{"routeIndex":0,"path":"routes[0].when","operator":"eq","ref":"value","value":9007199254740993,"result":true}],"decision":{"kind":"human","routeIndex":0}}`)
	var trace def.GateTrace
	if err := decodeJSON(payload, &trace); err != nil {
		t.Fatal(err)
	}
	reencoded, err := json.Marshal(trace)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(reencoded, []byte("9007199254740993")) {
		t.Fatalf("large integer changed during trace round trip: %s", reencoded)
	}
}

func TestTakeoverDetachesAndCompleteRoutesThroughGate(t *testing.T) {
	workflow := onePhaseWorkflow("takeover", []string{"writer"}, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{"takeover": workflow}, []string{"project"}, nil)
	h.profiles.setCapacity("project", "writer", 1)
	item := testItem("item", "project", "takeover", 0)
	if err := h.engine.Enqueue(item); err != nil {
		t.Fatal(err)
	}
	if err := h.store.AttachWorkItemPhaseRun(item.ID, "work", 1, "thread-one", "/tmp/narrative.md"); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.TakeOver(item.ID); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonTakenOver)
	if h.runner.stopCount() != 1 {
		t.Fatalf("runner stops = %d, want 1", h.runner.stopCount())
	}
	phases, err := h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if phases[0].Status != "parked" {
		t.Fatalf("phase status = %q, want parked", phases[0].Status)
	}
	var intervention TakeoverIntervention
	if err := json.Unmarshal(phases[0].Intervention, &intervention); err != nil {
		t.Fatal(err)
	}
	if intervention.Kind != "taken-over" || intervention.At == 0 {
		t.Fatalf("intervention = %+v", intervention)
	}

	if err := h.engine.CompleteTakeover(item.ID); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || !starts[1].FinalizeTakeover || starts[1].PriorThreadID != "thread-one" {
		t.Fatalf("finalize start = %+v", starts)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateDone, "")
}

type failingInterventionPersistence struct {
	persistence
	err error
}

func (f failingInterventionPersistence) UpdateWorkItemPhaseIntervention(string, string, int, json.RawMessage) error {
	return f.err
}

func TestTakeoverInterventionFailureStillParksAndReleases(t *testing.T) {
	workflow := onePhaseWorkflow("takeover-failure", []string{"writer"}, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{"takeover-failure": workflow}, []string{"project"}, nil)
	h.profiles.setCapacity("project", "writer", 1)
	item := testItem("item", "project", "takeover-failure", 0)
	if err := h.engine.Enqueue(item); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("intervention write failed")
	h.engine.store = failingInterventionPersistence{persistence: h.store, err: wantErr}
	if err := h.engine.TakeOver(item.ID); !errors.Is(err, wantErr) {
		t.Fatalf("takeover error = %v, want %v", err, wantErr)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonTakenOver)
	if h.engine.activeSlots != 0 || len(h.engine.holders) != 0 {
		t.Fatalf("takeover failure retained resources: slots=%d holders=%v", h.engine.activeSlots, h.engine.holders)
	}
	phases, err := h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0].Status != "parked" {
		t.Fatalf("phase after intervention failure = %+v", phases)
	}
}

func TestTakeoverFinalizeValidationFailureReparksAndResumeIsFresh(t *testing.T) {
	workflow := onePhaseWorkflow("takeover", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{"takeover": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "takeover", 0)
	if err := h.engine.Enqueue(item); err != nil {
		t.Fatal(err)
	}
	if err := h.store.AttachWorkItemPhaseRun(item.ID, "work", 1, "thread-one", "/tmp/narrative.md"); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.TakeOver(item.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.CompleteTakeover(item.ID); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeExecutionFailure})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonTakenOver)
	if err := h.engine.Resume(item.ID, ""); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	last := starts[len(starts)-1]
	if last.FinalizeTakeover || last.PriorThreadID != "" {
		t.Fatalf("fresh resume start = %+v", last)
	}
}

func TestTakeoverOfParkedQuestionLeavesAnswerPathUntouchedUntilTakeover(t *testing.T) {
	workflow := onePhaseWorkflow("question", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{"question": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "question", 0)
	if err := h.engine.Enqueue(item); err != nil {
		t.Fatal(err)
	}
	if err := h.store.AttachWorkItemPhaseRun(item.ID, "work", 1, "thread-one", "/tmp/narrative.md"); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.TakeOver(item.ID); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonTakenOver)
	if err := h.engine.Answer(item.ID, "too late"); err == nil {
		t.Fatal("question answer succeeded after explicit takeover")
	}
}
