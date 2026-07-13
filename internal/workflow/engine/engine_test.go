package engine

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/workflow/def"
)

func TestRemoveQueuedItemCancelsRecordAndLosesRaceToStart(t *testing.T) {
	workflow := onePhaseWorkflow("basic", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: false, GlobalConcurrency: 1}, map[string]def.Workflow{"basic": workflow}, []string{"project"}, nil)
	queued := testItem("queued", "project", "basic", 0)
	if err := h.engine.Enqueue(queued); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.RemoveQueued(queued.ID); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, queued.ID, StateCancelled, ReasonInterrupted)
	if events := h.emitter.stateEvents(queued.ID); len(events) != 1 || events[0].From != StateQueued || events[0].To != StateCancelled {
		t.Fatalf("remove events = %+v", events)
	}

	running := testItem("running", "project", "basic", 1)
	if err := h.engine.Enqueue(running); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.SetQueue(true, 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.RemoveQueued(running.ID); err == nil {
		t.Fatal("remove queued item succeeded after start won the race")
	}
}

func TestReenqueueFailedDrainsWithDiagnosisAtQueueTail(t *testing.T) {
	failedWhenFalse := def.Predicate{Eq: &def.Comparison{Ref: "work.ok", Value: false}}
	workflow := onePhaseWorkflow("retry-failed", nil, []def.Route{
		{When: &failedWhenFalse, To: "failed"},
		{To: "done"},
	})
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{"retry-failed": workflow}, []string{"project"}, nil)
	item := testItem("failed", "project", "retry-failed", 0)
	if err := h.engine.Enqueue(item); err != nil {
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
	if err := h.engine.SetQueue(false, 0, 1); err != nil {
		t.Fatal(err)
	}
	tail := testItem("tail", "project", "retry-failed", 4)
	if err := h.engine.Enqueue(tail); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.ReenqueueFailed(item.ID); err != nil {
		t.Fatal(err)
	}
	reloaded, err := h.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State != string(StateQueued) || reloaded.Reason != "" || reloaded.EndedAt != 0 {
		t.Fatalf("re-enqueued queued state = %+v", reloaded)
	}
	if reloaded.SortPosition <= tail.SortPosition {
		t.Fatalf("re-enqueued sort position = %d, want after %d", reloaded.SortPosition, tail.SortPosition)
	}
	if err := h.engine.SetQueue(true, 0, 1); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].Key.ItemID != tail.ID {
		t.Fatalf("queue tail ordering starts = %+v", starts)
	}
	h.runner.complete(t, tail.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	starts = h.runner.started()
	if len(starts) != 3 || starts[2].Key.ItemID != item.ID || starts[2].Key.PhaseID != "work" || starts[2].Key.Attempt != 2 {
		t.Fatalf("re-enqueued starts = %+v", starts)
	}
	if starts[2].Feedback == nil || starts[2].Feedback.Note != "check-failed-genuine: tests still fail" {
		t.Fatalf("re-enqueued feedback = %+v", starts[2].Feedback)
	}
	if starts[2].Feedback.Values["work.diagnosis"] != "tests still fail" {
		t.Fatalf("re-enqueued feedback values = %+v", starts[2].Feedback.Values)
	}
	reloaded, err = h.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.StartedAt <= failed.StartedAt {
		t.Fatalf("re-enqueued start time = %d, original = %d", reloaded.StartedAt, failed.StartedAt)
	}
	phases, err := h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	var input PhaseInput
	if len(phases) != 2 || json.Unmarshal(phases[1].InputEnvelope, &input) != nil || input.Feedback == nil || input.Feedback.Note != starts[2].Feedback.Note {
		t.Fatalf("re-enqueued phase input = %+v, phases=%+v", input, phases)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateDone, "")
}

func TestReenqueueFailedRejectsOtherStatesAndBrokenSnapshot(t *testing.T) {
	workflow := onePhaseWorkflow("retry-failed", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: false, GlobalConcurrency: 1}, map[string]def.Workflow{"retry-failed": workflow}, []string{"project"}, nil)
	for index, state := range []State{StateQueued, StateRunning, StateNeedsHuman, StateDone, StateCancelled} {
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
		if err := h.engine.ReenqueueFailed(item.ID); err == nil || !strings.Contains(err.Error(), "invalid state "+string(state)) {
			t.Fatalf("%s re-enqueue error = %v", state, err)
		}
	}
	broken := testItem("broken", "project", "retry-failed", 6)
	broken.State = string(StateFailed)
	broken.Reason = string(ReasonAgentError)
	broken.Snapshot = json.RawMessage(`{}`)
	if err := h.store.CreateWorkItem(broken); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.ReenqueueFailed(broken.ID); err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("broken snapshot re-enqueue error = %v", err)
	}

	discarded := testItem("discarded", "project", "retry-failed", 7)
	discarded.State = string(StateFailed)
	discarded.Reason = string(ReasonCheckFailedGenuine)
	discarded.Disposition = json.RawMessage(`{"action":"discarded","policy":"manual","at":1}`)
	if err := h.store.CreateWorkItem(discarded); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.ReenqueueFailed(discarded.ID); err == nil || !strings.Contains(err.Error(), "discarded") {
		t.Fatalf("discarded re-enqueue error = %v", err)
	}
}

func TestResolveDispositionAndResumeRejections(t *testing.T) {
	h := newHarness(t, Config{Active: false, GlobalConcurrency: 1}, nil, []string{"project"}, nil)
	parked := testItem("parked-disposition", "project", "unused", 0)
	parked.State = string(StateNeedsHuman)
	parked.Reason = string(ReasonDisposition)
	parked.EndedAt = 42
	if err := h.store.CreateWorkItem(parked); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Resume(parked.ID, ""); err == nil || !strings.Contains(err.Error(), "WorkflowMergeItem") {
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

func TestDetachedEnqueueReportsProvisioningFailureThroughEventsAndSync(t *testing.T) {
	workflow := onePhaseWorkflow("basic", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{"basic": workflow}, []string{"project"}, nil)
	item := testItem("detached", "project", "basic", 0)
	block := make(chan struct{})
	h.runner.startWait[item.ID] = block
	h.runner.startErrs[item.ID] = errors.Join(ErrSetupFailed, errors.New("provision failed"))
	enqueueResult := make(chan error, 1)
	go func() { enqueueResult <- h.engine.EnqueueDetachedStarts(item) }()
	var enqueueErr error
	select {
	case enqueueErr = <-enqueueResult:
	case <-time.After(time.Second):
		close(block)
		t.Fatal("detached enqueue waited for runner provisioning")
	}
	if enqueueErr != nil {
		t.Fatalf("detached enqueue returned provisioning error: %v", enqueueErr)
	}
	close(block)
	if err := h.engine.Sync(); !errors.Is(err, ErrSetupFailed) {
		t.Fatalf("Sync error = %v, want setup failure", err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonSetupFailed)
	if got := h.emitter.errorEvents(item.ID); len(got) == 0 {
		t.Fatal("detached provisioning failure did not emit workflow:error")
	}

	unresolvable := testItem("detached-unresolvable", "project", "missing", 1)
	if err := h.engine.EnqueueDetachedStarts(unresolvable); err != nil {
		t.Fatalf("detached enqueue returned triggered definition error: %v", err)
	}
	requireItemState(t, h.store, unresolvable.ID, StateNeedsHuman, ReasonSetupFailed)
	if got := h.emitter.errorEvents(unresolvable.ID); len(got) == 0 {
		t.Fatal("detached definition failure did not emit workflow:error")
	}

	paused := newHarness(t, Config{Active: false, GlobalConcurrency: 1}, map[string]def.Workflow{}, []string{"project"}, nil)
	queued := testItem("detached-queue-start", "project", "missing", 0)
	if err := paused.engine.Enqueue(queued); err != nil {
		t.Fatal(err)
	}
	if err := paused.engine.SetQueueDetachedStarts(true, 0, 1); err != nil {
		t.Fatalf("detached queue activation returned triggered definition error: %v", err)
	}
	requireItemState(t, paused.store, queued.ID, StateNeedsHuman, ReasonSetupFailed)
	if got := paused.emitter.errorEvents(queued.ID); len(got) == 0 {
		t.Fatal("detached queue activation did not emit workflow:error")
	}

	settings := newHarness(t, Config{Active: false, GlobalConcurrency: 1}, map[string]def.Workflow{}, []string{"project"}, nil)
	settingsItem := testItem("detached-settings-start", "project", "missing", 0)
	if err := settings.engine.Enqueue(settingsItem); err != nil {
		t.Fatal(err)
	}
	active := true
	if err := settings.engine.UpdateQueueSettingsDetachedStarts(&active, 1); err != nil {
		t.Fatalf("detached settings activation returned triggered definition error: %v", err)
	}
	requireItemState(t, settings.store, settingsItem.ID, StateNeedsHuman, ReasonSetupFailed)
	if got := settings.emitter.errorEvents(settingsItem.ID); len(got) == 0 {
		t.Fatal("detached settings activation did not emit workflow:error")
	}
}

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
	if err := h.engine.SetQueue(true, 0, 1); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateRunning, "")
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateDone, "")
	want := []StateEvent{
		{ItemID: item.ID, ProjectID: item.ProjectID, From: StateQueued, To: StateRunning},
		{ItemID: item.ID, ProjectID: item.ProjectID, From: StateRunning, To: StateDone},
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

func TestRunnerStartupDoesNotBlockEngineCancellation(t *testing.T) {
	workflow := onePhaseWorkflow("slow-start", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: false, GlobalConcurrency: 1}, map[string]def.Workflow{"slow-start": workflow}, []string{"project"}, nil)
	item := testItem("slow", "project", "slow-start", 0)
	if err := h.engine.Enqueue(item); err != nil {
		t.Fatal(err)
	}
	block := make(chan struct{})
	queuedBlock := make(chan struct{})
	h.runner.startWait[item.ID] = block
	h.runner.startWait["queued"] = queuedBlock
	if err := h.engine.Enqueue(testItem("queued", "project", "slow-start", 1)); err != nil {
		t.Fatal(err)
	}
	startResult := make(chan error, 1)
	go func() { startResult <- h.engine.SetQueue(true, 0, 1) }()
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
	close(queuedBlock)
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
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
	if err := h.engine.SetQueue(true, 2, 2); err != nil {
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
	if err := h.engine.SetQueue(true, 0, 2); err != nil {
		t.Fatal(err)
	}
	starts = h.runner.started()
	if len(starts) != 3 || starts[2].Key.ItemID != "last" {
		t.Fatalf("resumed starts = %+v", starts)
	}
}

func TestSettingsQueueUpdatePreservesProcessBound(t *testing.T) {
	workflow := onePhaseWorkflow("basic", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: false, GlobalConcurrency: 1}, map[string]def.Workflow{"basic": workflow}, []string{"project"}, nil)
	for index, id := range []string{"one", "two", "three"} {
		if err := h.engine.Enqueue(testItem(id, "project", "basic", index)); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.engine.SetQueue(true, 2, 1); err != nil {
		t.Fatal(err)
	}
	active := true
	if err := h.engine.UpdateQueueSettings(&active, 1); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, "one", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, "two", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.UpdateQueueSettings(nil, 2); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[0].Key.ItemID != "one" || starts[1].Key.ItemID != "two" {
		t.Fatalf("starts after settings update = %+v, want process bound preserved at two", starts)
	}
}

func TestAnswerQuestionContinuesPriorThread(t *testing.T) {
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
	if err := h.engine.Answer(item.ID, "Use the safe option"); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].Key.Attempt != 2 || starts[1].PriorThreadID != "thread-one" ||
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
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{"question": workflow}, []string{"project"}, nil)
	if err := h.engine.Answer("missing", ""); err == nil {
		t.Fatal("empty answer succeeded")
	}
	if err := h.engine.Answer("missing", string(make([]byte, maxHumanNoteBytes+1))); err == nil {
		t.Fatal("oversized answer succeeded")
	}

	item := testItem("stuck", "project", "question", 0)
	if err := h.engine.Enqueue(item); err != nil {
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

func TestAnswerQuestionRejectsWhenConcurrencyIsFull(t *testing.T) {
	workflow := onePhaseWorkflow("question", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: true, GlobalConcurrency: 1}, map[string]def.Workflow{"question": workflow}, []string{"project"}, nil)
	question := testItem("question", "project", "question", 0)
	if err := h.engine.Enqueue(question); err != nil {
		t.Fatal(err)
	}
	if err := h.store.AttachWorkItemPhaseRun(question.ID, "work", 1, "thread-question", "/tmp/narrative.md"); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, question.ID, Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Enqueue(testItem("blocker", "project", "question", 1)); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Answer(question.ID, "answer"); err == nil {
		t.Fatal("answer succeeded at full concurrency")
	}
}

func TestSetQueueUpdatesConcurrency(t *testing.T) {
	workflow := onePhaseWorkflow("queue", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{Active: false, GlobalConcurrency: 1}, map[string]def.Workflow{"queue": workflow}, []string{"project"}, nil)
	for _, id := range []string{"one", "two"} {
		if err := h.engine.Enqueue(testItem(id, "project", "queue", 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.engine.SetQueue(true, 0, 2); err != nil {
		t.Fatal(err)
	}
	if got := len(h.runner.started()); got != 2 {
		t.Fatalf("started %d items, want 2", got)
	}
	if err := h.engine.SetQueue(true, 0, 0); err == nil {
		t.Fatal("invalid concurrency succeeded")
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
