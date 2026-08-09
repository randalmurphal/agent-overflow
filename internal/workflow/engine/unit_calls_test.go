package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// callUnitFanOutPhase is the campaign shape: a fan-out whose work units are all
// call edges, joined by an ordinary agent unit. The join stays an agent because
// its envelope is the phase's and validation refuses a call join.
func callUnitFanOutPhase(id, target string, width int, routes []def.Route) def.Phase {
	phase := def.Phase{ID: id, Shape: def.ShapeFanOut, Gate: def.Gate{Routes: routes}}
	for index := 0; index < width; index++ {
		phase.FanOut = append(phase.FanOut, def.Unit{
			ID:   fmt.Sprintf("%s-unit-%d", id, index),
			Call: target, Args: map[string]string{"seed": "prepare.ok"},
		})
	}
	phase.Join = &def.Unit{ID: id + "-join", Provider: testProvider, Model: "test-model", Prompt: "join.md"}
	return phase
}

// callUnitWorkflow prepares a value, fans out into `width` called sub-workflows,
// and joins.
func callUnitWorkflow(target string, width int) def.Workflow {
	return def.Workflow{ID: "campaign", Phases: []def.Phase{
		agentPhase("prepare", nil, []def.Route{{To: "wave"}}),
		callUnitFanOutPhase("wave", target, width, []def.Route{{To: "done"}}),
	}}
}

func callUnitWorkflows(width int) map[string]def.Workflow {
	return map[string]def.Workflow{
		"campaign": callUnitWorkflow("child", width),
		"child":    childWorkflow("child"),
	}
}

// failingCallUnitWorkflows swaps in a child whose own gate routes it to
// `failed`, which is the only way a run reaches StateFailed.
func failingCallUnitWorkflows(width int) map[string]def.Workflow {
	workflows := callUnitWorkflows(width)
	workflows["child"] = childWorkflow("child", agentPhase("work", nil, []def.Route{
		{When: &def.Predicate{Eq: &def.Comparison{Ref: "work.ok", Value: true}}, To: "done"},
		{To: "failed"},
	}))
	return workflows
}

// startCampaign admits the campaign run and drives it into its fan-out, where
// every call unit has started a child and is resting.
func startCampaign(t *testing.T, h *testHarness) string {
	t.Helper()
	item := testItem("campaign", "project", "campaign", 0)
	item.WorktreePath = "/tmp/campaign-worktree"
	item.Branch = "ao-campaign"
	item.BaseBranch = "main"
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	return item.ID
}

func (h *testHarness) unitCallChild(t *testing.T, parentID, phaseID string, attempt int, unitID string) store.WorkItem {
	t.Helper()
	children, err := h.store.ListWorkItemUnitCallChildren(parentID, phaseID, attempt, unitID)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 {
		t.Fatalf("unit call children of %s/%s/%d/%s = %d, want exactly one",
			parentID, phaseID, attempt, unitID, len(children))
	}
	return children[0]
}

// restartEngine simulates a crash: the engine goes away without persisting
// anything, and a new one comes up over the same store and rebuilds from the
// rows alone. The runner is fresh too — the process that held those sessions is
// gone — which is what makes "which units survive a restart" observable.
func restartEngine(t *testing.T, h *testHarness) {
	t.Helper()
	if err := h.engine.Close(); err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	engine, err := New(h.store, runner, h.emitter, h.definitions, h.profiles, h.spend, Config{})
	if err != nil {
		t.Fatal(err)
	}
	engine.now = h.engine.now
	if err := engine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.engine, h.runner = engine, runner
}

func TestCallUnitStartsChildAndRests(t *testing.T) {
	h := newCallHarness(t, callUnitWorkflows(2), nil)
	parent := startCampaign(t, h)

	h.requireUnitStatuses(t, parent, "wave", 1, map[string]string{
		"wave-unit-0": "running", "wave-unit-1": "running", "wave-join": "pending",
	})
	for index := 0; index < 2; index++ {
		unitID := fmt.Sprintf("wave-unit-%d", index)
		child := h.unitCallChild(t, parent, "wave", 1, unitID)
		if child.ParentItemID != parent || child.ParentPhaseID != "wave" ||
			child.ParentUnitID != unitID || child.ParentAttempt != 1 || child.CallDepth != 1 {
			t.Fatalf("child linkage = %+v", child)
		}
		if child.Source != WorkItemSourceCall {
			t.Fatalf("child provenance = %q", child.Source)
		}
		if child.WorkflowID != "child" || child.State != string(StateRunning) {
			t.Fatalf("child run = %+v", child)
		}
		// Isolation is introduced by fan-out (§9): the engine stamps no workspace,
		// so the runner resolves the *unit's* sub-worktree through this linkage
		// rather than putting every unit's child in the caller's one checkout.
		if child.WorktreePath != "" || child.Branch != "" || child.BaseBranch != "" {
			t.Fatalf("unit call child inherited the caller's workspace: %+v", child)
		}
		if string(child.Seeds) != `{"seed":true}` {
			t.Fatalf("child seeds = %s", child.Seeds)
		}
	}
	// The units run no turn of their own, so the only runner starts under this
	// attempt belong to the children's phases.
	for _, start := range h.runner.started() {
		if start.Key.ItemID == parent && start.Key.PhaseID == "wave" {
			t.Fatalf("a call unit started a runner: %+v", start.Key)
		}
	}
	// A call unit takes no provider capacity; each child's own phase takes its own,
	// so the only holder is the provider bound and it is held exactly twice.
	if got := h.engine.holders[resourceKey{projectID: "project", name: ProviderResource(testProvider)}]; got != 2 {
		t.Fatalf("provider holders = %d, want one per child phase (all: %v)", got, h.engine.holders)
	}
	if len(h.engine.holders) != 1 {
		t.Fatalf("a call unit acquired resources of its own: %v", h.engine.holders)
	}
	if h.definitions.callResolveCount("child") != 2 {
		t.Fatalf("call-time resolutions = %d, want one per unit", h.definitions.callResolveCount("child"))
	}
}

func TestCallUnitSourceRefNamesTheUnit(t *testing.T) {
	h := newCallHarness(t, callUnitWorkflows(1), nil)
	parent := startCampaign(t, h)
	child := h.unitCallChild(t, parent, "wave", 1, "wave-unit-0")
	if child.SourceRef != "campaign/wave[wave-unit-0]/1" {
		t.Fatalf("child source ref = %q", child.SourceRef)
	}
}

func TestCallUnitChildDoneBecomesTheUnitEnvelope(t *testing.T) {
	h := newCallHarness(t, callUnitWorkflows(1), nil)
	parent := startCampaign(t, h)
	child := h.unitCallChild(t, parent, "wave", 1, "wave-unit-0")

	h.runner.complete(t, child.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	requireItemState(t, h.store, child.ID, StateDone, "")
	rows, err := h.store.ListWorkItemPhaseUnits(parent, "wave", 1)
	if err != nil {
		t.Fatal(err)
	}
	var unit store.WorkItemUnit
	for _, row := range rows {
		if row.UnitID == "wave-unit-0" {
			unit = row
		}
	}
	if unit.Status != store.WorkItemUnitDone {
		t.Fatalf("call unit after its child completed = %+v", unit)
	}
	envelope := decodeEnvelope(t, unit.Envelope)
	if envelope.Status != "done" || envelope.Outputs["verdict"] != true {
		t.Fatalf("unit envelope = %+v, want the child workflow's declared outputs", envelope)
	}
	// Every unit rests done, so the join runs — the fan-out advanced exactly as
	// it does when an agent unit finishes.
	starts := h.runner.started()
	if last := starts[len(starts)-1].Key; last.ItemID != parent || last.UnitID != "wave-join" {
		t.Fatalf("join did not start after the last call unit settled: %+v", starts)
	}
}

func TestCallUnitChildFailureParksTheAttemptUnitFailed(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		workflows map[string]def.Workflow
		settle    func(*testing.T, *testHarness, store.WorkItem)
		contains  string
	}{
		{
			name: "failed child", workflows: failingCallUnitWorkflows(2),
			settle: func(t *testing.T, h *testHarness, child store.WorkItem) {
				h.runner.complete(t, child.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(false)})
			},
			contains: "failed",
		},
		{
			name: "cancelled child", workflows: callUnitWorkflows(2),
			settle: func(t *testing.T, h *testHarness, child store.WorkItem) {
				if err := h.engine.Cancel(child.ID); err != nil {
					t.Fatal(err)
				}
			},
			contains: "cancelled",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			h := newCallHarness(t, testCase.workflows, nil)
			parent := startCampaign(t, h)
			child := h.unitCallChild(t, parent, "wave", 1, "wave-unit-0")

			testCase.settle(t, h, child)
			if err := h.engine.Sync(); err != nil {
				t.Fatal(err)
			}

			// The sibling is untouched: its work is durable, and a failure never
			// interrupts an in-flight unit.
			h.requireUnitStatuses(t, parent, "wave", 1, map[string]string{
				"wave-unit-0": "failed", "wave-unit-1": "running", "wave-join": "pending",
			})
			requireItemState(t, h.store, parent, StateRunning, "")

			sibling := h.unitCallChild(t, parent, "wave", 1, "wave-unit-1")
			h.runner.complete(t, sibling.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
			if err := h.engine.Sync(); err != nil {
				t.Fatal(err)
			}
			requireItemState(t, h.store, parent, StateNeedsHuman, ReasonUnitFailed)

			rows, err := h.store.ListWorkItemPhaseUnits(parent, "wave", 1)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if row.UnitID != "wave-unit-0" {
					continue
				}
				if !strings.Contains(row.Feedback, child.ID) || !strings.Contains(row.Feedback, testCase.contains) {
					t.Fatalf("failed unit feedback = %q, want it to name the child and its outcome", row.Feedback)
				}
			}
		})
	}
}

// A child that parks needs a human but is not terminal, so the unit keeps
// waiting and the attempt is still live.
func TestCallUnitWaitsThroughAChildPark(t *testing.T) {
	h := newCallHarness(t, callUnitWorkflows(1), nil)
	parent := startCampaign(t, h)
	child := h.unitCallChild(t, parent, "wave", 1, "wave-unit-0")

	h.runner.complete(t, child.ID, Outcome{Kind: OutcomeStuck, Envelope: stuckEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	requireItemState(t, h.store, child.ID, StateNeedsHuman, ReasonStuck)
	requireItemState(t, h.store, parent, StateRunning, "")
	h.requireUnitStatuses(t, parent, "wave", 1, map[string]string{
		"wave-unit-0": "running", "wave-join": "pending",
	})
}

// Cancelling the campaign brings its units' children down first: a descendant
// whose caller has left the phase can never be consumed by anything.
func TestCancelCascadesThroughUnitCallChildren(t *testing.T) {
	h := newCallHarness(t, callUnitWorkflows(2), nil)
	parent := startCampaign(t, h)
	first := h.unitCallChild(t, parent, "wave", 1, "wave-unit-0")
	second := h.unitCallChild(t, parent, "wave", 1, "wave-unit-1")

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

// Pause is the one teardown that keeps the children it is waiting on, and
// resume re-links each unit to the child it left running.
func TestPauseAndResumeRetainUnitCallChildren(t *testing.T) {
	h := newCallHarness(t, callUnitWorkflows(1), nil)
	parent := startCampaign(t, h)
	child := h.unitCallChild(t, parent, "wave", 1, "wave-unit-0")

	if err := h.engine.PauseItem(parent); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, parent, StateNeedsHuman, ReasonPaused)
	requireItemState(t, h.store, child.ID, StateNeedsHuman, ReasonPaused)
	// The unit is not swept: its work is a child run that is still there.
	h.requireUnitStatuses(t, parent, "wave", 1, map[string]string{
		"wave-unit-0": "running", "wave-join": "pending",
	})

	if err := h.engine.ResumeItem(parent); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, parent, StateRunning, "")
	requireItemState(t, h.store, child.ID, StateRunning, "")
	// Resuming re-linked the existing child rather than starting a second one.
	if h.unitCallChild(t, parent, "wave", 1, "wave-unit-0").ID != child.ID {
		t.Fatal("resume replaced the child the unit was waiting on")
	}

	h.runner.complete(t, child.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.requireUnitStatuses(t, parent, "wave", 1, map[string]string{
		"wave-unit-0": "done", "wave-join": "running",
	})
}

// The point of the rebuild path: a restart mid-campaign must put the resting
// units back on the children that are still alive, not abandon a wave of
// sub-workflows and park.
func TestRebuildAdoptsRestingUnitCalls(t *testing.T) {
	h := newCallHarness(t, callUnitWorkflows(2), nil)
	parent := startCampaign(t, h)
	first := h.unitCallChild(t, parent, "wave", 1, "wave-unit-0")
	second := h.unitCallChild(t, parent, "wave", 1, "wave-unit-1")

	restartEngine(t, h)

	requireItemState(t, h.store, parent, StateRunning, "")
	// Both children were interrupted mid-turn and park as themselves; the parent
	// keeps waiting on them rather than parking too.
	requireItemState(t, h.store, first.ID, StateNeedsHuman, ReasonInterrupted)
	requireItemState(t, h.store, second.ID, StateNeedsHuman, ReasonInterrupted)
	h.requireUnitStatuses(t, parent, "wave", 1, map[string]string{
		"wave-unit-0": "running", "wave-unit-1": "running", "wave-join": "pending",
	})
	// No second invocation: the rebuild adopted the children rather than
	// re-calling from the persisted attempt.
	if h.definitions.callResolveCount("child") != 2 {
		t.Fatalf("call resolutions after rebuild = %d, want the two the run already made",
			h.definitions.callResolveCount("child"))
	}

	// Resuming each child settles the units it belongs to, and the campaign
	// proceeds to its join.
	for _, child := range []store.WorkItem{first, second} {
		if err := h.engine.ResumeItem(child.ID); err != nil {
			t.Fatal(err)
		}
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
		h.runner.complete(t, child.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	h.requireUnitStatuses(t, parent, "wave", 1, map[string]string{
		"wave-unit-0": "done", "wave-unit-1": "done", "wave-join": "running",
	})
}

// A crash between persisting the unit running and creating its child leaves a
// unit with no child. That gap re-invokes in place — no new attempt row, so the
// phase history and every loop count derived from it stay honest.
func TestRebuildReinvokesACallUnitWithNoChild(t *testing.T) {
	workflows := callUnitWorkflows(1)
	snapshot, err := json.Marshal(Snapshot{Workflow: workflows["campaign"]})
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(PhaseInput{Vars: map[string]any{"prepare.ok": true}})
	if err != nil {
		t.Fatal(err)
	}
	h := newCallHarness(t, workflows, func(database *store.Store) {
		item := testItem("campaign", "project", "campaign", 0)
		if err := database.CreateWorkItem(item); err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateWorkItemRunStart(item.ID, snapshot, "", "", "", 20); err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateWorkItemState(item.ID, string(StateRunning), "", 0); err != nil {
			t.Fatal(err)
		}
		if err := database.CreateWorkItemPhase(store.WorkItemPhase{
			ItemID: item.ID, PhaseID: "wave", Attempt: 1,
			InputEnvelope: input, Status: "running", StartedAt: 21,
		}); err != nil {
			t.Fatal(err)
		}
		// The unit row is written running before the child exists, so this is
		// exactly the state a crash in that window leaves behind.
		if err := database.CreateWorkItemUnits([]store.WorkItemUnit{
			{
				ItemID: item.ID, PhaseID: "wave", Attempt: 1, UnitID: "wave-unit-0",
				UnitIndex: 0, Kind: store.WorkItemUnitKindUnit,
				Status: store.WorkItemUnitRunning, UnitAttempt: 1, StartedAt: 22,
			},
			{
				ItemID: item.ID, PhaseID: "wave", Attempt: 1, UnitID: "wave-join",
				UnitIndex: 1, Kind: store.WorkItemUnitKindJoin, Provider: testProvider,
				Status: store.WorkItemUnitPending, UnitAttempt: 1,
			},
		}); err != nil {
			t.Fatal(err)
		}
	})

	child := h.unitCallChild(t, "campaign", "wave", 1, "wave-unit-0")
	requireItemState(t, h.store, child.ID, StateRunning, "")
	requireItemState(t, h.store, "campaign", StateRunning, "")
	// Re-invoked in place: no new attempt row, so the phase history and every
	// loop count derived from it stay honest.
	phases, err := h.store.ListWorkItemPhases("campaign")
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0].Attempt != 1 || phases[0].Status != "running" {
		t.Fatalf("re-invocation did not continue the persisted attempt: %+v", phases)
	}
}

// A campaign whose units call the campaign itself is the shape the depth bounds
// exist for. The engine's absolute ceiling stops it wherever the author's
// max_depth does not.
func TestUnitCallDepthIsBounded(t *testing.T) {
	recursive := callUnitWorkflow("campaign", 1)
	h := newCallHarness(t, map[string]def.Workflow{"campaign": recursive}, nil)
	item := testItem("campaign", "project", "campaign", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	// Drive the chain: each generation's prepare completes, its unit calls the
	// campaign again, until a depth bound refuses.
	current := item.ID
	for depth := 0; depth <= MaxCallDepth+1; depth++ {
		h.runner.complete(t, current, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
		children, err := h.store.ListWorkItemUnitCallChildren(current, "wave", 1, "wave-unit-0")
		if err != nil {
			t.Fatal(err)
		}
		if len(children) == 0 {
			break
		}
		current = children[0].ID
	}
	deepest, err := h.store.GetWorkItem(current)
	if err != nil {
		t.Fatal(err)
	}
	if deepest.State != string(StateNeedsHuman) || deepest.Reason != string(ReasonWiringError) {
		t.Fatalf("deepest run = %+v, want a wiring-error park at the depth ceiling", deepest)
	}
	if deepest.CallDepth < MaxCallDepth {
		t.Fatalf("recursion stopped at depth %d, before the absolute ceiling %d", deepest.CallDepth, MaxCallDepth)
	}
	attempt := h.phaseAttempt(t, current, "wave", 1)
	if !strings.Contains(string(attempt.OutputEnvelope), "campaign") {
		t.Fatalf("depth refusal did not carry the call chain: %s", attempt.OutputEnvelope)
	}
}

// The declared bound is the author's much tighter one, and a unit edge carries
// it exactly as a phase edge does.
func TestUnitCallHonoursDeclaredMaxDepth(t *testing.T) {
	recursive := callUnitWorkflow("campaign", 1)
	recursive.Phases[1].FanOut[0].MaxDepth = 2
	h := newCallHarness(t, map[string]def.Workflow{"campaign": recursive}, nil)
	item := testItem("campaign", "project", "campaign", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	current := item.ID
	depths := 0
	for {
		h.runner.complete(t, current, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
		children, err := h.store.ListWorkItemUnitCallChildren(current, "wave", 1, "wave-unit-0")
		if err != nil {
			t.Fatal(err)
		}
		if len(children) == 0 {
			break
		}
		current = children[0].ID
		depths++
		if depths > MaxCallDepth {
			t.Fatal("the declared max_depth never stopped the recursion")
		}
	}
	if depths != 2 {
		t.Fatalf("recursion made %d calls, want the declared max_depth of 2", depths)
	}
	requireItemState(t, h.store, current, StateNeedsHuman, ReasonWiringError)
}

// A unit whose arguments cannot be evaluated produced nothing runnable, so the
// attempt parks under the phase-level reason rather than recording a unit
// failure — the same rule a unit that cannot start takes.
func TestCallUnitWithUnresolvableArgsParksWiringError(t *testing.T) {
	workflows := callUnitWorkflows(1)
	campaign := workflows["campaign"]
	campaign.Phases[1].FanOut[0].Args = map[string]string{"seed": "prepare.nothing"}
	workflows["campaign"] = campaign
	h := newCallHarness(t, workflows, nil)
	parent := startCampaign(t, h)

	requireItemState(t, h.store, parent, StateNeedsHuman, ReasonWiringError)
	attempt := h.phaseAttempt(t, parent, "wave", 1)
	if !strings.Contains(string(attempt.OutputEnvelope), "prepare.nothing") {
		t.Fatalf("park envelope did not name the unresolvable argument: %s", attempt.OutputEnvelope)
	}
	// The edge is decided before the unit row moves, so there is no unit outcome
	// to record: the row is still pending rather than failed.
	h.requireUnitStatuses(t, parent, "wave", 1, map[string]string{
		"wave-unit-0": store.WorkItemUnitPending,
		"wave-join":   store.WorkItemUnitPending,
	})
	h.requireNoHeldResources(t)
}

// A unit call edge answers to the same optionality rule a phase call edge does,
// because both go through one implementation: an argument seeding an input the
// child declares optional is omitted rather than refused.
func TestCallUnitOmitsAnArgumentAnAbsentOptionalChildInputWouldSeed(t *testing.T) {
	workflows := callUnitWorkflows(1)
	campaign := workflows["campaign"]
	campaign.Phases[1].FanOut[0].Args["job-notes"] = "job-notes"
	workflows["campaign"] = campaign
	workflows["child"] = optionalNotesChild(true)
	h := newCallHarness(t, workflows, nil)
	parent := startCampaign(t, h)

	child := h.unitCallChild(t, parent, "wave", 1, "wave-unit-0")
	if string(child.Seeds) != `{"seed":true}` {
		t.Fatalf("child seeds = %s, want the absent optional argument omitted entirely", child.Seeds)
	}
	requireItemState(t, h.store, parent, StateRunning, "")
	requireItemState(t, h.store, child.ID, StateRunning, "")
}

func TestCallUnitParksWhenAnAbsentArgumentSeedsARequiredChildInput(t *testing.T) {
	workflows := callUnitWorkflows(1)
	campaign := workflows["campaign"]
	campaign.Phases[1].FanOut[0].Args["job-notes"] = "job-notes"
	workflows["campaign"] = campaign
	workflows["child"] = optionalNotesChild(false)
	h := newCallHarness(t, workflows, nil)
	parent := startCampaign(t, h)

	requireItemState(t, h.store, parent, StateNeedsHuman, ReasonWiringError)
	attempt := h.phaseAttempt(t, parent, "wave", 1)
	if !strings.Contains(string(attempt.OutputEnvelope), "job-notes (job-notes)") {
		t.Fatalf("park envelope did not name the unresolved argument: %s", attempt.OutputEnvelope)
	}
	h.requireUnitStatuses(t, parent, "wave", 1, map[string]string{
		"wave-unit-0": store.WorkItemUnitPending,
		"wave-join":   store.WorkItemUnitPending,
	})
}

// The unit-scoped repair is the same one an agent unit gets: a retried call unit
// makes a *fresh* child rather than resuming the one that failed.
func TestRetryCallUnitMakesANewChild(t *testing.T) {
	h := newCallHarness(t, failingCallUnitWorkflows(1), nil)
	parent := startCampaign(t, h)
	child := h.unitCallChild(t, parent, "wave", 1, "wave-unit-0")
	h.runner.complete(t, child.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(false)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, parent, StateNeedsHuman, ReasonUnitFailed)

	if err := h.engine.RetryUnit(parent, "wave-unit-0", "retry"); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	requireItemState(t, h.store, parent, StateRunning, "")
	children, err := h.store.ListWorkItemUnitCallChildren(parent, "wave", 1, "wave-unit-0")
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 {
		t.Fatalf("children of a retried call unit = %d, want the failed one plus a fresh one", len(children))
	}
	retried := children[len(children)-1]
	if retried.ID == child.ID || retried.State != string(StateRunning) {
		t.Fatalf("retry did not make a fresh child: %+v", retried)
	}
	// The newest child is the one the unit is waiting on: settling the stale one
	// again must not move the unit.
	h.runner.complete(t, retried.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.requireUnitStatuses(t, parent, "wave", 1, map[string]string{
		"wave-unit-0": "done", "wave-join": "running",
	})
}

// TakeOver is a session-level action; a call unit holds no session, so it is
// refused with the reason rather than silently doing nothing.
func TestTakeOverRefusesACallUnit(t *testing.T) {
	h := newCallHarness(t, callUnitWorkflows(1), nil)
	parent := startCampaign(t, h)
	err := h.engine.TakeOverUnit(parent, "wave-unit-0")
	if err == nil || !strings.Contains(err.Error(), "holds no session") {
		t.Fatalf("take over a call unit = %v, want a refusal explaining there is no session", err)
	}
}

// A done child whose declared outputs cannot be read leaves nothing to hand the
// join, which is this unit's failure and not the whole attempt's.
func TestCallUnitChildWithoutDeclaredOutputsFailsTheUnit(t *testing.T) {
	workflows := callUnitWorkflows(1)
	child := workflows["child"]
	child.Outputs = map[string]def.WorkflowOutput{"verdict": {From: "work.never"}}
	workflows["child"] = child
	h := newCallHarness(t, workflows, nil)
	parent := startCampaign(t, h)
	childRun := h.unitCallChild(t, parent, "wave", 1, "wave-unit-0")

	h.runner.complete(t, childRun.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	h.requireUnitStatuses(t, parent, "wave", 1, map[string]string{
		"wave-unit-0": "failed", "wave-join": "pending",
	})
	requireItemState(t, h.store, parent, StateNeedsHuman, ReasonUnitFailed)
}

// A dynamic fan-out's call unit interpolates its arguments from the same
// element binding a unit prompt would reference.
func TestDynamicCallUnitArgsBindTheFanOutElement(t *testing.T) {
	campaign := def.Workflow{ID: "campaign", Phases: []def.Phase{
		{
			ID: "prepare", Driver: def.DriverAgent, Provider: testProvider, Model: "test-model",
			Outputs: map[string]def.Variable{"sections": {Schema: def.JSONSchema{
				Type: "array", Items: &def.JSONSchema{Type: "string"},
			}}},
			Gate: def.Gate{Routes: []def.Route{{To: "wave"}}},
		},
		{
			ID: "wave", Shape: def.ShapeFanOut, Over: "prepare.sections", As: "section",
			Unit: &def.Unit{ID: "wave-unit", Call: "child", Args: map[string]string{"seed": "section"}},
			Join: &def.Unit{ID: "wave-join", Provider: testProvider, Model: "test-model", Prompt: "join.md"},
			Gate: def.Gate{Routes: []def.Route{{To: "done"}}},
		},
	}}
	h := newCallHarness(t, map[string]def.Workflow{
		"campaign": campaign, "child": childWorkflow("child"),
	}, nil)
	item := testItem("campaign", "project", "campaign", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{
		Kind:     OutcomeDone,
		Envelope: json.RawMessage(`{"status":"done","outputs":{"sections":["alpha","beta"]},"question":null,"reason":null}`),
	})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	for index, want := range []string{"alpha", "beta"} {
		unitID := fmt.Sprintf("wave-unit-%d", index)
		child := h.unitCallChild(t, item.ID, "wave", 1, unitID)
		if string(child.Seeds) != fmt.Sprintf(`{"seed":%q}`, want) {
			t.Fatalf("unit %q seeds = %s, want the element it was stamped with", unitID, child.Seeds)
		}
	}
}
