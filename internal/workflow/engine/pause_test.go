package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

func TestResumeNoteNamesProviderContextDisposition(t *testing.T) {
	for _, test := range []struct {
		name    string
		context resumeContext
		want    string
	}{
		{name: "same context", context: resumeContextAvailable, want: continuationAvailableNote},
		{name: "lost context", context: resumeContextUnavailable, want: continuationUnavailableNote},
		{name: "turn never started", context: resumeContextNotStarted,
			want: "the parked attempt never started a provider turn, so run its full prompt now"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := resumeNote(ReasonPaused, test.context); !strings.Contains(got, test.want) {
				t.Fatalf("resume note = %q, want it to contain %q", got, test.want)
			}
		})
	}
}

// seedThread creates the thread row a parked attempt's session id points at, so
// resume's existence probe sees a live session rather than a deleted one.
func seedThread(t *testing.T, database *store.Store, threadID string) {
	t.Helper()
	if err := database.CreateThread(store.Thread{
		ID: threadID, ProjectID: "project", ProjectPath: "/tmp/project",
		Title: threadID, Provider: testProvider, Model: "test-model",
		Mode: "workflow", SessionRef: threadID, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

// startPausableItem admits one single-shape run and attaches the provider
// thread its attempt is running on.
func startPausableItem(t *testing.T, h *testHarness, itemID, threadID string) string {
	t.Helper()
	workflowID := "pausable"
	item := testItem(itemID, "project", workflowID, 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	if threadID != "" {
		seedThread(t, h.store, threadID)
		if err := h.store.AttachWorkItemPhaseRun(item.ID, "work", 1, threadID, "/tmp/narrative.md"); err != nil {
			t.Fatal(err)
		}
	}
	return item.ID
}

func newPauseHarness(t *testing.T) *testHarness {
	t.Helper()
	workflow := onePhaseWorkflow("pausable", []string{"writer"}, []def.Route{{To: "done"}})
	h := newHarness(t, Config{}, map[string]def.Workflow{"pausable": workflow}, []string{"project"}, nil)
	h.profiles.setCapacity("project", "writer", 1)
	return h
}

func TestPauseParksRunReleasesResourcesAndStopsTheTurn(t *testing.T) {
	h := newPauseHarness(t)
	item := startPausableItem(t, h, "item", "thread-one")

	if err := h.engine.PauseItem(item); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonPaused)
	// Pause interrupts NOW rather than letting the turn finish — that is the
	// whole difference from the unit-failure policy.
	if stopped := h.runner.stopped(); len(stopped) != 1 || stopped[0].ItemID != item {
		t.Fatalf("runner stops = %+v, want the live attempt stopped once", stopped)
	}
	h.requireNoHeldResources(t)
	phases, err := h.store.ListWorkItemPhases(item)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0].Status != "parked" || phases[0].EndedAt == 0 {
		t.Fatalf("paused phase = %+v, want one parked attempt", phases)
	}
	// The freed capacity goes to the next run, which is what makes pause a
	// scheduling action and not just a state write.
	other := testItem("other", "project", "pausable", 1)
	if err := h.engine.StartItem(other); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].Key.ItemID != other.ID {
		t.Fatalf("runner starts after pause = %+v, want the freed writer handed on", starts)
	}
}

func TestResumeContinuesTheParkedProviderThread(t *testing.T) {
	h := newPauseHarness(t)
	item := startPausableItem(t, h, "item", "thread-one")
	if err := h.engine.PauseItem(item); err != nil {
		t.Fatal(err)
	}

	if err := h.engine.ResumeItem(item); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateRunning, "")
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].Key.Attempt != 2 || starts[1].Launch.ThreadID() != "thread-one" {
		t.Fatalf("resume runner starts = %+v, want a second attempt on the parked thread", starts)
	}
	if starts[1].Feedback == nil || !strings.Contains(starts[1].Feedback.Note, "resumed after a pause") ||
		!strings.Contains(starts[1].Feedback.Note, "continue from where the previous turn stopped") {
		t.Fatalf("resume feedback = %+v", starts[1].Feedback)
	}
	h.runner.complete(t, item, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateDone, "")
}

// A crash-parked run resumes through the same path with the same shape; only
// the note differs, because the reason exists for the human reading the list.
func TestResumeInterruptedRunUsesTheSameContinuation(t *testing.T) {
	workflow := onePhaseWorkflow("crash", nil, []def.Route{{To: "done"}})
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
		if err := database.CreateThread(store.Thread{
			ID: "crash-thread", ProjectID: "project", ProjectPath: "/tmp/project",
			Title: "crash", Provider: testProvider, Model: "test-model",
			Mode: "workflow", SessionRef: "crash-thread", CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
		if err := database.CreateWorkItemPhase(store.WorkItemPhase{
			ItemID: item.ID, PhaseID: "work", Attempt: 1, ThreadID: "crash-thread",
			InputEnvelope: json.RawMessage(`{}`), Status: "running", StartedAt: 21,
		}); err != nil {
			t.Fatal(err)
		}
	})
	requireItemState(t, h.store, "crashed", StateNeedsHuman, ReasonInterrupted)

	if err := h.engine.ResumeItem("crashed"); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 1 || starts[0].Key.Attempt != 2 || starts[0].Launch.ThreadID() != "crash-thread" {
		t.Fatalf("interrupted resume starts = %+v", starts)
	}
	if starts[0].Feedback == nil || !strings.Contains(starts[0].Feedback.Note, "resumed after the run was interrupted") {
		t.Fatalf("interrupted resume feedback = %+v", starts[0].Feedback)
	}
}

// A resumed attempt whose provider session is gone re-enters the phase from its
// inputs, and says so in the feedback the next turn reads.
func TestResumeWithoutASessionStartsAFreshAttemptLoudly(t *testing.T) {
	h := newPauseHarness(t)
	item := startPausableItem(t, h, "item", "thread-one")
	if err := h.engine.PauseItem(item); err != nil {
		t.Fatal(err)
	}
	if err := h.store.DeleteThread("thread-one"); err != nil {
		t.Fatal(err)
	}

	if err := h.engine.ResumeItem(item); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].Launch.ReusesThread() {
		t.Fatalf("resume after session loss = %+v, want a fresh attempt", starts)
	}
	if starts[1].Feedback == nil ||
		!strings.Contains(starts[1].Feedback.Note, "provider session is unavailable") {
		t.Fatalf("session-loss feedback = %+v, want the loss recorded", starts[1].Feedback)
	}
}

func TestResumeRefusesReasonsThatAreNotAContinuation(t *testing.T) {
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
	err := h.engine.ResumeItem(item.ID)
	if err == nil || !strings.Contains(err.Error(), "continuing a parked run applies to") {
		t.Fatalf("resume of a question park = %v, want a typed refusal", err)
	}
	// The run is untouched: a refusal is not a state change.
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonQuestion)
}

func TestPauseRefusesACalledRunAndPausesTheWholeTree(t *testing.T) {
	h := newCallHarness(t, defaultCallWorkflows(), nil)
	parent := startCaller(t, h)
	child := h.callChild(t, parent, "audit", 1)
	seedThread(t, h.store, "child-thread")
	if err := h.store.AttachWorkItemPhaseRun(child.ID, "work", 1, "child-thread", "/tmp/child.md"); err != nil {
		t.Fatal(err)
	}

	err := h.engine.PauseItem(child.ID)
	if err == nil || !strings.Contains(err.Error(), "pause the run that called it") {
		t.Fatalf("pause of a called run = %v, want a refusal naming the caller", err)
	}
	requireItemState(t, h.store, child.ID, StateRunning, "")

	if err := h.engine.PauseItem(parent); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, parent, StateNeedsHuman, ReasonPaused)
	requireItemState(t, h.store, child.ID, StateNeedsHuman, ReasonPaused)
	// The child's live turn came down; the parent held no runner of its own.
	if stopped := h.runner.stopped(); len(stopped) != 1 || stopped[0].ItemID != child.ID {
		t.Fatalf("runner stops = %+v, want only the child's live attempt", stopped)
	}
	h.requireNoHeldResources(t)
}

func TestResumeWalksTheTreeAndReturnsTheParentToWaiting(t *testing.T) {
	h := newCallHarness(t, defaultCallWorkflows(), nil)
	parent := startCaller(t, h)
	child := h.callChild(t, parent, "audit", 1)
	seedThread(t, h.store, "child-thread")
	if err := h.store.AttachWorkItemPhaseRun(child.ID, "work", 1, "child-thread", "/tmp/child.md"); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.PauseItem(parent); err != nil {
		t.Fatal(err)
	}

	if err := h.engine.ResumeItem(parent); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, parent, StateRunning, "")
	requireItemState(t, h.store, child.ID, StateRunning, "")
	// The parent runs no turn of its own: only the child restarted, and on the
	// session it parked on.
	childStarts := 0
	for _, start := range h.runner.started() {
		if start.Key.ItemID == child.ID && start.Key.Attempt == 2 {
			childStarts++
			if start.Launch.ThreadID() != "child-thread" {
				t.Fatalf("resumed child prior thread = %q", start.Launch.ThreadID())
			}
		}
		if start.Key.ItemID == parent && start.Key.PhaseID == "audit" {
			t.Fatalf("call phase started a runner: %+v", start)
		}
	}
	if childStarts != 1 {
		t.Fatalf("resumed child starts = %d, want exactly one", childStarts)
	}
	// The tree still settles the ordinary way once the child finishes.
	h.runner.complete(t, child.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, child.ID, StateDone, "")
	requireItemState(t, h.store, parent, StateRunning, "")
}

// A descendant parked for a reason of its own is not swept up by the resume:
// the parent goes back to waiting on it, which is the correct resting shape.
func TestResumeLeavesADescendantParkedForItsOwnReason(t *testing.T) {
	h := newCallHarness(t, defaultCallWorkflows(), nil)
	parent := startCaller(t, h)
	child := h.callChild(t, parent, "audit", 1)
	seedThread(t, h.store, "child-thread")
	if err := h.store.AttachWorkItemPhaseRun(child.ID, "work", 1, "child-thread", "/tmp/child.md"); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, child.ID, Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, child.ID, StateNeedsHuman, ReasonQuestion)
	if err := h.engine.PauseItem(parent); err != nil {
		t.Fatal(err)
	}
	// Pausing does not rewrite why the child needs a human.
	requireItemState(t, h.store, child.ID, StateNeedsHuman, ReasonQuestion)
	requireItemState(t, h.store, parent, StateNeedsHuman, ReasonPaused)

	if err := h.engine.ResumeItem(parent); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, parent, StateRunning, "")
	requireItemState(t, h.store, child.ID, StateNeedsHuman, ReasonQuestion)
	// Answering the child still re-enters the parent, which is what "back to
	// waiting" has to mean.
	if err := h.engine.Answer(child.ID, "use the safe option"); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, child.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, child.ID, StateDone, "")
}

func TestResumeRepairsAnInterruptedFanOutAttempt(t *testing.T) {
	workflow := fanOutWorkflow("fan", 2)
	h := newHarness(t, Config{}, map[string]def.Workflow{"fan": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "fan", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	// One unit finishes before the pause; the other is cut mid-flight.
	h.runner.completeRun(t, unitKey(item.ID, "work", 1, "work-unit-0"),
		Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.PauseItem(item.ID); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonPaused)
	h.requireUnitStatuses(t, item.ID, "work", 1, map[string]string{
		"work-unit-0": string(store.WorkItemUnitDone),
		"work-unit-1": string(store.WorkItemUnitFailed),
		"work-join":   string(store.WorkItemUnitPending),
	})

	if err := h.engine.ResumeItem(item.ID); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateRunning, "")
	// The finished unit keeps its result; only the interrupted one re-runs.
	h.requireUnitStatuses(t, item.ID, "work", 1, map[string]string{
		"work-unit-0": string(store.WorkItemUnitDone),
		"work-unit-1": string(store.WorkItemUnitRunning),
		"work-join":   string(store.WorkItemUnitPending),
	})
	relaunched := 0
	for _, start := range h.runner.started() {
		if start.Key.UnitID == "work-unit-1" {
			relaunched++
		}
		if start.Key.UnitID == "work-unit-0" && relaunched > 0 {
			t.Fatal("resume re-ran a unit that had already produced a result")
		}
	}
	if relaunched != 2 {
		t.Fatalf("interrupted unit launches = %d, want the original plus one repair", relaunched)
	}
	h.runner.completeRun(t, unitKey(item.ID, "work", 1, "work-unit-1"),
		Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey(item.ID, "work", 1, "work-join"),
		Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateDone, "")
}

// A fan-out whose join already ran continues the join rather than re-expanding
// the units the join exists to consolidate.
func TestResumeContinuesAnInterruptedFanOutJoin(t *testing.T) {
	workflow := fanOutWorkflow("fan", 1)
	h := newHarness(t, Config{}, map[string]def.Workflow{"fan": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "fan", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey(item.ID, "work", 1, "work-unit-0"),
		Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	seedThread(t, h.store, "join-thread")
	if err := h.store.AttachWorkItemPhaseRun(item.ID, "work", 1, "join-thread", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.PauseItem(item.ID); err != nil {
		t.Fatal(err)
	}

	if err := h.engine.ResumeItem(item.ID); err != nil {
		t.Fatal(err)
	}
	joinStarts := make([]RunRequest, 0)
	for _, start := range h.runner.started() {
		if start.Key.UnitID == "work-join" {
			joinStarts = append(joinStarts, start)
		}
		if start.Key.UnitID == "work-unit-0" && len(joinStarts) > 0 {
			t.Fatal("resume re-expanded the units under a join that had already run")
		}
	}
	if len(joinStarts) != 2 || joinStarts[1].Launch.ThreadID() != "join-thread" {
		t.Fatalf("join starts = %+v, want a retry on the thread the attempt parked on", joinStarts)
	}
}

// Graceful quit: every active root comes down, and what it leaves behind is
// resumable — that is the whole point of pausing instead of dying.
func TestPauseAllActiveLeavesEveryRootResumable(t *testing.T) {
	h := newPauseHarness(t)
	h.profiles.setCapacity("project", "writer", 3)
	h.limitProviderCapacity("project", 3)
	first := startPausableItem(t, h, "first", "thread-first")
	second := testItem("second", "project", "pausable", 1)
	if err := h.engine.StartItem(second); err != nil {
		t.Fatal(err)
	}
	seedThread(t, h.store, "thread-second")
	if err := h.store.AttachWorkItemPhaseRun(second.ID, "work", 1, "thread-second", ""); err != nil {
		t.Fatal(err)
	}
	parked := testItem("parked", "project", "pausable", 2)
	if err := h.engine.StartItem(parked); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, parked.ID, Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	if err := h.engine.PauseAllActive(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, first, StateNeedsHuman, ReasonPaused)
	requireItemState(t, h.store, second.ID, StateNeedsHuman, ReasonPaused)
	// An already-parked run keeps the reason it parked under.
	requireItemState(t, h.store, parked.ID, StateNeedsHuman, ReasonQuestion)
	h.requireNoHeldResources(t)

	if err := h.engine.ResumeItem(first); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.ResumeItem(second.ID); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, first, StateRunning, "")
	requireItemState(t, h.store, second.ID, StateRunning, "")
}

func TestPauseAllActiveWalksCalledRunsThroughTheirRoot(t *testing.T) {
	h := newCallHarness(t, defaultCallWorkflows(), nil)
	parent := startCaller(t, h)
	child := h.callChild(t, parent, "audit", 1)

	if err := h.engine.PauseAllActive(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, parent, StateNeedsHuman, ReasonPaused)
	requireItemState(t, h.store, child.ID, StateNeedsHuman, ReasonPaused)
	if stopped := h.runner.stopped(); len(stopped) != 1 || stopped[0].ItemID != child.ID {
		t.Fatalf("runner stops = %+v, want the child's live attempt stopped exactly once", stopped)
	}
}

func TestPauseOfATerminalOrParkedRunChangesNothing(t *testing.T) {
	h := newPauseHarness(t)
	item := startPausableItem(t, h, "item", "thread-one")
	h.runner.complete(t, item, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateDone, "")

	if err := h.engine.PauseItem(item); err != nil {
		t.Fatalf("pausing a finished run = %v, want a no-op", err)
	}
	requireItemState(t, h.store, item, StateDone, "")
	if err := h.engine.ResumeItem(item); err == nil {
		t.Fatal("resume of a finished run succeeded")
	}
}

// A phase waiting for capacity holds a start rather than a runner. Pausing must
// release that hold too, or the run would sit forever behind a queue position
// nothing will ever consume.
func TestPauseReleasesAHeldStartForAWaitingPhase(t *testing.T) {
	h := newPauseHarness(t)
	holder := startPausableItem(t, h, "holder", "")
	waiting := testItem("waiting", "project", "pausable", 1)
	if err := h.engine.StartItem(waiting); err != nil {
		t.Fatal(err)
	}
	if starts := h.runner.started(); len(starts) != 1 || starts[0].Key.ItemID != holder {
		t.Fatalf("runner starts = %+v, want only the capacity holder running", starts)
	}

	if err := h.engine.PauseItem(waiting.ID); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, waiting.ID, StateNeedsHuman, ReasonPaused)
	if h.runner.stopCount() != 0 {
		t.Fatalf("runner stops = %d, want none — the paused phase never started", h.runner.stopCount())
	}
	// Releasing the holder must not hand capacity to the paused run.
	h.runner.complete(t, holder, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if starts := h.runner.started(); len(starts) != 1 {
		t.Fatalf("runner starts after the holder finished = %+v, want the paused run left alone", starts)
	}
	h.requireNoHeldResources(t)
}
