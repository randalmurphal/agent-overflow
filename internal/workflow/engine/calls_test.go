package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// callPhaseDef is the authored call shape the engine executes: a phase that
// declares only its target, its arguments, its optional depth bound, and its
// gate.
func callPhaseDef(id, target string, args map[string]string, maxDepth int, routes []def.Route) def.Phase {
	return def.Phase{
		ID: id, Shape: def.ShapeCall, Call: target, Args: args, MaxDepth: maxDepth,
		Gate: def.Gate{Routes: routes},
	}
}

// callerWorkflow runs one agent phase, calls `target` with its output, routes on
// the child's declared outputs, and reports.
func callerWorkflow(target string) def.Workflow {
	return def.Workflow{ID: "caller", Phases: []def.Phase{
		agentPhase("prepare", nil, []def.Route{{To: "audit"}}),
		callPhaseDef("audit", target, map[string]string{"seed": "prepare.ok"}, 0, []def.Route{
			{When: &def.Predicate{Eq: &def.Comparison{Ref: "audit.verdict", Value: true}}, To: "report"},
			{To: "failed"},
		}),
		agentPhase("report", nil, []def.Route{{To: "done"}}),
	}}
}

// childWorkflow declares an output, which is the whole downstream surface its
// caller's call phase gets.
func childWorkflow(id string, phases ...def.Phase) def.Workflow {
	if len(phases) == 0 {
		phases = []def.Phase{agentPhase("work", nil, []def.Route{{To: "done"}})}
	}
	return def.Workflow{
		ID: id, Phases: phases,
		Outputs: map[string]def.WorkflowOutput{"verdict": {From: "work.ok"}},
	}
}

func newCallHarness(t *testing.T, workflows map[string]def.Workflow, before func(*store.Store)) *testHarness {
	t.Helper()
	return newHarness(t, Config{}, workflows, []string{"project"}, before)
}

func defaultCallWorkflows() map[string]def.Workflow {
	return map[string]def.Workflow{"caller": callerWorkflow("child"), "child": childWorkflow("child")}
}

// startCaller admits the parent and drives it to its call phase.
func startCaller(t *testing.T, h *testHarness) string {
	t.Helper()
	item := testItem("parent", "project", "caller", 0)
	item.WorktreePath = "/tmp/parent-worktree"
	item.Branch = "ao-parent"
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

func (h *testHarness) callChild(t *testing.T, parentID, phaseID string, attempt int) store.WorkItem {
	t.Helper()
	children, err := h.store.ListWorkItemCallChildren(parentID, phaseID, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 {
		t.Fatalf("call children of %s/%s/%d = %d, want exactly one", parentID, phaseID, attempt, len(children))
	}
	return children[0]
}

func (h *testHarness) phaseAttempt(t *testing.T, itemID, phaseID string, attempt int) store.WorkItemPhase {
	t.Helper()
	phases, err := h.store.ListWorkItemPhases(itemID)
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range phases {
		if phase.PhaseID == phaseID && phase.Attempt == attempt {
			return phase
		}
	}
	t.Fatalf("item %q has no attempt %s/%d; rows: %+v", itemID, phaseID, attempt, phases)
	return store.WorkItemPhase{}
}

func decodeEnvelope(t *testing.T, payload json.RawMessage) controlEnvelope {
	t.Helper()
	var envelope controlEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("decode envelope %s: %v", payload, err)
	}
	return envelope
}

func TestCallPhaseStartsChildAndRestsWaiting(t *testing.T) {
	h := newCallHarness(t, defaultCallWorkflows(), nil)
	parent := startCaller(t, h)

	child := h.callChild(t, parent, "audit", 1)
	if child.Source != WorkItemSourceCall || child.SourceRef != "parent/audit/1" {
		t.Fatalf("child provenance = %q/%q", child.Source, child.SourceRef)
	}
	if child.ParentItemID != parent || child.ParentPhaseID != "audit" || child.ParentAttempt != 1 || child.CallDepth != 1 {
		t.Fatalf("child linkage = %+v", child)
	}
	if child.WorkflowID != "child" || child.State != string(StateRunning) {
		t.Fatalf("child run = %+v", child)
	}
	// Workspace flows down: the child provisions nothing of its own (§9).
	if child.WorktreePath != "/tmp/parent-worktree" || child.Branch != "ao-parent" || child.BaseBranch != "main" {
		t.Fatalf("child workspace = %q/%q/%q, want the caller's", child.WorktreePath, child.Branch, child.BaseBranch)
	}
	if len(child.Budget) != 0 {
		t.Fatalf("child carries its own budget: %s", child.Budget)
	}
	// The seeds are the evaluated argument map, and the same values are on the
	// parent's attempt row so the invocation is auditable.
	if string(child.Seeds) != `{"seed":true}` {
		t.Fatalf("child seeds = %s", child.Seeds)
	}
	var input PhaseInput
	if err := json.Unmarshal(h.phaseAttempt(t, parent, "audit", 1).InputEnvelope, &input); err != nil {
		t.Fatal(err)
	}
	if len(input.Args) != 1 || input.Args["seed"] != true {
		t.Fatalf("persisted call args = %+v", input.Args)
	}

	// The parent rests: no runner, no resources, no provider capacity.
	for _, start := range h.runner.started() {
		if start.Key.ItemID == parent && start.Key.PhaseID == "audit" {
			t.Fatalf("call phase started a runner: %+v", start.Key)
		}
	}
	if len(h.engine.holders) != 1 {
		t.Fatalf("resource holders while a call rests = %v, want only the child's phase", h.engine.holders)
	}
	requireItemState(t, h.store, parent, StateRunning, "")
	requireItemState(t, h.store, child.ID, StateRunning, "")
	// The child's definition is resolved live, per invocation (§8).
	if h.definitions.callResolveCount("child") != 1 {
		t.Fatalf("call-time resolutions = %d, want 1", h.definitions.callResolveCount("child"))
	}
	if starts := h.runner.started(); starts[len(starts)-1].Key.ItemID != child.ID {
		t.Fatalf("child phase was not started: %+v", starts)
	}
}

func TestCallChildDoneCompletesParentPhaseWithChildOutputs(t *testing.T) {
	h := newCallHarness(t, defaultCallWorkflows(), nil)
	parent := startCaller(t, h)
	child := h.callChild(t, parent, "audit", 1)

	h.runner.complete(t, child.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	requireItemState(t, h.store, child.ID, StateDone, "")
	attempt := h.phaseAttempt(t, parent, "audit", 1)
	if attempt.Status != "completed" {
		t.Fatalf("parent call attempt = %+v", attempt)
	}
	envelope := decodeEnvelope(t, attempt.OutputEnvelope)
	if envelope.Status != "done" || envelope.Outputs["verdict"] != true {
		t.Fatalf("call phase envelope = %+v, want the child's declared outputs", envelope)
	}
	// The gate routed on the child's output, so the parent moved on.
	starts := h.runner.started()
	if last := starts[len(starts)-1]; last.Key.ItemID != parent || last.Key.PhaseID != "report" {
		t.Fatalf("parent did not advance on the child's outputs: %+v", last.Key)
	}
	h.runner.complete(t, parent, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, parent, StateDone, "")
	h.requireNoHeldResources(t)
}

// optionalNotesChild declares the shape the live campaign incident turned on: a
// child input that is legally absent, which a caller's `args:` forwards whether
// or not the run was seeded with one.
func optionalNotesChild(optional bool) def.Workflow {
	child := childWorkflow("child")
	child.Inputs = map[string]def.Variable{
		"job-notes": {Schema: def.JSONSchema{Type: "string"}, Optional: optional},
	}
	return child
}

// notesForwardingCaller adds the unseeded forward to the ordinary caller: an
// argument reading a workflow input this run was started without.
func notesForwardingCaller() def.Workflow {
	workflow := callerWorkflow("child")
	workflow.Phases[1].Args["job-notes"] = "job-notes"
	return workflow
}

// An input the child declares optional may be absent, so the argument that
// forwards it may be too: the child is started without that seed, which is the
// same run a direct start without it would have produced. A campaign whose
// final gate calls itself for the next wave must not die at its own recursion
// point because a value it never had is still missing.
func TestCallPhaseOmitsAnArgumentAnAbsentOptionalChildInputWouldSeed(t *testing.T) {
	h := newCallHarness(t, map[string]def.Workflow{
		"caller": notesForwardingCaller(), "child": optionalNotesChild(true),
	}, nil)
	parent := startCaller(t, h)

	child := h.callChild(t, parent, "audit", 1)
	if string(child.Seeds) != `{"seed":true}` {
		t.Fatalf("child seeds = %s, want the absent optional argument omitted entirely", child.Seeds)
	}
	requireItemState(t, h.store, parent, StateRunning, "")
	requireItemState(t, h.store, child.ID, StateRunning, "")
}

// The same absence against a REQUIRED child input is still a wiring error — and
// it now parks on a phase attempt that exists, so the refusal is readable from
// the run's own history instead of vanishing with the invocation.
func TestCallPhaseParksWhenAnAbsentArgumentSeedsARequiredChildInput(t *testing.T) {
	h := newCallHarness(t, map[string]def.Workflow{
		"caller": notesForwardingCaller(), "child": optionalNotesChild(false),
	}, nil)
	parent := startCaller(t, h)

	requireItemState(t, h.store, parent, StateNeedsHuman, ReasonWiringError)
	requireParkCause(t, h.phaseAttempt(t, parent, "audit", 1), "job-notes (job-notes)")
	children, err := h.store.ListWorkItemCallChildren(parent, "audit", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 0 {
		t.Fatalf("a refused call still created %d child runs", len(children))
	}
	h.requireNoHeldResources(t)
}

// An argument naming no child input at all is refused whether or not it
// resolves: there is no declaration that could be optional, so nothing says the
// absence is legal.
func TestCallPhaseParksWhenAnAbsentArgumentNamesNoChildInput(t *testing.T) {
	h := newCallHarness(t, map[string]def.Workflow{
		"caller": notesForwardingCaller(), "child": childWorkflow("child"),
	}, nil)
	parent := startCaller(t, h)

	requireItemState(t, h.store, parent, StateNeedsHuman, ReasonWiringError)
	requireParkCause(t, h.phaseAttempt(t, parent, "audit", 1), "job-notes")
}

func TestCallChildFailedFailsParentPhaseAndRerunCallsAgain(t *testing.T) {
	workflows := defaultCallWorkflows()
	// The child's own gate sends it to `failed`.
	workflows["child"] = childWorkflow("child", agentPhase("work", nil, []def.Route{
		{When: &def.Predicate{Eq: &def.Comparison{Ref: "work.ok", Value: true}}, To: "done"},
		{To: "failed"},
	}))
	h := newCallHarness(t, workflows, nil)
	parent := startCaller(t, h)
	child := h.callChild(t, parent, "audit", 1)

	h.runner.complete(t, child.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(false)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, child.ID, StateFailed, ReasonCheckFailedGenuine)
	requireItemState(t, h.store, parent, StateFailed, ReasonChildFailed)
	envelope := decodeEnvelope(t, h.phaseAttempt(t, parent, "audit", 1).OutputEnvelope)
	if !strings.Contains(envelope.Reason, child.ID) || !strings.Contains(envelope.Reason, "failed") {
		t.Fatalf("failure envelope must name the child: %+v", envelope)
	}
	h.requireNoHeldResources(t)

	// A rerun makes a fresh call: the failed child is history, not something to
	// resume.
	if err := h.engine.RerunFailed(parent, "", false); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, parent, StateRunning, "")
	retried := h.callChild(t, parent, "audit", 2)
	if retried.ID == child.ID {
		t.Fatal("rerun reused the failed child run")
	}
	if retried.ParentAttempt != 2 || retried.State != string(StateRunning) {
		t.Fatalf("fresh child = %+v", retried)
	}
}

func TestCallChildParkedLeavesParentWaitingUntilItCompletes(t *testing.T) {
	h := newCallHarness(t, defaultCallWorkflows(), nil)
	parent := startCaller(t, h)
	child := h.callChild(t, parent, "audit", 1)

	// The runner stamps the attempt's thread the moment it exists; the answer
	// continues that same provider session.
	if err := h.store.AttachWorkItemPhaseRun(child.ID, "work", 1, "child-thread", "/tmp/child.md"); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, child.ID, Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, child.ID, StateNeedsHuman, ReasonQuestion)
	// A parked child is not a finished child: the parent keeps waiting rather
	// than failing or parking in sympathy.
	requireItemState(t, h.store, parent, StateRunning, "")
	if h.phaseAttempt(t, parent, "audit", 1).Status != "running" {
		t.Fatal("parent call attempt settled on a parked child")
	}

	if err := h.engine.Answer(child.ID, "carry on"); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, child.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, child.ID, StateDone, "")
	if status := h.phaseAttempt(t, parent, "audit", 1).Status; status != "completed" {
		t.Fatalf("parent call attempt after the child finished = %q", status)
	}
	requireItemState(t, h.store, parent, StateRunning, "")
}

func TestCallChildCancelledParksParent(t *testing.T) {
	h := newCallHarness(t, defaultCallWorkflows(), nil)
	parent := startCaller(t, h)
	child := h.callChild(t, parent, "audit", 1)

	if err := h.engine.Cancel(child.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, child.ID, StateCancelled, ReasonInterrupted)
	// Cancelling the child is not cancelling the parent — that stays a human
	// decision — but the parent cannot proceed either.
	requireItemState(t, h.store, parent, StateNeedsHuman, ReasonAgentError)
	envelope := decodeEnvelope(t, h.phaseAttempt(t, parent, "audit", 1).OutputEnvelope)
	if !strings.Contains(envelope.Reason, child.ID) || !strings.Contains(envelope.Reason, "cancelled") {
		t.Fatalf("park envelope must name the cancelled child: %+v", envelope)
	}
	h.requireNoHeldResources(t)
}

// recursiveWorkflow calls itself after its first phase, bounded by maxDepth.
func recursiveWorkflow(maxDepth int) def.Workflow {
	return def.Workflow{ID: "recurse", Phases: []def.Phase{
		agentPhase("work", nil, []def.Route{{To: "again"}}),
		callPhaseDef("again", "recurse", nil, maxDepth, []def.Route{{To: "done"}}),
	}, Outputs: map[string]def.WorkflowOutput{"verdict": {From: "work.ok"}}}
}

func TestSelfCallRecursionStopsAtDeclaredMaxDepth(t *testing.T) {
	h := newCallHarness(t, map[string]def.Workflow{"recurse": recursiveWorkflow(2)}, nil)
	if err := h.engine.StartItem(testItem("root", "project", "recurse", 0)); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, "root", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	first := h.callChild(t, "root", "again", 1)
	if first.CallDepth != 1 {
		t.Fatalf("first child depth = %d", first.CallDepth)
	}

	h.runner.complete(t, first.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	second := h.callChild(t, first.ID, "again", 1)
	if second.CallDepth != 2 {
		t.Fatalf("second child depth = %d", second.CallDepth)
	}

	// The third invocation would be the third traversal of an edge bounded at
	// two, so it is refused before a run exists.
	h.runner.complete(t, second.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, second.ID, StateNeedsHuman, ReasonWiringError)
	grandchildren, err := h.store.ListWorkItemChildren(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(grandchildren) != 0 {
		t.Fatalf("depth bound did not stop the recursion: %+v", grandchildren)
	}
	requireParkCause(t, h.phaseAttempt(t, second.ID, "again", 1),
		"max_depth of 2", "recurse.again -> recurse.again -> recurse.again")
	// Its ancestors are still waiting on it; a bounded recursion parks the run
	// that hit the bound, not the tree.
	requireItemState(t, h.store, first.ID, StateRunning, "")
	requireItemState(t, h.store, "root", StateRunning, "")
}

func TestFreshChildStartsWithEmptyLoopCounts(t *testing.T) {
	// The loop edge is bounded at one traversal per fresh entry. A child that
	// inherited its parent's spend could not loop at all.
	looping := def.Workflow{ID: "recurse", Phases: []def.Phase{
		agentPhase("work", nil, []def.Route{
			{When: &def.Predicate{Eq: &def.Comparison{Ref: "work.ok", Value: false}}, Loop: "work", Max: def.LiteralBound(1)},
			{To: "again"},
		}),
		callPhaseDef("again", "recurse", nil, 2, []def.Route{{To: "done"}}),
	}, Outputs: map[string]def.WorkflowOutput{"verdict": {From: "work.ok"}}}
	h := newCallHarness(t, map[string]def.Workflow{"recurse": looping}, nil)
	if err := h.engine.StartItem(testItem("root", "project", "recurse", 0)); err != nil {
		t.Fatal(err)
	}
	// The root spends its loop edge, then advances into the call.
	h.runner.complete(t, "root", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(false)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, "root", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	child := h.callChild(t, "root", "again", 1)

	phases, err := h.store.ListWorkItemPhaseContexts(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	counts, err := loopCounts(child.ID, phases)
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 0 {
		t.Fatalf("fresh child loop counts = %v, want none", counts)
	}
	// And behaviourally: the child may traverse the same edge its parent spent.
	h.runner.complete(t, child.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(false)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, child.ID, StateRunning, "")
	if h.phaseAttempt(t, child.ID, "work", 2).Status != "running" {
		t.Fatal("child could not loop; its counts were inherited")
	}
}

func TestRootBudgetIsEnforcedAcrossTheCallTree(t *testing.T) {
	workflows := defaultCallWorkflows()
	// Two phases, so the child reaches a budget-checked boundary of its own.
	workflows["child"] = childWorkflow("child",
		agentPhase("work", nil, []def.Route{{To: "verify"}}),
		agentPhase("verify", nil, []def.Route{{To: "done"}}),
	)
	h := newCallHarness(t, workflows, nil)
	item := testItem("parent", "project", "caller", 0)
	item.Budget = json.RawMessage(`{"tokens":100}`)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	child := h.callChild(t, item.ID, "audit", 1)

	// The spend is the child's alone; the ceiling is the root's.
	h.spend.spends[child.ID] = Spend{Tokens: 250}
	h.runner.complete(t, child.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, child.ID, StateNeedsHuman, ReasonBudgetExhausted)
	// The run parked is the one that was about to spend past the ceiling.
	events := h.emitter.errorEvents(child.ID)
	if len(events) != 1 || events[0].Spend == nil || events[0].Spend.Tokens != 250 {
		t.Fatalf("budget error events = %+v", events)
	}
	requireItemState(t, h.store, item.ID, StateRunning, "")
	if h.spend.calls[len(h.spend.calls)-1] != item.ID {
		t.Fatalf("spend was queried for %q, want the tree root %q", h.spend.calls[len(h.spend.calls)-1], item.ID)
	}
}

func TestBudgetOnACalledRunIsRefused(t *testing.T) {
	h := newCallHarness(t, defaultCallWorkflows(), nil)
	child := testItem("smuggled", "project", "child", 0)
	child.ParentItemID = "parent"
	child.ParentPhaseID = "audit"
	child.ParentAttempt = 1
	child.CallDepth = 1
	child.Budget = json.RawMessage(`{"tokens":10}`)
	err := h.engine.StartItem(child)
	if err == nil || !strings.Contains(err.Error(), "only a root run carries a budget") {
		t.Fatalf("budget on a child = %v", err)
	}
	child.Budget = nil
	err = h.engine.StartItem(child)
	if err == nil || !strings.Contains(err.Error(), "parent linkage is set by the call path") {
		t.Fatalf("externally admitted linkage = %v", err)
	}
}

func TestCancelTearsDownTheChildSubtreeFirst(t *testing.T) {
	h := newCallHarness(t, map[string]def.Workflow{"recurse": recursiveWorkflow(4)}, nil)
	if err := h.engine.StartItem(testItem("root", "project", "recurse", 0)); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, "root", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	child := h.callChild(t, "root", "again", 1)
	h.runner.complete(t, child.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	grandchild := h.callChild(t, child.ID, "again", 1)

	if err := h.engine.Cancel("root"); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, grandchild.ID, StateCancelled, ReasonInterrupted)
	requireItemState(t, h.store, child.ID, StateCancelled, ReasonInterrupted)
	requireItemState(t, h.store, "root", StateCancelled, ReasonInterrupted)

	// Deepest first: the only live provider session in the tree is the
	// grandchild's, and it must be stopped as part of bringing the tree down.
	stopped := h.runner.stopped()
	if len(stopped) != 1 || stopped[0].ItemID != grandchild.ID {
		t.Fatalf("stops = %+v, want the grandchild's live phase", stopped)
	}
	h.requireNoHeldResources(t)
}

func TestCancelDoesNotReachAParkedDescendantsSiblingTree(t *testing.T) {
	h := newCallHarness(t, defaultCallWorkflows(), nil)
	parent := startCaller(t, h)
	child := h.callChild(t, parent, "audit", 1)
	// A parked child is evicted from memory; cancelling the tree still has to
	// bring it down.
	h.runner.complete(t, child.ID, Outcome{Kind: OutcomeStuck, Envelope: stuckEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, child.ID, StateNeedsHuman, ReasonStuck)

	other := testItem("other", "project", "caller", 1)
	if err := h.engine.StartItem(other); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Cancel(parent); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, child.ID, StateCancelled, ReasonInterrupted)
	requireItemState(t, h.store, parent, StateCancelled, ReasonInterrupted)
	requireItemState(t, h.store, other.ID, StateRunning, "")
}

// callTreeSeed writes the rows a crash leaves behind: a caller resting on a call
// attempt, and whatever the child got to before the process died. `childState`
// is empty when the crash happened before the child row was created.
type callTreeSeed struct {
	childState     State
	childReason    Reason
	childEnvelope  json.RawMessage
	childCreatedAt int64
	omitChild      bool
}

func seedInterruptedCall(t *testing.T, database *store.Store, seed callTreeSeed) (parentID, childID string) {
	t.Helper()
	caller := callerWorkflow("child")
	child := childWorkflow("child")
	callerSnapshot, err := json.Marshal(Snapshot{Workflow: caller, WorkspaceNeed: def.WorkspaceProjectRoot})
	if err != nil {
		t.Fatal(err)
	}
	childSnapshot, err := json.Marshal(Snapshot{Workflow: child, WorkspaceNeed: def.WorkspaceProjectRoot})
	if err != nil {
		t.Fatal(err)
	}
	parent := testItem("parent", "project", "caller", 0)
	if err := database.CreateWorkItem(parent); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateWorkItemRunStart(parent.ID, callerSnapshot, "", "", "", 20); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateWorkItemPhase(store.WorkItemPhase{
		ItemID: parent.ID, PhaseID: "prepare", Attempt: 1,
		InputEnvelope:  json.RawMessage(`{}`),
		OutputEnvelope: doneEnvelope(true), Status: "completed", StartedAt: 21, EndedAt: 22,
	}); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(PhaseInput{
		Vars: map[string]any{"prepare.ok": true}, Args: map[string]any{"seed": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CreateWorkItemPhase(store.WorkItemPhase{
		ItemID: parent.ID, PhaseID: "audit", Attempt: 1,
		InputEnvelope: input, Status: "running", StartedAt: 23,
	}); err != nil {
		t.Fatal(err)
	}
	if seed.omitChild {
		return parent.ID, ""
	}
	called := testItem("called", "project", "child", 0)
	called.CreatedAt = seed.childCreatedAt
	called.Seeds = json.RawMessage(`{"seed":true}`)
	called.Source = WorkItemSourceCall
	called.SourceRef = parent.ID + "/audit/1"
	called.ParentItemID = parent.ID
	called.ParentPhaseID = "audit"
	called.ParentAttempt = 1
	called.CallDepth = 1
	if err := database.CreateWorkItem(called); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateWorkItemRunStart(called.ID, childSnapshot, "", "", "", 24); err != nil {
		t.Fatal(err)
	}
	status, endedAt := "running", int64(0)
	if len(seed.childEnvelope) > 0 {
		status, endedAt = "completed", 26
	}
	if err := database.CreateWorkItemPhase(store.WorkItemPhase{
		ItemID: called.ID, PhaseID: "work", Attempt: 1,
		InputEnvelope: json.RawMessage(`{}`), OutputEnvelope: seed.childEnvelope,
		Status: status, StartedAt: 25, EndedAt: endedAt,
	}); err != nil {
		t.Fatal(err)
	}
	if seed.childState != "" && seed.childState != StateRunning {
		if err := database.UpdateWorkItemState(called.ID, string(seed.childState), string(seed.childReason), 27); err != nil {
			t.Fatal(err)
		}
	}
	return parent.ID, called.ID
}

func TestRebuildLeavesAParentRestingOnALiveChild(t *testing.T) {
	var parent, child string
	h := newCallHarness(t, defaultCallWorkflows(), func(database *store.Store) {
		parent, child = seedInterruptedCall(t, database, callTreeSeed{
			childState: StateRunning, childCreatedAt: 11,
		})
	})
	// The child rebuilds as an ordinary run: its own interrupted attempt is
	// swept, which parks it and leaves the parent waiting exactly as a live park
	// would.
	requireItemState(t, h.store, child, StateNeedsHuman, ReasonInterrupted)
	requireItemState(t, h.store, parent, StateRunning, "")
	if status := h.phaseAttempt(t, parent, "audit", 1).Status; status != "running" {
		t.Fatalf("call attempt after rebuild = %q, want it still resting", status)
	}
	phases, err := h.store.ListWorkItemPhases(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 2 {
		t.Fatalf("rebuild added parent attempts: %+v", phases)
	}
	children, err := h.store.ListWorkItemChildren(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 {
		t.Fatalf("rebuild re-invoked a live call: %+v", children)
	}
}

func TestRebuildSettlesAParentFromATerminalChild(t *testing.T) {
	// Rebuild order must not matter: the parent may be rebuilt before or after
	// the child, and the store is the only thing either of them reads.
	for _, tc := range []struct {
		name           string
		childCreatedAt int64
	}{
		{"child rebuilt first", 5},
		{"parent rebuilt first", 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var parent, child string
			h := newCallHarness(t, defaultCallWorkflows(), func(database *store.Store) {
				parent, child = seedInterruptedCall(t, database, callTreeSeed{
					childState: StateDone, childEnvelope: doneEnvelope(true),
					childCreatedAt: tc.childCreatedAt,
				})
			})
			requireItemState(t, h.store, child, StateDone, "")
			attempt := h.phaseAttempt(t, parent, "audit", 1)
			if attempt.Status != "completed" {
				t.Fatalf("call attempt after rebuild = %+v", attempt)
			}
			envelope := decodeEnvelope(t, attempt.OutputEnvelope)
			if envelope.Status != "done" || envelope.Outputs["verdict"] != true {
				t.Fatalf("rebuilt call envelope = %+v", envelope)
			}
			// The gate ran on the child's outputs, so the parent advanced.
			starts := h.runner.started()
			if len(starts) != 1 || starts[0].Key.ItemID != parent || starts[0].Key.PhaseID != "report" {
				t.Fatalf("parent did not advance after rebuild: %+v", starts)
			}
			requireItemState(t, h.store, parent, StateRunning, "")
		})
	}
}

func TestRebuildFailsAParentFromAFailedChild(t *testing.T) {
	var parent, child string
	h := newCallHarness(t, defaultCallWorkflows(), func(database *store.Store) {
		parent, child = seedInterruptedCall(t, database, callTreeSeed{
			childState: StateFailed, childReason: ReasonCheckFailedGenuine,
			childEnvelope: doneEnvelope(false), childCreatedAt: 11,
		})
	})
	requireItemState(t, h.store, child, StateFailed, ReasonCheckFailedGenuine)
	requireItemState(t, h.store, parent, StateFailed, ReasonChildFailed)
	if status := h.phaseAttempt(t, parent, "audit", 1).Status; status != "failed" {
		t.Fatalf("call attempt after rebuild = %q", status)
	}
}

func TestRebuildReinvokesACallWhoseChildWasNeverCreated(t *testing.T) {
	var parent string
	h := newCallHarness(t, defaultCallWorkflows(), func(database *store.Store) {
		parent, _ = seedInterruptedCall(t, database, callTreeSeed{omitChild: true})
	})
	requireItemState(t, h.store, parent, StateRunning, "")
	child := h.callChild(t, parent, "audit", 1)
	if child.ParentAttempt != 1 || child.CallDepth != 1 || child.Source != WorkItemSourceCall {
		t.Fatalf("re-invoked child = %+v", child)
	}
	// The re-invocation reuses the persisted attempt — a second attempt row would
	// pollute the phase history and every loop count derived from it.
	phases, err := h.store.ListWorkItemPhases(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 2 {
		t.Fatalf("re-invocation created a new attempt: %+v", phases)
	}
	// The arguments came from the persisted attempt input, not from a re-run of
	// the phase that produced them.
	if string(child.Seeds) != `{"seed":true}` {
		t.Fatalf("re-invoked child seeds = %s", child.Seeds)
	}
}
