package engine

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

// A join that fails is a unit of the attempt failing, and the incident that
// makes it matter is the campaign: every work unit of the wave is a whole child
// run, already finished and paid for. Re-entering the phase re-expands the wave
// and runs those children again, so every verb a human reaches for after a
// failed join has to repair the attempt in place instead.

// startCampaignWave drives the campaign into its wave with both call units
// resting on live children, and returns the run plus the children's ids in unit
// order.
func startCampaignWave(t *testing.T, h *testHarness) (string, []string) {
	t.Helper()
	parent := startCampaign(t, h)
	children := make([]string, 0, 2)
	for index := 0; index < 2; index++ {
		children = append(children, h.unitCallChild(t, parent, "wave", 1, fmt.Sprintf("wave-unit-%d", index)).ID)
	}
	return parent, children
}

// parkCampaignOnFailedJoin completes both called runs, lets the join start over
// their results, and fails it. That is the incident's exact resting shape: two
// done call units, a failed join, and a run needing a human.
func parkCampaignOnFailedJoin(t *testing.T, h *testHarness, threadID string) (string, []string) {
	t.Helper()
	parent, children := startCampaignWave(t, h)
	for _, child := range children {
		h.runner.complete(t, child, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if threadID != "" {
		// The app runner stamps the join's thread onto the phase attempt row as
		// soon as the join has one; every phase-level continuation reads it there.
		seedThread(t, h.store, threadID)
		if err := h.store.AttachWorkItemPhaseRun(parent, "wave", 1, threadID, ""); err != nil {
			t.Fatal(err)
		}
	}
	h.runner.completeRun(t, unitKey(parent, "wave", 1, "wave-join"),
		Outcome{Kind: OutcomeExecutionFailure, Envelope: failureEnvelope("join blew up")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, parent, StateNeedsHuman, ReasonUnitFailed)
	return parent, children
}

// childIDs is every run the tree has spawned, so a repair that re-executed a
// finished call unit is visible as a child that did not exist before it.
func (h *testHarness) childIDs(t *testing.T, parentID string) []string {
	t.Helper()
	children, err := h.store.ListWorkItemChildren(parentID)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(children))
	for _, child := range children {
		ids = append(ids, child.ID)
	}
	return ids
}

func TestFanOutJoinFailureParksUnitFailed(t *testing.T) {
	h := newFanOutHarness(t, 1)
	item := startFanOut(t, h, "fan")
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-0"),
		Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope("a")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-join"),
		Outcome{Kind: OutcomeExecutionFailure, Envelope: failureEnvelope("join blew up")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	// unit-failed, not agent-error: the join is a unit of the attempt, and this
	// is the reason every unit-repair verb acts on.
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonUnitFailed)
	h.requireUnitStatuses(t, item, "work", 1, map[string]string{
		"work-unit-0": store.WorkItemUnitDone,
		"work-join":   store.WorkItemUnitFailed,
	})
	h.requireNoHeldResources(t)
}

// The join is nameable by `run retry-unit`, and retrying it re-runs the join
// alone: the wave's finished units keep their results and their children are
// never re-executed.
func TestRetryUnitRerunsAFailedJoinOverTheSameChildren(t *testing.T) {
	h := newCallHarness(t, callUnitWorkflows(2), nil)
	parent, children := parkCampaignOnFailedJoin(t, h, "")

	if err := h.engine.RetryUnit(parent, "wave-join", "consolidate them again"); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, parent, StateRunning, "")
	h.requireUnitStatuses(t, parent, "wave", 1, map[string]string{
		"wave-unit-0": store.WorkItemUnitDone,
		"wave-unit-1": store.WorkItemUnitDone,
		"wave-join":   store.WorkItemUnitRunning,
	})
	if got := h.childIDs(t, parent); !reflect.DeepEqual(got, children) {
		t.Fatalf("children after the join retry = %v, want the two the wave already ran (%v)", got, children)
	}
	rerun := h.runner.startFor(t, unitKey(parent, "wave", 1, "wave-join"))
	if rerun.Feedback == nil || rerun.Feedback.Note != "consolidate them again" {
		t.Fatalf("join feedback = %+v, want the human's note", rerun.Feedback)
	}
	if rerun.UnitAttempt != 2 {
		t.Fatalf("join try = %d, want a fresh try so its narrative is not overwritten", rerun.UnitAttempt)
	}
	h.runner.completeRun(t, unitKey(parent, "wave", 1, "wave-join"), Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, parent, StateDone, "")
	h.requireNoHeldResources(t)
}

// `run retry-failed-units` is the verb an agent reaches for first, and a failed
// join has to be in its set: it is the only failed unit there is, so a set that
// excluded it would leave the run with no repair at all.
func TestRetryFailedUnitsRerunsOnlyTheFailedJoin(t *testing.T) {
	h := newCallHarness(t, callUnitWorkflows(2), nil)
	parent, children := parkCampaignOnFailedJoin(t, h, "")

	if err := h.engine.RetryFailedUnits(parent, "the limit reset"); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, parent, StateRunning, "")
	h.requireUnitStatuses(t, parent, "wave", 1, map[string]string{
		"wave-unit-0": store.WorkItemUnitDone,
		"wave-unit-1": store.WorkItemUnitDone,
		"wave-join":   store.WorkItemUnitRunning,
	})
	if got := h.childIDs(t, parent); !reflect.DeepEqual(got, children) {
		t.Fatalf("children after retry-failed-units = %v, want the two the wave already ran (%v)", got, children)
	}
	// The wave's units are done, so the only unit the runner starts is the join.
	if got := h.runner.startedUnitIDs(); !reflect.DeepEqual(got, []string{"wave-join", "wave-join"}) {
		t.Fatalf("started units = %v, want the join and nothing else", got)
	}
	rerun := h.runner.startFor(t, unitKey(parent, "wave", 1, "wave-join"))
	if rerun.Feedback == nil || rerun.Feedback.Note != "the limit reset" {
		t.Fatalf("join feedback = %+v, want the retry-all's note", rerun.Feedback)
	}
}

// The generic verb is the dangerous one: an agent that types `run resume` must
// get the preserving repair, not a fresh wave. It continues the join on the
// session the attempt parked on, exactly as Answer does.
func TestResumeOnAFailedJoinContinuesTheAttempt(t *testing.T) {
	h := newCallHarness(t, callUnitWorkflows(2), nil)
	parent, children := parkCampaignOnFailedJoin(t, h, "join-thread")

	if err := h.engine.Resume(parent, "", false); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, parent, StateRunning, "")
	if got := h.childIDs(t, parent); !reflect.DeepEqual(got, children) {
		t.Fatalf("children after a bare resume = %v, want the two the wave already ran (%v)", got, children)
	}
	phases, err := h.store.ListWorkItemPhases(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range phases {
		if phase.PhaseID == "wave" && phase.Attempt != 1 {
			t.Fatalf("phase attempts = %+v, want the parked attempt reopened rather than replaced", phases)
		}
	}
	h.requireUnitStatuses(t, parent, "wave", 1, map[string]string{
		"wave-unit-0": store.WorkItemUnitDone,
		"wave-unit-1": store.WorkItemUnitDone,
		"wave-join":   store.WorkItemUnitRunning,
	})
	rerun := h.runner.startFor(t, unitKey(parent, "wave", 1, "wave-join"))
	if rerun.Launch.ThreadID() != "join-thread" {
		t.Fatalf("join prior thread = %q, want the session the attempt parked on", rerun.Launch.ThreadID())
	}
	if rerun.Feedback == nil || !strings.Contains(rerun.Feedback.Note, "unit of the fan-out failed") {
		t.Fatalf("join feedback = %+v, want the resume note naming why it parked", rerun.Feedback)
	}
}

// Naming the parked phase is the explicit start-over, and it must still work:
// the wave expands again and its call units run new children. That is the whole
// point of `--phase`, and the reason bare resume no longer implies it.
func TestResumeWithTheParkedPhaseReExpandsTheWave(t *testing.T) {
	h := newCallHarness(t, callUnitWorkflows(2), nil)
	parent, children := parkCampaignOnFailedJoin(t, h, "join-thread")

	if err := h.engine.Resume(parent, "wave", false); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, parent, StateRunning, "")
	h.requireUnitStatuses(t, parent, "wave", 2, map[string]string{
		"wave-unit-0": store.WorkItemUnitRunning,
		"wave-unit-1": store.WorkItemUnitRunning,
		"wave-join":   store.WorkItemUnitPending,
	})
	fresh := h.childIDs(t, parent)
	if len(fresh) != len(children)+2 {
		t.Fatalf("children after a targeted resume = %v, want two more than the %v the first wave ran", fresh, children)
	}
	// The first attempt's units keep their record: the fresh attempt is a second
	// wave beside them, not a rewrite of the first.
	h.requireUnitStatuses(t, parent, "wave", 1, map[string]string{
		"wave-unit-0": store.WorkItemUnitDone,
		"wave-unit-1": store.WorkItemUnitDone,
		"wave-join":   store.WorkItemUnitFailed,
	})
}

// Accepting a join's absence is meaningless — nothing would consolidate the
// units and the phase would have no envelope — so drop still refuses it, with a
// message that names what to do instead.
func TestDropRefusesAFailedJoin(t *testing.T) {
	h := newCallHarness(t, callUnitWorkflows(2), nil)
	parent, _ := parkCampaignOnFailedJoin(t, h, "")

	err := h.engine.DropUnit(parent, "wave-join", "")
	if err == nil || !strings.Contains(err.Error(), "absence cannot be accepted") {
		t.Fatalf("dropping the join = %v, want a refusal naming why", err)
	}
	requireItemState(t, h.store, parent, StateNeedsHuman, ReasonUnitFailed)
	h.requireUnitStatuses(t, parent, "wave", 1, map[string]string{
		"wave-unit-0": store.WorkItemUnitDone,
		"wave-unit-1": store.WorkItemUnitDone,
		"wave-join":   store.WorkItemUnitFailed,
	})
}

// The same preservation for the ordinary unit failure the retry verbs already
// covered: a bare resume reopens what is blocking and relaunches it, and the
// unit that finished keeps its result rather than running a second time.
func TestResumeOnAFailedWorkUnitReopensOnlyThatUnit(t *testing.T) {
	h := newFanOutHarness(t, 2)
	item := parkOneFailedUnit(t, h)

	if err := h.engine.Resume(item, "", false); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateRunning, "")
	h.requireUnitStatuses(t, item, "work", 1, map[string]string{
		"work-unit-0": store.WorkItemUnitRunning,
		"work-unit-1": store.WorkItemUnitDone,
		"work-join":   store.WorkItemUnitPending,
	})
	if got := h.runner.startedUnitIDs(); !reflect.DeepEqual(got, []string{
		"work-unit-0", "work-unit-1", "work-unit-0",
	}) {
		t.Fatalf("started units = %v, want only the failed unit relaunched", got)
	}
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-0"),
		Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope("a")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := h.runner.startedUnitIDs(); got[len(got)-1] != "work-join" {
		t.Fatalf("started units = %v, want the join once the repaired unit finished", got)
	}
}
