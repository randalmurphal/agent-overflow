package engine

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

func TestRerunFailedStartsImmediatelyWithDiagnosisFeedback(t *testing.T) {
	failedWhenFalse := def.Predicate{Eq: &def.Comparison{Ref: "work.ok", Value: false}}
	workflow := onePhaseWorkflow("retry-failed", nil, []def.Route{
		{When: &failedWhenFalse, To: "failed"},
		{To: "done"},
	})
	h := newHarness(t, Config{}, map[string]def.Workflow{"retry-failed": workflow}, []string{"project"}, nil)
	item := testItem("failed", "project", "retry-failed", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: json.RawMessage(`{"status":"done","outputs":{"ok":false,"diagnosis":"tests still fail"},"question":null,"reason":null}`)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	failed, err := h.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != string(StateFailed) {
		t.Fatalf("failed item state = %q", failed.State)
	}
	if err := h.engine.RerunFailed(item.ID, "", false); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].Key.ItemID != item.ID || starts[1].Key.PhaseID != "work" || starts[1].Key.Attempt != 2 {
		t.Fatalf("rerun starts = %+v, want an immediate second attempt", starts)
	}
	if starts[1].Feedback == nil || starts[1].Feedback.Note != "check-failed-genuine: tests still fail" {
		t.Fatalf("rerun feedback = %+v", starts[1].Feedback)
	}
	if starts[1].Feedback.Values["work.diagnosis"] != "tests still fail" {
		t.Fatalf("rerun feedback values = %+v", starts[1].Feedback.Values)
	}
	reloaded, err := h.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State != string(StateRunning) || reloaded.Reason != "" || reloaded.EndedAt != 0 {
		t.Fatalf("rerun item = %+v, want running with no reason", reloaded)
	}
	if reloaded.StartedAt <= failed.StartedAt {
		t.Fatalf("rerun start time = %d, original = %d", reloaded.StartedAt, failed.StartedAt)
	}
	if events := h.emitter.stateEvents(item.ID); len(events) == 0 ||
		events[len(events)-1].From != StateFailed || events[len(events)-1].To != StateRunning {
		t.Fatalf("rerun state events = %+v, want a failed -> running edge", h.emitter.stateEvents(item.ID))
	}
	phases, err := h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	var input PhaseInput
	if len(phases) != 2 || json.Unmarshal(phases[1].InputEnvelope, &input) != nil || input.Feedback == nil || input.Feedback.Note != starts[1].Feedback.Note {
		t.Fatalf("rerun phase input = %+v, phases=%+v", input, phases)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateDone, "")
}

// TestRerunFailedWaitsForProviderCapacity pins that a rerun is a direct start
// subject to the same resource bound as any other phase: it does not jump a
// held phase, and it starts the moment capacity frees.
func TestRerunFailedWaitsForProviderCapacity(t *testing.T) {
	failedWhenFalse := def.Predicate{Eq: &def.Comparison{Ref: "work.ok", Value: false}}
	workflow := onePhaseWorkflow("retry-failed", nil, []def.Route{
		{When: &failedWhenFalse, To: "failed"},
		{To: "done"},
	})
	h := newHarness(t, Config{}, map[string]def.Workflow{"retry-failed": workflow}, []string{"project"}, nil)
	h.limitProviderCapacity("project", 1)
	item := testItem("failed", "project", "retry-failed", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(false)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateFailed, ReasonCheckFailedGenuine)

	holder := testItem("holder", "project", "retry-failed", 1)
	if err := h.engine.StartItem(holder); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.RerunFailed(item.ID, "", false); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateRunning, "")
	if got := len(h.runner.started()); got != 2 {
		t.Fatalf("runner starts = %d, want the rerun held behind provider capacity", got)
	}
	h.runner.complete(t, holder.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 3 || starts[2].Key.ItemID != item.ID || starts[2].Key.Attempt != 2 {
		t.Fatalf("released starts = %+v", starts)
	}
}

func TestRerunFailedRejectsOtherStatesAndBrokenSnapshot(t *testing.T) {
	workflow := onePhaseWorkflow("retry-failed", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Paused: true}, map[string]def.Workflow{"retry-failed": workflow}, []string{"project"}, nil)
	for index, state := range []State{StateRunning, StateNeedsHuman, StateDone, StateCancelled} {
		item := testItem("not-failed-"+string(state), "project", "retry-failed", index)
		item.State = string(state)
		switch state {
		case StateNeedsHuman:
			item.Reason = string(ReasonStuck)
		case StateCancelled:
			item.Reason = string(ReasonInterrupted)
		}
		if err := h.store.CreateWorkItem(item); err != nil {
			t.Fatal(err)
		}
		if err := h.engine.RerunFailed(item.ID, "", false); err == nil || !strings.Contains(err.Error(), "invalid state "+string(state)) {
			t.Fatalf("%s rerun error = %v", state, err)
		}
	}
	broken := testItem("broken", "project", "retry-failed", 6)
	broken.State = string(StateFailed)
	broken.Reason = string(ReasonAgentError)
	broken.Snapshot = json.RawMessage(`{}`)
	if err := h.store.CreateWorkItem(broken); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.RerunFailed(broken.ID, "", false); err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("broken snapshot rerun error = %v", err)
	}

	discarded := testItem("discarded", "project", "retry-failed", 7)
	discarded.State = string(StateFailed)
	discarded.Reason = string(ReasonCheckFailedGenuine)
	discarded.Disposition = json.RawMessage(`{"action":"discarded","policy":"manual","at":1}`)
	if err := h.store.CreateWorkItem(discarded); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.RerunFailed(discarded.ID, "", false); err == nil || !strings.Contains(err.Error(), "discarded") {
		t.Fatalf("discarded rerun error = %v", err)
	}
}

func TestResolveDispositionAndResumeRejections(t *testing.T) {
	h := newHarness(t, Config{Paused: true}, nil, []string{"project"}, nil)
	parked := testItem("parked-disposition", "project", "unused", 0)
	parked.State = string(StateNeedsHuman)
	parked.Reason = string(ReasonDisposition)
	parked.EndedAt = 42
	if err := h.store.CreateWorkItem(parked); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Resume(parked.ID, "", false); err == nil || !strings.Contains(err.Error(), "WorkflowMergeItem") {
		t.Fatalf("disposition resume error = %v", err)
	}
	if err := h.engine.ResolveDisposition(parked.ID); err != nil {
		t.Fatal(err)
	}
	resolved, err := h.store.GetWorkItem(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != string(StateDone) || resolved.Reason != "" || resolved.EndedAt != parked.EndedAt {
		t.Fatalf("resolved disposition = %+v", resolved)
	}
	events := h.emitter.stateEvents(parked.ID)
	if len(events) != 1 || events[0].From != StateNeedsHuman || events[0].To != StateDone || events[0].Reason != "" {
		t.Fatalf("resolve disposition events = %+v", events)
	}
	if err := h.engine.ResolveDisposition(parked.ID); err == nil || !strings.Contains(err.Error(), "invalid state done") {
		t.Fatalf("done resolve error = %v", err)
	}

	wrongReason := testItem("wrong-reason", "project", "unused", 1)
	wrongReason.State = string(StateNeedsHuman)
	wrongReason.Reason = string(ReasonStuck)
	if err := h.store.CreateWorkItem(wrongReason); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.ResolveDisposition(wrongReason.ID); err == nil || !strings.Contains(err.Error(), "want needs-human(disposition)") {
		t.Fatalf("wrong-reason resolve error = %v", err)
	}
}

func TestDetachedStartReportsProvisioningFailureThroughEventsAndSync(t *testing.T) {
	workflow := onePhaseWorkflow("basic", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{}, map[string]def.Workflow{"basic": workflow}, []string{"project"}, nil)
	item := testItem("detached", "project", "basic", 0)
	block := make(chan struct{})
	h.runner.startWait[item.ID] = block
	h.runner.startErrs[item.ID] = errors.Join(ErrSetupFailed, errors.New("provision failed"))
	startResult := make(chan error, 1)
	go func() { startResult <- h.engine.StartItemDetachedStarts(item) }()
	var startErr error
	select {
	case startErr = <-startResult:
	case <-time.After(time.Second):
		close(block)
		t.Fatal("detached start waited for runner provisioning")
	}
	if startErr != nil {
		t.Fatalf("detached start returned provisioning error: %v", startErr)
	}
	close(block)
	if err := h.engine.Sync(); !errors.Is(err, ErrSetupFailed) {
		t.Fatalf("Sync error = %v, want setup failure", err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonSetupFailed)
	if got := h.emitter.errorEvents(item.ID); len(got) == 0 {
		t.Fatal("detached provisioning failure did not emit workflow:error")
	}

	// Detached only detaches runner provisioning. Everything the start does
	// synchronously — validation, persistence, definition resolution, phase
	// entry — still reports to the caller, and the run record is parked with a
	// typed reason so the failure is visible after the RPC error is gone.
	unresolvable := testItem("detached-unresolvable", "project", "missing", 1)
	if err := h.engine.StartItemDetachedStarts(unresolvable); err == nil ||
		!strings.Contains(err.Error(), `workflow "missing" not found`) {
		t.Fatalf("detached start definition error = %v", err)
	}
	requireItemState(t, h.store, unresolvable.ID, StateNeedsHuman, ReasonSetupFailed)

	// An unpause triggers held starts. Detached unpausing must not hand the
	// caller their failures either — they land on the item and the event bus.
	// The phase declares a resource the project profile never binds, so the
	// release fails inside the unpause with a typed park.
	unbound := onePhaseWorkflow("unbound", []string{"never-declared"}, []def.Route{{To: "done"}})
	unpause := newHarness(t, Config{Paused: true}, map[string]def.Workflow{"unbound": unbound}, []string{"project"}, nil)
	held := testItem("detached-unpause-start", "project", "unbound", 0)
	if err := unpause.engine.StartItem(held); err != nil {
		t.Fatal(err)
	}
	if got := len(unpause.runner.started()); got != 0 {
		t.Fatalf("paused engine started %d runners", got)
	}
	if err := unpause.engine.PauseDetachedStarts(false); err != nil {
		t.Fatalf("detached unpause returned triggered start error: %v", err)
	}
	requireItemState(t, unpause.store, held.ID, StateNeedsHuman, ReasonSetupFailed)
	if got := unpause.emitter.errorEvents(held.ID); len(got) == 0 {
		t.Fatal("detached unpause failure did not emit workflow:error")
	}
	if err := unpause.engine.Pause(false); err != nil {
		t.Fatalf("waiting unpause reported the already-reported failure again: %v", err)
	}
}

func TestGlobalPauseHoldsPhaseStartsAndUnpauseReleasesThem(t *testing.T) {
	workflow := onePhaseWorkflow("basic", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{}, map[string]def.Workflow{"basic": workflow}, []string{"project"}, nil)
	if paused, err := h.engine.Paused(); err != nil || paused {
		t.Fatalf("initial paused = %v err=%v, want false", paused, err)
	}
	if err := h.engine.Pause(true); err != nil {
		t.Fatal(err)
	}
	if paused, err := h.engine.Paused(); err != nil || !paused {
		t.Fatalf("paused = %v err=%v, want true", paused, err)
	}
	first := testItem("first", "project", "basic", 0)
	second := testItem("second", "project", "basic", 1)
	for _, item := range []store.WorkItem{first, second} {
		if err := h.engine.StartItem(item); err != nil {
			t.Fatal(err)
		}
		requireItemState(t, h.store, item.ID, StateRunning, "")
	}
	if got := len(h.runner.started()); got != 0 {
		t.Fatalf("paused engine started %d runners", got)
	}
	phases, err := h.store.ListWorkItemPhases(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0].Status != "running" {
		t.Fatalf("held phase attempt = %+v, want one running row", phases)
	}
	if err := h.engine.Pause(false); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[0].Key.ItemID != first.ID || starts[1].Key.ItemID != second.ID {
		t.Fatalf("released starts = %+v, want FIFO release", starts)
	}
	// Repeating the flag value is a no-op: one event per real change.
	if err := h.engine.Pause(false); err != nil {
		t.Fatal(err)
	}
	want := []EngineState{{Paused: false}, {Paused: true}, {Paused: false}}
	if got := h.emitter.engineStateEvents(); !reflect.DeepEqual(got, want) {
		t.Fatalf("engine state events = %+v, want %+v", got, want)
	}
}

// TestGlobalPauseLetsInFlightTurnsFinishAndRestsAtThePhaseBoundary pins the
// documented pause semantics: pausing never interrupts a live turn, and the
// item that finishes one stays `running` with its next phase held.
func TestGlobalPauseLetsInFlightTurnsFinishAndRestsAtThePhaseBoundary(t *testing.T) {
	workflow := def.Workflow{ID: "two-phase", Phases: []def.Phase{
		agentPhase("one", nil, []def.Route{{To: "two"}}),
		agentPhase("two", nil, []def.Route{{To: "done"}}),
	}}
	h := newHarness(t, Config{}, map[string]def.Workflow{"two-phase": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "two-phase", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Pause(true); err != nil {
		t.Fatal(err)
	}
	if got := len(h.runner.started()); got != 1 {
		t.Fatalf("pause disturbed the in-flight turn: %d starts", got)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateRunning, "")
	if got := len(h.runner.started()); got != 1 {
		t.Fatalf("paused engine started the next phase: %+v", h.runner.started())
	}
	if err := h.engine.Pause(false); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].Key.PhaseID != "two" {
		t.Fatalf("unpaused starts = %+v, want the held second phase", starts)
	}
}

func TestEngineInstanceCannotRestartAfterClose(t *testing.T) {
	workflow := onePhaseWorkflow("basic", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{}, map[string]def.Workflow{"basic": workflow}, []string{"project"}, nil)
	if err := h.engine.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Start(context.Background()); err == nil {
		t.Fatal("expected one-shot engine restart rejection")
	}
}

func TestRunnerStartupDoesNotBlockEngineCancellation(t *testing.T) {
	workflow := onePhaseWorkflow("slow-start", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Paused: true}, map[string]def.Workflow{"slow-start": workflow}, []string{"project"}, nil)
	h.limitProviderCapacity("project", 1)
	item := testItem("slow", "project", "slow-start", 0)
	block := make(chan struct{})
	heldBlock := make(chan struct{})
	h.runner.startWait[item.ID] = block
	h.runner.startWait["held"] = heldBlock
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.StartItem(testItem("held", "project", "slow-start", 1)); err != nil {
		t.Fatal(err)
	}
	startResult := make(chan error, 1)
	go func() { startResult <- h.engine.Pause(false) }()
	deadline := time.Now().Add(time.Second)
	for len(h.runner.started()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(h.runner.started()) == 0 {
		t.Fatal("runner startup did not begin")
	}
	cancelResult := make(chan error, 1)
	go func() { cancelResult <- h.engine.Cancel(item.ID) }()
	select {
	case err := <-cancelResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel blocked behind runner startup")
	}
	select {
	case err := <-startResult:
		if err != nil {
			t.Fatalf("stale cancelled start returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("startup caller did not settle after cancellation")
	}
	requireItemState(t, h.store, item.ID, StateCancelled, ReasonInterrupted)
	close(heldBlock)
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].Key.ItemID != "held" {
		t.Fatalf("starts = %+v, want the freed capacity to release the held phase", starts)
	}
}

func TestFSMTransitionTableIsClosed(t *testing.T) {
	states := []State{StateRunning, StateNeedsHuman, StateDone, StateFailed, StateCancelled}
	want := map[State]map[State]bool{
		StateRunning: {StateNeedsHuman: true, StateDone: true, StateFailed: true, StateCancelled: true},
		// Resume is one way out of a park; cancel is the other, so a run resting
		// at a gate nobody will approve is not immortal.
		StateNeedsHuman: {StateRunning: true, StateCancelled: true},
		StateDone:       {},
		// Rerun is the only way back out of failed.
		StateFailed:    {StateRunning: true},
		StateCancelled: {},
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
	h := newHarness(t, Config{}, map[string]def.Workflow{"question": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "question", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonQuestion)
	if err := h.engine.Resume(item.ID, "", false); err != nil {
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

func TestAnswerQuestionContinuesPriorThread(t *testing.T) {
	workflow := onePhaseWorkflow("question", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{}, map[string]def.Workflow{"question": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "question", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	seedThread(t, h.store, "thread-one")
	if err := h.store.AttachWorkItemPhaseRun(item.ID, "work", 1, "thread-one", "/tmp/narrative.md"); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Answer(item.ID, "Use the safe option"); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].Key.Attempt != 2 || starts[1].PriorThreadID != "thread-one" ||
		starts[1].PromptMode != PromptContinue ||
		starts[1].Feedback == nil || starts[1].Feedback.Note != "Use the safe option" {
		t.Fatalf("answer runner starts = %+v", starts)
	}
	phases, err := h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	var input PhaseInput
	if len(phases) != 2 || json.Unmarshal(phases[1].InputEnvelope, &input) != nil || input.Feedback == nil || input.Feedback.Note != "Use the safe option" {
		t.Fatalf("answer phase input = %+v, phases=%+v", input, phases)
	}
}

func TestAnswerQuestionRejections(t *testing.T) {
	workflow := onePhaseWorkflow("question", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{}, map[string]def.Workflow{"question": workflow}, []string{"project"}, nil)
	if err := h.engine.Answer("missing", ""); err == nil {
		t.Fatal("empty answer succeeded")
	}
	if err := h.engine.Answer("missing", string(make([]byte, maxHumanNoteBytes+1))); err == nil {
		t.Fatal("oversized answer succeeded")
	}

	item := testItem("stuck", "project", "question", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeStuck, Envelope: stuckEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Answer(item.ID, "answer"); err == nil {
		t.Fatal("answering a stuck item succeeded")
	}
}

// TestAnswerQuestionWaitsForProviderCapacity replaces the old "answer is
// refused when concurrency is full" behavior: with no queue, an answered run
// re-enters its phase and that phase waits on the provider bound like any
// other, instead of handing the human an error.
func TestAnswerQuestionWaitsForProviderCapacity(t *testing.T) {
	workflow := onePhaseWorkflow("question", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{}, map[string]def.Workflow{"question": workflow}, []string{"project"}, nil)
	h.limitProviderCapacity("project", 1)
	question := testItem("question", "project", "question", 0)
	if err := h.engine.StartItem(question); err != nil {
		t.Fatal(err)
	}
	seedThread(t, h.store, "thread-question")
	if err := h.store.AttachWorkItemPhaseRun(question.ID, "work", 1, "thread-question", "/tmp/narrative.md"); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, question.ID, Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	blocker := testItem("blocker", "project", "question", 1)
	if err := h.engine.StartItem(blocker); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Answer(question.ID, "answer"); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, question.ID, StateRunning, "")
	if got := len(h.runner.started()); got != 2 {
		t.Fatalf("runner starts = %d, want the answered phase held behind provider capacity", got)
	}
	h.runner.complete(t, blocker.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 3 || starts[2].Key.ItemID != question.ID || starts[2].Key.Attempt != 2 ||
		starts[2].PriorThreadID != "thread-question" {
		t.Fatalf("released answer start = %+v", starts)
	}
}

func TestGateUsesSeedsAndNamespacedPhaseOutputs(t *testing.T) {
	workflow := def.Workflow{ID: "routing", Inputs: map[string]def.Variable{
		"enabled": {Schema: def.JSONSchema{Type: "boolean"}},
	}, Phases: []def.Phase{
		agentPhase("one", nil, []def.Route{{
			When: &def.Predicate{All: []def.Predicate{
				{Eq: &def.Comparison{Ref: "enabled", Value: true}},
				{Eq: &def.Comparison{Ref: "one.ok", Value: true}},
			}}, To: "two",
		}}),
		agentPhase("two", nil, []def.Route{{To: "done"}}),
	}}
	h := newHarness(t, Config{}, map[string]def.Workflow{"routing": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "routing", 0)
	item.Seeds = json.RawMessage(`{"enabled":true}`)
	if err := h.engine.StartItem(item); err != nil {
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
	h := newHarness(t, Config{}, map[string]def.Workflow{"no-match": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "no-match", 0)
	if err := h.engine.StartItem(item); err != nil {
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
	h := newHarness(t, Config{}, map[string]def.Workflow{"park": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "park", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonGate)
	if err := h.engine.Resume(item.ID, "", false); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].Key.Attempt != 2 {
		t.Fatalf("explicit park resume starts = %+v", starts)
	}
}

func TestGateLoopExhaustionFallsThroughToNextRoute(t *testing.T) {
	workflow := def.Workflow{ID: "fallthrough", Phases: []def.Phase{
		agentPhase("build", nil, []def.Route{{To: "review"}}),
		agentPhase("review", nil, []def.Route{
			{Loop: "build", Max: def.LiteralBound(1)},
			{To: "done"},
		}),
	}}
	h := newHarness(t, Config{}, map[string]def.Workflow{"fallthrough": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "fallthrough", 0)
	if err := h.engine.StartItem(item); err != nil {
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
	h := newHarness(t, Config{}, map[string]def.Workflow{"cancel": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "cancel", 0)
	if err := h.engine.StartItem(item); err != nil {
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
