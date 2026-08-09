package engine

import (
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// A run resting at a human gate holds no runner and no resources, but it is
// still a run: a human who decides the gate will never be approved has to be
// able to stop it without first resuming it into work nobody wants.
func TestCancelOfAGateParkedRunSettlesItTerminally(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{"human": humanWorkflow()}, []string{"project"}, nil)
	item := testItem("item", "project", "human", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	parkAtHumanGate(t, h, item.ID)

	if err := h.engine.Cancel(item.ID); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateCancelled, ReasonInterrupted)
	events := h.emitter.stateEvents(item.ID)
	last := events[len(events)-1]
	if last.From != StateNeedsHuman || last.To != StateCancelled || last.Reason != ReasonInterrupted {
		t.Fatalf("last state event = %+v, want needs-human -> cancelled", last)
	}
	// The attempt that parked keeps its own account of why a human was ever
	// asked; cancelling the run does not rewrite it.
	phases, err := h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	parked := phases[len(phases)-1]
	if parked.Status != "parked" || len(parked.GateTrace) == 0 {
		t.Fatalf("parked attempt after cancel = %+v, want its gate trace intact", parked)
	}

	// Cancelled is terminal for every verb, including cancel.
	if err := h.engine.ResolveHumanGate(item.ID, HumanApprove, ""); err == nil {
		t.Fatal("a cancelled run accepted a gate decision")
	}
	if err := h.engine.Resume(item.ID, "", false); err == nil {
		t.Fatal("a cancelled run was resumed")
	}
	if err := h.engine.Cancel(item.ID); err == nil {
		t.Fatal("a cancelled run was cancelled again")
	}
	h.requireNoHeldResources(t)
}

// Pause parks a whole tree; cancel is how that tree is abandoned rather than
// resumed. The root is not resident either way, and its resting descendants
// come down with it exactly as a running root's teardown brings them down.
func TestCancelOfAPausedTreeCancelsItsRestingDescendants(t *testing.T) {
	h := newCallHarness(t, defaultCallWorkflows(), nil)
	parent := startCaller(t, h)
	child := h.callChild(t, parent, "audit", 1)
	if err := h.engine.PauseItem(parent); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, parent, StateNeedsHuman, ReasonPaused)
	requireItemState(t, h.store, child.ID, StateNeedsHuman, ReasonPaused)

	if err := h.engine.Cancel(parent); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, parent, StateCancelled, ReasonInterrupted)
	requireItemState(t, h.store, child.ID, StateCancelled, ReasonInterrupted)
	h.requireNoHeldResources(t)
}

// The fan-out shape of the same tree. A paused campaign retains the children
// its call units are waiting on, so cancelling the parked root has to reach
// them through the attempt's persisted unit linkage — there is no in-memory
// fan-out state left to read.
func TestCancelOfAPausedCampaignCancelsItsUnitCallChildren(t *testing.T) {
	h := newCallHarness(t, callUnitWorkflows(2), nil)
	parent := startCampaign(t, h)
	first := h.unitCallChild(t, parent, "wave", 1, "wave-unit-0")
	second := h.unitCallChild(t, parent, "wave", 1, "wave-unit-1")
	if err := h.engine.PauseItem(parent); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, parent, StateNeedsHuman, ReasonPaused)

	if err := h.engine.Cancel(parent); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, parent, StateCancelled, ReasonInterrupted)
	requireItemState(t, h.store, first.ID, StateCancelled, ReasonInterrupted)
	requireItemState(t, h.store, second.ID, StateCancelled, ReasonInterrupted)
	h.requireNoHeldResources(t)
}

// A parked descendant cancelled on its own reaches the parent still waiting on
// it through the ordinary terminal-transition path. The parent does not fail:
// the child was stopped on purpose and the parent's own work is intact, so
// whether the tree dies too is the human's next call.
func TestCancelOfAParkedCallChildSettlesTheWaitingParent(t *testing.T) {
	h := newCallHarness(t, defaultCallWorkflows(), nil)
	parent := startCaller(t, h)
	child := h.callChild(t, parent, "audit", 1)
	h.runner.complete(t, child.ID, Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, child.ID, StateNeedsHuman, ReasonQuestion)
	requireItemState(t, h.store, parent, StateRunning, "")

	if err := h.engine.Cancel(child.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, child.ID, StateCancelled, ReasonInterrupted)
	requireItemState(t, h.store, parent, StateNeedsHuman, ReasonAgentError)
	envelope := decodeEnvelope(t, h.phaseAttempt(t, parent, "audit", 1).OutputEnvelope)
	if !strings.Contains(envelope.Reason, child.ID) || !strings.Contains(envelope.Reason, "cancelled") {
		t.Fatalf("parent call envelope = %+v, want it to name the cancelled child", envelope)
	}
	h.requireNoHeldResources(t)
}

// The unit-scoped twin: to a fan-out a cancelled child is one unit that
// produced no result, which is the ordinary unit-failure policy.
func TestCancelOfAParkedUnitCallChildFailsThatUnit(t *testing.T) {
	h := newCallHarness(t, callUnitWorkflows(1), nil)
	parent := startCampaign(t, h)
	child := h.unitCallChild(t, parent, "wave", 1, "wave-unit-0")
	h.runner.complete(t, child.ID, Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, child.ID, StateNeedsHuman, ReasonQuestion)

	if err := h.engine.Cancel(child.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, child.ID, StateCancelled, ReasonInterrupted)
	requireItemState(t, h.store, parent, StateNeedsHuman, ReasonUnitFailed)
	h.requireUnitStatuses(t, parent, "wave", 1, map[string]string{
		"wave-unit-0": store.WorkItemUnitFailed,
		"wave-join":   store.WorkItemUnitPending,
	})
	h.requireNoHeldResources(t)
}

// A disposition park is a DONE run wearing a park while it waits for a merge,
// a PR, or a discard. Cancelling it would rewrite a run that succeeded as one
// that was stopped, so it is refused with the verbs that do settle it.
func TestCancelRefusesADispositionPark(t *testing.T) {
	workflow := onePhaseWorkflow("disposition", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{}, map[string]def.Workflow{"disposition": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "disposition", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.ParkDisposition(item.ID); err != nil {
		t.Fatal(err)
	}

	err := h.engine.Cancel(item.ID)
	if err == nil || !strings.Contains(err.Error(), "awaiting disposition") {
		t.Fatalf("cancel of a disposition park = %v, want a refusal naming the disposition verbs", err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonDisposition)
}
