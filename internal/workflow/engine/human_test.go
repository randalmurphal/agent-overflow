package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/workflow/def"
)

func humanWorkflow() def.Workflow {
	return def.Workflow{ID: "human", Phases: []def.Phase{
		agentPhase("build", nil, []def.Route{{To: "review"}}),
		agentPhase("review", nil, []def.Route{{Human: &def.HumanRoute{
			Approve: "done",
			Reject:  &def.LoopTarget{Loop: "build", Max: def.LiteralBound(1), Feedback: []string{"review.ok"}},
		}}}),
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

// A park: route rests under the same `gate` reason a human: route does, but it
// declares no approve or reject — there is nothing a decision could select. The
// refusal has to say that and name the repair, because "persisted decision is
// park without a human intervention" reads as a corrupt record rather than as
// the construct working; and the repair it names must actually work.
func TestParkRouteRefusesResolveAndNamesTheRepair(t *testing.T) {
	workflow := def.Workflow{ID: "parking", Phases: []def.Phase{
		agentPhase("review", nil, []def.Route{{Park: "review-unresolved"}}),
	}}
	h := newHarness(t, Config{}, map[string]def.Workflow{"parking": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "parking", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonGate)
	err := h.engine.ResolveHumanGate(item.ID, HumanApprove, "")
	if err == nil {
		t.Fatal("a park: route accepted an approve decision it never declared")
	}
	for _, want := range []string{`park: route ("review-unresolved")`, "resume the run", "human: route"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("park refusal %q does not carry %q", err, want)
		}
	}
	if err := h.engine.Resume(item.ID, "", false); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateRunning, "")
}

func TestHumanGateApprovalRoutesToDone(t *testing.T) {
	workflow := humanWorkflow()
	h := newHarness(t, Config{}, map[string]def.Workflow{"human": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "human", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	parkAtHumanGate(t, h, item.ID)
	// The gate's own decision belongs to ResolveHumanGate: re-entering the phase
	// that is waiting on one is not a way to take it.
	if err := h.engine.Resume(item.ID, "", false); err == nil {
		t.Fatal("generic resume bypassed human gate decision")
	}
	if err := h.engine.ResolveHumanGate(item.ID, HumanApprove, ""); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateDone, "")
}

func TestHumanGateRejectPersistsFeedbackAndEnforcesBound(t *testing.T) {
	workflow := humanWorkflow()
	h := newHarness(t, Config{}, map[string]def.Workflow{"human": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "human", 0)
	if err := h.engine.StartItem(item); err != nil {
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
	if err := h.engine.ResolveHumanGate(item.ID, HumanReject, "again"); err == nil {
		t.Fatal("a reject past its bound was taken")
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonGate)
	if got := len(h.runner.started()); got != 4 {
		t.Fatalf("exhausted human reject started another phase: %d starts", got)
	}
}

// A reject the route has no budget left for is refused, not converted. The gate
// offered two verbs; parking the run `retries-exhausted` on the spent one would
// take the other away too, so approve has to still be there afterwards — and
// the refusal has to name every option that is.
func TestHumanRejectPastItsBoundIsRefusedAndLeavesTheGateDecidable(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{"human": humanWorkflow()}, []string{"project"}, nil)
	item := testItem("item", "project", "human", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	parkAtHumanGate(t, h, item.ID)
	if err := h.engine.ResolveHumanGate(item.ID, HumanReject, "once"); err != nil {
		t.Fatal(err)
	}
	parkAtHumanGate(t, h, item.ID)

	err := h.engine.ResolveHumanGate(item.ID, HumanReject, "twice")
	if err == nil {
		t.Fatal("a reject past its bound was taken")
	}
	for _, want := range []string{"approve", "run resume --phase build", "cancel"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("over-budget reject refusal %q does not name %q", err, want)
		}
	}
	// The run rests exactly where it was, on the trace the gate wrote: nothing
	// about the refused decision was persisted.
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonGate)
	phases, err := h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	parked := phases[len(phases)-1]
	if parked.Status != "parked" || len(parked.Intervention) != 0 {
		t.Fatalf("refused reject changed the parked attempt: %+v", parked)
	}
	var trace def.GateTrace
	if err := json.Unmarshal(parked.GateTrace, &trace); err != nil {
		t.Fatal(err)
	}
	if trace.Decision.Kind != def.DecisionHuman {
		t.Fatalf("gate trace decision = %+v, want the gate still undecided", trace.Decision)
	}

	// The approve the gate declared is still there and still completes.
	if err := h.engine.ResolveHumanGate(item.ID, HumanApprove, ""); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateDone, "")
}

// The other option the refusal names: a fresh entry into the loop's target
// refills the budget, so the reject becomes available again.
func TestResumeToTheRejectTargetRefillsTheRejectBudget(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{"human": humanWorkflow()}, []string{"project"}, nil)
	item := testItem("item", "project", "human", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	parkAtHumanGate(t, h, item.ID)
	if err := h.engine.ResolveHumanGate(item.ID, HumanReject, "once"); err != nil {
		t.Fatal(err)
	}
	parkAtHumanGate(t, h, item.ID)
	if err := h.engine.ResolveHumanGate(item.ID, HumanReject, "twice"); err == nil {
		t.Fatal("a reject past its bound was taken")
	}

	if err := h.engine.Resume(item.ID, "build", false); err != nil {
		t.Fatal(err)
	}
	parkAtHumanGate(t, h, item.ID)
	if err := h.engine.ResolveHumanGate(item.ID, HumanReject, "after a fresh entry"); err != nil {
		t.Fatalf("a refilled reject budget was still refused: %v", err)
	}
	requireItemState(t, h.store, item.ID, StateRunning, "")
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
	h := newHarness(t, Config{}, map[string]def.Workflow{"takeover": workflow}, []string{"project"}, nil)
	h.profiles.setCapacity("project", "writer", 1)
	item := testItem("item", "project", "takeover", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	seedThread(t, h.store, "thread-one")
	if err := h.store.AttachWorkItemPhaseRun(item.ID, "work", 1, "thread-one", "/tmp/narrative.md"); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.TakeOver(item.ID); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonTakenOver)
	// A takeover detaches the attempt through StopForTakeover; the ordinary stop
	// path must not also run, which would kill the session the human now steers.
	if takeovers := h.runner.takeovers(); len(takeovers) != 1 || takeovers[0].ItemID != item.ID {
		t.Fatalf("takeover stops = %+v, want the live attempt detached once", takeovers)
	}
	if h.runner.stopCount() != 0 {
		t.Fatalf("runner stops = %d, want none for a takeover", h.runner.stopCount())
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
	h := newHarness(t, Config{}, map[string]def.Workflow{"takeover-failure": workflow}, []string{"project"}, nil)
	h.profiles.setCapacity("project", "writer", 1)
	item := testItem("item", "project", "takeover-failure", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("intervention write failed")
	h.engine.store = failingInterventionPersistence{persistence: h.store, err: wantErr}
	if err := h.engine.TakeOver(item.ID); !errors.Is(err, wantErr) {
		t.Fatalf("takeover error = %v, want %v", err, wantErr)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonTakenOver)
	if len(h.engine.holders) != 0 {
		t.Fatalf("takeover failure retained resources: holders=%v", h.engine.holders)
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
	h := newHarness(t, Config{}, map[string]def.Workflow{"takeover": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "takeover", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	seedThread(t, h.store, "thread-one")
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
	if err := h.engine.Resume(item.ID, "", false); err != nil {
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
	h := newHarness(t, Config{}, map[string]def.Workflow{"question": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "question", 0)
	if err := h.engine.StartItem(item); err != nil {
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

// seededRejectWorkflow reads its reject budget from a run input rather than
// from the definition, which is what lets one campaign spend two rejections
// where another spends five without a second workflow.
func seededRejectWorkflow() def.Workflow {
	workflow := humanWorkflow()
	workflow.Phases[1].Gate.Routes[0].Human.Reject.Max = def.RefBound("fix-budget")
	return workflow
}

func TestHumanRejectResolvesASeededBoundAndRecordsIt(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{"human": seededRejectWorkflow()}, []string{"project"}, nil)
	item := testItem("item", "project", "human", 0)
	item.Seeds = json.RawMessage(`{"fix-budget":1}`)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	parkAtHumanGate(t, h, item.ID)
	if err := h.engine.ResolveHumanGate(item.ID, HumanReject, "seeded"); err != nil {
		t.Fatal(err)
	}
	phases, err := h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	var trace def.GateTrace
	if err := json.Unmarshal(phases[1].GateTrace, &trace); err != nil {
		t.Fatal(err)
	}
	if trace.Decision.Kind != def.DecisionLoop || trace.Decision.Max != 1 {
		t.Fatalf("persisted reject decision = %+v, want a loop bounded at the seeded 1", trace.Decision)
	}

	parkAtHumanGate(t, h, item.ID)
	if err := h.engine.ResolveHumanGate(item.ID, HumanReject, "again"); err == nil {
		t.Fatal("a reject past its seeded bound was taken")
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonGate)
}

// A bound that will not resolve is not an exhausted budget and not an implicit
// one: the human's action fails loudly and the run stays parked exactly where
// it was, still answerable the other way.
func TestHumanRejectRefusesAnUnresolvableSeededBound(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{"human": seededRejectWorkflow()}, []string{"project"}, nil)
	item := testItem("item", "project", "human", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	parkAtHumanGate(t, h, item.ID)
	if err := h.engine.ResolveHumanGate(item.ID, HumanReject, ""); err == nil {
		t.Fatal("an unresolvable reject bound was spent silently")
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonGate)
	if got := len(h.runner.started()); got != 2 {
		t.Fatalf("refused reject started another phase: %d starts", got)
	}
	if err := h.engine.ResolveHumanGate(item.ID, HumanApprove, ""); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateDone, "")
}
