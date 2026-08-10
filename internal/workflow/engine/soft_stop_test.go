package engine

import (
	"strings"
	"testing"

	"agent-overflow/internal/workflow/def"
)

// campaignWorkflow is the shape a soft stop exists for: one wave of work, then a
// self-call that starts the next one. `max_depth` is generous so the recursion
// is bounded by the stop rather than by the bound.
func campaignWorkflow() def.Workflow {
	return recursiveWorkflow(8)
}

func (h *testHarness) softStopArmedOnRow(t *testing.T, itemID string) bool {
	t.Helper()
	item, err := h.store.GetWorkItem(itemID)
	if err != nil {
		t.Fatal(err)
	}
	return item.SoftStop
}

// The flag is checked at the call boundary and nowhere else: arming it while a
// wave's own phase is running leaves that phase alone, and the wave finishes
// before anything stops.
func TestSoftStopParksAtTheCallBoundaryInsteadOfCalling(t *testing.T) {
	h := newCallHarness(t, map[string]def.Workflow{"recurse": campaignWorkflow()}, nil)
	if err := h.engine.StartItem(testItem("root", "project", "recurse", 0)); err != nil {
		t.Fatal(err)
	}
	// Armed mid-wave, with the work phase's turn still in flight.
	if err := h.engine.SetSoftStop("root", true); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "root", StateRunning, "")
	if !h.softStopArmedOnRow(t, "root") {
		t.Fatal("soft stop did not persist on the root row")
	}

	// The wave runs to completion — nothing was interrupted.
	h.runner.complete(t, "root", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	requireItemState(t, h.store, "root", StateNeedsHuman, ReasonCheckpoint)
	children, err := h.store.ListWorkItemChildren("root")
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 0 {
		t.Fatalf("the boundary fired but still called: %+v", children)
	}
	// The park explains itself on the attempt, because no turn ran to author an
	// envelope and nothing else records which call was skipped.
	requireParkCause(t, h.phaseAttempt(t, "root", "again", 1),
		"requested checkpoint", `phase "again"`)
	// The boundary consumed the request: the row no longer claims a pending stop.
	if h.softStopArmedOnRow(t, "root") {
		t.Fatal("the fired boundary left the request armed; resume would re-park forever")
	}
}

// Resuming takes the call edge the park skipped. It is the whole point of
// parking at a boundary rather than cancelling: the campaign continues.
func TestSoftStopResumeTakesTheSkippedCall(t *testing.T) {
	h := newCallHarness(t, map[string]def.Workflow{"recurse": campaignWorkflow()}, nil)
	if err := h.engine.StartItem(testItem("root", "project", "recurse", 0)); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.SetSoftStop("root", true); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, "root", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "root", StateNeedsHuman, ReasonCheckpoint)

	if err := h.engine.ResumeItem("root"); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "root", StateRunning, "")
	children, err := h.store.ListWorkItemChildren("root")
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 {
		t.Fatalf("resume after a checkpoint = %d children, want the skipped call taken", len(children))
	}
	if children[0].CallDepth != 1 {
		t.Fatalf("resumed call child depth = %d, want 1", children[0].CallDepth)
	}
}

// Clearing before the boundary is reached is a full undo: the call is made as
// though nothing had been asked for.
func TestSoftStopClearedBeforeTheBoundaryDoesNotPark(t *testing.T) {
	h := newCallHarness(t, map[string]def.Workflow{"recurse": campaignWorkflow()}, nil)
	if err := h.engine.StartItem(testItem("root", "project", "recurse", 0)); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.SetSoftStop("root", true); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.SetSoftStop("root", false); err != nil {
		t.Fatal(err)
	}
	if h.softStopArmedOnRow(t, "root") {
		t.Fatal("clearing did not disarm the row")
	}
	h.runner.complete(t, "root", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "root", StateRunning, "")
	if children, err := h.store.ListWorkItemChildren("root"); err != nil {
		t.Fatal(err)
	} else if len(children) != 1 {
		t.Fatal("a cleared request must not stop the call")
	}
}

// Every transition of the flag is legal and lands on the row, including the
// no-op ones. Arming twice is one armed row, not two requests; clearing a run
// that was never armed succeeds rather than refusing something a human would
// reasonably re-issue after a dropped response.
func TestSoftStopTransitionsAreIdempotent(t *testing.T) {
	h := newCallHarness(t, map[string]def.Workflow{"recurse": campaignWorkflow()}, nil)
	if err := h.engine.StartItem(testItem("root", "project", "recurse", 0)); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		armed bool
		want  bool
	}{
		{false, false}, // clear before any arm
		{true, true},
		{true, true}, // arm twice
		{false, false},
		{false, false}, // clear twice
		{true, true},   // and back on again
	} {
		if err := h.engine.SetSoftStop("root", step.armed); err != nil {
			t.Fatalf("set soft stop %v: %v", step.armed, err)
		}
		if got := h.softStopArmedOnRow(t, "root"); got != step.want {
			t.Fatalf("after set %v the row says %v, want %v", step.armed, got, step.want)
		}
	}
}

// A descendant's boundary honours the ROOT's request. This is the campaign's
// real shape: the human watches wave one and the stop has to reach whatever
// wave is actually running when it is armed.
func TestSoftStopOnTheRootStopsADescendantsBoundary(t *testing.T) {
	h := newCallHarness(t, map[string]def.Workflow{"recurse": campaignWorkflow()}, nil)
	if err := h.engine.StartItem(testItem("root", "project", "recurse", 0)); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, "root", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	wave2 := h.callChild(t, "root", "again", 1)

	// Armed against the run a human is watching, while wave two is the one
	// doing the work.
	if err := h.engine.SetSoftStop("root", true); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, wave2.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	// The run parked is the one whose boundary fired, not the root — which is
	// still running, waiting on the descendant exactly as it was.
	requireItemState(t, h.store, wave2.ID, StateNeedsHuman, ReasonCheckpoint)
	requireItemState(t, h.store, "root", StateRunning, "")
	if children, err := h.store.ListWorkItemChildren(wave2.ID); err != nil {
		t.Fatal(err)
	} else if len(children) != 0 {
		t.Fatal("the descendant's boundary fired but still called")
	}
	if h.softStopArmedOnRow(t, "root") {
		t.Fatal("a descendant's boundary must consume the root's request")
	}
	// A descendant's park must name the root that asked.
	requireParkCause(t, h.phaseAttempt(t, wave2.ID, "again", 1), "root")
}

// A soft stop is a tree-level request, so it is set on the tree's root. A child
// is refused with the run to set it on instead, exactly as pause is.
func TestSoftStopRefusesACalledRun(t *testing.T) {
	h := newCallHarness(t, map[string]def.Workflow{"recurse": campaignWorkflow()}, nil)
	if err := h.engine.StartItem(testItem("root", "project", "recurse", 0)); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, "root", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	child := h.callChild(t, "root", "again", 1)

	err := h.engine.SetSoftStop(child.ID, true)
	if err == nil || !strings.Contains(err.Error(), "set it on the run that called it") {
		t.Fatalf("soft stop of a called run = %v, want a refusal naming the root", err)
	}
	if h.softStopArmedOnRow(t, child.ID) {
		t.Fatal("a refused request must write nothing")
	}
}

// Arming a run that has stopped has nothing to stop. Disarming one stays legal,
// so a request can always be withdrawn however the run ended up.
func TestSoftStopRefusesArmingARunThatIsNotGoing(t *testing.T) {
	h := newCallHarness(t, map[string]def.Workflow{"recurse": campaignWorkflow()}, nil)
	if err := h.engine.StartItem(testItem("root", "project", "recurse", 0)); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Cancel("root"); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "root", StateCancelled, ReasonInterrupted)

	err := h.engine.SetSoftStop("root", true)
	if err == nil || !strings.Contains(err.Error(), "still going") {
		t.Fatalf("arming a cancelled run = %v, want a refusal", err)
	}
	if err := h.engine.SetSoftStop("root", false); err != nil {
		t.Fatalf("clearing a cancelled run = %v, want it to succeed", err)
	}
}

// A run with no call edge never reaches the check, so the request is inert
// rather than firing at some other boundary. The run finishes normally and the
// row still says what was asked for.
func TestSoftStopNeverFiresOnARunWithNoCallEdge(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"plain": {ID: "plain", Phases: []def.Phase{agentPhase("work", nil, []def.Route{{To: "done"}})}},
	}, []string{"project"}, nil)
	if err := h.engine.StartItem(testItem("root", "project", "plain", 0)); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.SetSoftStop("root", true); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, "root", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "root", StateDone, "")
	if !h.softStopArmedOnRow(t, "root") {
		t.Fatal("nothing consumed the request, so the row must still carry it")
	}
}
