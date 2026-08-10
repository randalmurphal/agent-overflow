package main

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	workflowrunner "agent-overflow/internal/workflow/runner"
)

// Where the goal chain comes from: the run tree's call linkage, resolved on the
// same ancestry walk campaign memory keys on.

type goalHarness struct{ *memoryHarness }

func newGoalHarness(t *testing.T) *goalHarness {
	t.Helper()
	return &goalHarness{memoryHarness: newMemoryHarness(t)}
}

// callChain writes a root run and one called run per goal, returning the rows
// root-first. Each entry is `id=goal`; a bare id states no goal of its own,
// which is what a run started without `--goal` looks like.
func (h *goalHarness) callChain(t *testing.T, workflowID string, nonGoals []string, entries ...string) {
	t.Helper()
	snapshot, err := json.Marshal(engine.Snapshot{
		Workflow: def.Workflow{ID: workflowID, NonGoals: nonGoals},
	})
	if err != nil {
		t.Fatal(err)
	}
	var previous string
	for depth, entry := range entries {
		id, goal, _ := strings.Cut(entry, "=")
		item := store.WorkItem{
			ID: id, WorkflowID: workflowID, State: string(engine.StateRunning),
			Goal: goal, Snapshot: snapshot,
		}
		if depth > 0 {
			item.ParentItemID, item.ParentPhaseID, item.ParentAttempt, item.CallDepth = previous, "next-wave", 1, depth
		}
		h.run(t, item)
		previous = id
	}
}

func (h *goalHarness) chain(itemID string, workflow def.Workflow) workflowrunner.GoalChain {
	return h.app.workflowPromptAncestry(itemID, workflow).Goals
}

// The chain a lane reads is the whole call stack above it, root-first, and the
// non-goals riding with it are the ones its OWN definition declared.
func TestGoalChainResolvesTheCallStackRootFirst(t *testing.T) {
	h := newGoalHarness(t)
	h.callChain(t, "port-campaign", nil,
		"root=port the renderer", "wave-1=port the effects layer", "lane=port effects/blur.go")

	chain := h.chain("lane", def.Workflow{ID: "port-one-task", NonGoals: []string{"Do not widen the public API."}})
	if len(chain.Links) != 3 {
		t.Fatalf("links = %+v", chain.Links)
	}
	for index, want := range []workflowrunner.GoalLink{
		{RunID: "root", WorkflowID: "port-campaign", Goal: "port the renderer", Root: true},
		{RunID: "wave-1", WorkflowID: "port-campaign", Goal: "port the effects layer"},
		{RunID: "lane", WorkflowID: "port-campaign", Goal: "port effects/blur.go", Current: true},
	} {
		if chain.Links[index] != want {
			t.Fatalf("link %d = %+v, want %+v", index, chain.Links[index], want)
		}
	}
	if chain.WorkflowID != "port-one-task" || len(chain.NonGoals) != 1 {
		t.Fatalf("this run's non-goals did not come from the definition in hand: %+v", chain)
	}
}

// The engine copies a caller's goal onto every run it calls, so a forty-wave
// campaign's raw chain is forty copies of one sentence. Consecutive duplicates
// collapse to one link attributed to the ROOT-most run that stated it — the run
// the goal was actually recorded on.
func TestConsecutiveInheritedGoalsCollapseToOneLink(t *testing.T) {
	h := newGoalHarness(t)
	h.callChain(t, "port-campaign", nil,
		"root=port the renderer", "wave-1=port the renderer", "wave-2=port the renderer",
		"wave-3=port the effects layer", "lane=port the renderer")

	chain := h.chain("lane", def.Workflow{ID: "port-campaign"})
	if len(chain.Links) != 3 {
		t.Fatalf("inherited goals did not collapse: %+v", chain.Links)
	}
	if chain.Links[0].RunID != "root" || !chain.Links[0].Root {
		t.Fatalf("collapsed link is not attributed to the root-most stater: %+v", chain.Links[0])
	}
	// A goal that RE-APPEARS after a different one is a real second statement of
	// intent, not an inheritance, so it keeps its own link.
	if chain.Links[2].RunID != "lane" || chain.Links[2].Goal != "port the renderer" || !chain.Links[2].Current {
		t.Fatalf("a re-stated goal was collapsed into an earlier one: %+v", chain.Links)
	}
}

// A run with no goal and no ancestry produces nothing to render at all — the
// bare single-run case must cost zero prompt bytes.
func TestABareRunResolvesAnEmptyChain(t *testing.T) {
	h := newGoalHarness(t)
	h.callChain(t, "review", nil, "solo")
	if chain := h.chain("solo", def.Workflow{ID: "review"}); !chain.Empty() {
		t.Fatalf("a bare goalless run resolved a chain: %+v", chain)
	}
	// Its own definition's non-goals alone are enough to earn the block: the
	// boundary is a fact about the definition, not about the call stack.
	chain := h.chain("solo", def.Workflow{ID: "review", NonGoals: []string{"Do not refactor."}})
	if chain.Empty() || len(chain.Links) != 0 {
		t.Fatalf("non-goals alone did not produce a block: %+v", chain)
	}
}

// A called run that stated no goal of its own still reads the chain above it,
// and no link claims to be its.
func TestAChildWithoutAGoalKeepsTheChainAboveIt(t *testing.T) {
	h := newGoalHarness(t)
	h.callChain(t, "port-campaign", nil, "root=port the renderer", "lane")
	chain := h.chain("lane", def.Workflow{ID: "port-one-task"})
	if len(chain.Links) != 1 || chain.Links[0].RunID != "root" {
		t.Fatalf("child lost the chain above it: %+v", chain.Links)
	}
	if chain.Links[0].Current {
		t.Fatalf("a goalless child claimed the root's link: %+v", chain.Links[0])
	}
}

// The root's non-goals bind every run inside the campaign it started, so a lane
// running a DIFFERENT definition reads both lists.
func TestARootsNonGoalsRideDownToAChildRunningAnotherDefinition(t *testing.T) {
	h := newGoalHarness(t)
	h.callChain(t, "port-campaign", []string{"Do not redesign the build system."},
		"root=port the renderer", "lane=port one file")

	chain := h.chain("lane", def.Workflow{ID: "port-one-task", NonGoals: []string{"Do not widen the public API."}})
	if chain.RootWorkflowID != "port-campaign" || len(chain.RootNonGoals) != 1 {
		t.Fatalf("the root's non-goals did not reach the lane: %+v", chain)
	}
	if chain.RootNonGoals[0] != "Do not redesign the build system." {
		t.Fatalf("root non-goals = %+v", chain.RootNonGoals)
	}
}

// Prompt assembly runs the ancestry walk for EVERY element of every wave, so
// the walk may not drag a frozen workflow snapshot along per ancestor: a
// twenty-unit wave at depth forty would decode eight hundred of them, with every
// prompt file inlined, to render one goal chain and one memory digest.
//
// The root's snapshot is still read — it is where the campaign's non-goals live
// — but exactly once, from the root's own row, which is what the assertions
// below are: the ancestors carry no snapshot, and the root's non-goals arrive
// anyway.
func TestTheAncestryWalkCarriesNoFrozenSnapshots(t *testing.T) {
	h := newGoalHarness(t)
	h.callChain(t, "port-campaign", []string{"Do not redesign the build system."},
		"root=port the renderer", "wave-1=port the effects layer", "lane=port one file")

	lane, err := h.app.store.GetWorkItem("lane")
	if err != nil {
		t.Fatal(err)
	}
	if len(lane.Snapshot) == 0 {
		t.Fatal("the fixture wrote no snapshots, so this test proves nothing")
	}
	ancestry, err := h.app.workflowAncestry(lane)
	if err != nil {
		t.Fatal(err)
	}
	if len(ancestry) != 3 || ancestry[0].ID != "root" || ancestry[2].ID != "lane" {
		t.Fatalf("ancestry = %+v, want root-first root/wave-1/lane", ancestry)
	}
	for _, ancestor := range ancestry[:len(ancestry)-1] {
		if len(ancestor.Snapshot) != 0 || len(ancestor.Seeds) != 0 {
			t.Fatalf("ancestor %q was walked as a full row (%d snapshot bytes)",
				ancestor.ID, len(ancestor.Snapshot))
		}
	}
	// The tree the memory digest keys on comes off that same walk, so the two
	// blocks cost one traversal between them.
	tree, err := h.app.workflowMemoryTreeOf(ancestry)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := h.app.workflowMemoryTreeFor(lane)
	if err != nil {
		t.Fatal(err)
	}
	if tree != direct {
		t.Fatalf("the walked tree %+v differs from the re-walked one %+v", tree, direct)
	}
	if chain := h.chain("lane", def.Workflow{ID: "port-one-task"}); len(chain.RootNonGoals) != 1 {
		t.Fatalf("the root's non-goals were lost with its snapshot: %+v", chain)
	}
}

// A recursive campaign's waves run the SAME definition, so the two lists are
// one list — printing it twice under two headings would say nothing more and
// read as two separate boundaries.
func TestIdenticalRootNonGoalsAreNotPrintedTwice(t *testing.T) {
	h := newGoalHarness(t)
	nonGoals := []string{"Do not redesign the build system."}
	h.callChain(t, "port-campaign", nonGoals, "root=port the renderer", "wave-1=port the effects layer")

	chain := h.chain("wave-1", def.Workflow{ID: "port-campaign", NonGoals: nonGoals})
	if len(chain.RootNonGoals) != 0 || chain.RootWorkflowID != "" {
		t.Fatalf("an identical root list was carried a second time: %+v", chain)
	}
	if len(chain.NonGoals) != 1 {
		t.Fatalf("the run's own list was dropped: %+v", chain)
	}
}

// The root of a tree is its own root: there is no second list to carry, whether
// or not it declares any.
func TestARootRunCarriesOnlyItsOwnNonGoals(t *testing.T) {
	h := newGoalHarness(t)
	h.callChain(t, "port-campaign", []string{"Do not redesign the build system."}, "root=port the renderer")
	chain := h.chain("root", def.Workflow{ID: "port-campaign", NonGoals: []string{"Do not redesign the build system."}})
	if len(chain.RootNonGoals) != 0 {
		t.Fatalf("a root carried its own list twice: %+v", chain)
	}
	if len(chain.Links) != 1 || !chain.Links[0].Root || !chain.Links[0].Current {
		t.Fatalf("a root run's single link is mislabelled: %+v", chain.Links)
	}
}

// Context is not contract. A run that cannot be loaded at all, and one whose
// frozen snapshot will not decode, both yield the blocks that could be built
// rather than failing the attempt that was about to start.
func TestAnUnresolvableAncestryDegradesToWhatIsInHand(t *testing.T) {
	h := newGoalHarness(t)
	// The definition is in hand, so its non-goals survive a run that cannot be
	// loaded at all; only the goals, which live on the rows, are lost.
	unloadable := h.chain("no-such-run", def.Workflow{ID: "w", NonGoals: []string{"Do not refactor."}})
	if len(unloadable.Links) != 0 {
		t.Fatalf("an unloadable run produced goals: %+v", unloadable.Links)
	}
	if len(unloadable.NonGoals) != 1 || unloadable.WorkflowID != "w" {
		t.Fatalf("an unloadable run lost the boundaries its definition states: %+v", unloadable)
	}

	// A root whose snapshot will not decode into a definition still contributes
	// its GOAL — that comes off the run row — and simply contributes no
	// non-goals. The store's CHECK constraint keeps the column parseable as
	// JSON, so what is unreachable is malformed bytes, not a wrong shape.
	h.run(t, store.WorkItem{
		ID: "root", WorkflowID: "port-campaign", State: string(engine.StateRunning),
		Goal: "port the renderer", Snapshot: []byte(`{"workflow":"not a definition"}`),
	})
	h.run(t, store.WorkItem{
		ID: "lane", WorkflowID: "port-campaign", State: string(engine.StateRunning),
		Goal: "port a file", ParentItemID: "root", ParentPhaseID: "next-wave", ParentAttempt: 1, CallDepth: 1,
	})
	chain := h.chain("lane", def.Workflow{ID: "port-one-task"})
	if len(chain.Links) != 2 || chain.Links[0].Goal != "port the renderer" {
		t.Fatalf("a corrupt snapshot cost the run its goal: %+v", chain.Links)
	}
	if len(chain.RootNonGoals) != 0 {
		t.Fatalf("a corrupt snapshot produced non-goals: %+v", chain.RootNonGoals)
	}
}
