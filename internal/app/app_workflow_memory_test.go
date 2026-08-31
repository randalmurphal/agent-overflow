package app

import (
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/memory"
	workflowrunner "agent-overflow/internal/workflow/runner"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Campaign memory's run-shaped half: which tree a note lands in, what
// provenance it is stamped with, and who may write it. The fixture writes run
// rows directly, exactly as the inspect fixture does — what is under test is
// the resolution and the authorization, not how a run reaches a state.

type memoryHarness struct{ *inspectHarness }

func newMemoryHarness(t *testing.T) *memoryHarness {
	t.Helper()
	return &memoryHarness{inspectHarness: newInspectHarness(t)}
}

// campaign writes a root run and a chain of called runs under it, returning
// every row root-first. The linkage CHECK constraints tie parentage and
// call_depth together, so this is also the only correct way to produce a run
// whose wave is not zero.
func (h *memoryHarness) campaign(t *testing.T, ids ...string) []store.WorkItem {
	t.Helper()
	rows := make([]store.WorkItem, 0, len(ids))
	for index, id := range ids {
		item := store.WorkItem{ID: id, WorkflowID: "campaign", State: string(engine.StateRunning)}
		if index > 0 {
			item.ParentItemID = ids[index-1]
			item.ParentPhaseID = "call-next"
			item.ParentAttempt = 1
			item.CallDepth = index
		}
		rows = append(rows, h.run(t, item))
	}
	return rows
}

func (h *memoryHarness) phaseScope(itemID, phaseID, threadID string, grants ...def.Grant) context.Context {
	names := make([]string, 0, len(grants))
	for _, grant := range grants {
		names = append(names, string(grant))
	}
	return transport.WithCallerScope(context.Background(), transport.CallerScope{
		Kind: transport.ScopeKindPhase, ThreadID: threadID, ProjectID: defaultTestProjectID,
		ItemID: itemID, PhaseID: phaseID, Grants: names,
	})
}

func (h *memoryHarness) notes(t *testing.T, rootID string) []memory.Note {
	t.Helper()
	path, err := memory.NotesPath(h.app.workflowDataRoot(), rootID)
	if err != nil {
		t.Fatal(err)
	}
	notes, skipped, err := memory.ReadNotes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Fatalf("log holds unreadable lines: %+v", skipped)
	}
	return notes
}

// A campaign is the TREE, not the run: a lane three calls deep records into the
// root's log, and the wave it records is its own call depth.
func TestAddMemoryWritesTheRootTreeAndStampsTheWave(t *testing.T) {
	h := newMemoryHarness(t)
	runs := h.campaign(t, "root", "wave-1", "lane")

	for index, item := range runs {
		ctx := h.phaseScope(item.ID, "implement", "thread-"+item.ID)
		result, err := h.app.WorkflowAgentAddMemory(ctx, WorkflowAgentMemoryInput{
			Kind: memory.KindLearning, Text: "note from " + item.ID, Files: []string{"a.go"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.RootID != "root" {
			t.Fatalf("%s wrote tree %q, want the root's", item.ID, result.RootID)
		}
		if result.Wave != index {
			t.Fatalf("%s recorded wave %d, want its call depth %d", item.ID, result.Wave, index)
		}
	}

	notes := h.notes(t, "root")
	if len(notes) != 3 {
		t.Fatalf("root log holds %d notes, want the whole tree's 3", len(notes))
	}
	for index, note := range notes {
		if note.Provenance.RunID != runs[index].ID || note.Provenance.Wave != index {
			t.Fatalf("note %d provenance = %+v", index, note.Provenance)
		}
		if note.Provenance.PhaseID != "implement" || note.At == 0 {
			t.Fatalf("note %d was not stamped: %+v", index, note)
		}
	}
	// No other tree was created: a child run's notes do not fork a log of its
	// own, which is the entire point of keying by the root.
	for _, id := range []string{"wave-1", "lane"} {
		dir, err := memory.Dir(h.app.workflowDataRoot(), id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("run %s got a memory tree of its own", id)
		}
	}
}

// Provenance is resolved from the session's own rows, not from anything the
// caller sends: the RPC input has no field for it at all, and the attempt and
// unit come off the row the calling thread is attached to.
func TestAddMemoryResolvesTheAttemptAndUnitFromTheSession(t *testing.T) {
	h := newMemoryHarness(t)
	h.campaign(t, "root")
	h.phase(t, store.WorkItemPhase{ItemID: "root", PhaseID: "implement", Attempt: 3, ThreadID: "phase-thread"})
	if err := h.app.store.CreateWorkItemUnits([]store.WorkItemUnit{{
		ItemID: "root", PhaseID: "review", Attempt: 2, UnitID: "lens-a", UnitIndex: 0,
		Kind: string(engine.UnitWork), ThreadID: "unit-thread", Status: "running", UnitAttempt: 1,
	}}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.app.WorkflowAgentAddMemory(
		h.phaseScope("root", "implement", "phase-thread"),
		WorkflowAgentMemoryInput{Kind: memory.KindWarning, Text: "from the phase"},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.WorkflowAgentAddMemory(
		h.phaseScope("root", "review", "unit-thread"),
		WorkflowAgentMemoryInput{Kind: memory.KindWarning, Text: "from the unit"},
	); err != nil {
		t.Fatal(err)
	}

	notes := h.notes(t, "root")
	if len(notes) != 2 {
		t.Fatalf("notes = %+v", notes)
	}
	if notes[0].Provenance.Attempt != 3 || notes[0].Provenance.UnitID != "" {
		t.Fatalf("phase provenance = %+v", notes[0].Provenance)
	}
	if notes[1].Provenance.UnitID != "lens-a" || notes[1].Provenance.Attempt != 2 {
		t.Fatalf("unit provenance = %+v", notes[1].Provenance)
	}
}

func TestAddMemoryRefusesABadKindAndAnOversizeNote(t *testing.T) {
	h := newMemoryHarness(t)
	h.campaign(t, "root")
	ctx := h.phaseScope("root", "implement", "thread")

	_, err := h.app.WorkflowAgentAddMemory(ctx, WorkflowAgentMemoryInput{Kind: "insight", Text: "x"})
	if err == nil || !strings.Contains(err.Error(), memory.KindList()) {
		t.Fatalf("bad kind error = %v", err)
	}
	_, err = h.app.WorkflowAgentAddMemory(ctx, WorkflowAgentMemoryInput{
		Kind: memory.KindLearning, Text: strings.Repeat("x", memory.MaxTextBytes+1),
	})
	if err == nil || !strings.Contains(err.Error(), "text") {
		t.Fatalf("oversize error = %v", err)
	}
	// A refused note leaves no log behind at all.
	if notes := h.notes(t, "root"); len(notes) != 0 {
		t.Fatalf("a refused note was recorded: %+v", notes)
	}
}

// A phase token writes its OWN tree. Naming another campaign's run is refused
// even when the phase holds `introspect`: reading another campaign's lessons is
// not what a project-wide read grant is for, and writing them never is.
func TestMemoryScopeIsConfinedToTheCallersOwnTree(t *testing.T) {
	h := newMemoryHarness(t)
	h.campaign(t, "root", "lane")
	h.campaign(t, "other-root")
	h.run(t, store.WorkItem{
		ID: "started-by-lane", WorkflowID: "child", State: string(engine.StateRunning),
		Source: workflowSourceAgent, SourceRef: "lane/implement/hash",
	})

	ctx := h.phaseScope("lane", "implement", "thread", def.GrantIntrospect, def.GrantStartRun)
	for _, verb := range []string{"add", "list"} {
		var err error
		if verb == "add" {
			_, err = h.app.WorkflowAgentAddMemory(ctx, WorkflowAgentMemoryInput{
				ItemID: "other-root", Kind: memory.KindWarning, Text: "not mine",
			})
		} else {
			_, err = h.app.WorkflowAgentListMemory(ctx, WorkflowAgentMemoryListInput{ItemID: "other-root"})
		}
		if err == nil || !strings.Contains(err.Error(), "may only act on the runs it started") {
			t.Fatalf("memory %s reached another campaign: %v", verb, err)
		}
	}
	if _, err := os.Stat(filepath.Join(h.app.workflowDataRoot(), memory.DirName, "other-root")); !os.IsNotExist(err) {
		t.Fatal("a refused write created the other campaign's tree")
	}

	// Its own run resolves with no id at all, and naming it explicitly is the
	// same answer — a phase's own run is not one it "started", so this is the
	// case a bare scopedRun check would have refused.
	for _, named := range []string{"", "lane"} {
		if _, err := h.app.WorkflowAgentAddMemory(ctx, WorkflowAgentMemoryInput{
			ItemID: named, Kind: memory.KindPattern, Text: "mine",
		}); err != nil {
			t.Fatalf("itemId=%q was refused: %v", named, err)
		}
	}
	// A run this phase started shares the same root, and is reachable too.
	if _, err := h.app.WorkflowAgentAddMemory(ctx, WorkflowAgentMemoryInput{
		ItemID: "started-by-lane", Kind: memory.KindHandoff, Text: "from the child",
	}); err != nil {
		t.Fatalf("a run this phase started was refused: %v", err)
	}
	if notes := h.notes(t, "root"); len(notes) != 2 {
		t.Fatalf("root tree = %d notes, want its own two", len(notes))
	}
}

// An interactive session has no run to infer, so it must name one — and it is
// confined to its own project like every other scoped read.
func TestMemoryFromAnInteractiveThreadRequiresARunId(t *testing.T) {
	h := newMemoryHarness(t)
	h.campaign(t, "root")
	ctx := transport.WithCallerScope(context.Background(), transport.CallerScope{
		Kind: transport.ScopeKindInteractive, ThreadID: "chat", ProjectID: defaultTestProjectID,
	})
	if _, err := h.app.WorkflowAgentAddMemory(ctx, WorkflowAgentMemoryInput{
		Kind: memory.KindWarning, Text: "x",
	}); err == nil || !strings.Contains(err.Error(), "run id is required") {
		t.Fatalf("error = %v", err)
	}
	if _, err := h.app.WorkflowAgentAddMemory(ctx, WorkflowAgentMemoryInput{
		ItemID: "root", Kind: memory.KindWarning, Text: "a human ruling",
	}); err != nil {
		t.Fatal(err)
	}
	notes := h.notes(t, "root")
	if len(notes) != 1 || notes[0].Provenance.PhaseID != "" {
		t.Fatalf("a human note carries a phase: %+v", notes)
	}
}

func TestListMemoryRendersProvenanceAndFiltersByKind(t *testing.T) {
	h := newMemoryHarness(t)
	h.campaign(t, "root")
	ctx := h.phaseScope("root", "implement", "thread")
	for _, draft := range []WorkflowAgentMemoryInput{
		{Kind: memory.KindWarning, Text: "the warning"},
		{Kind: memory.KindLearning, Text: "the learning", Files: []string{"pkg/a.go"}},
		{Kind: memory.KindWarning, Text: "another warning"},
	} {
		if _, err := h.app.WorkflowAgentAddMemory(ctx, draft); err != nil {
			t.Fatal(err)
		}
	}

	whole, err := h.app.WorkflowAgentListMemory(ctx, WorkflowAgentMemoryListInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(whole.Notes) != 3 || whole.Total != 3 || whole.RootID != "root" {
		t.Fatalf("whole log = %+v", whole)
	}
	if whole.Notes[0].Text != "the warning" {
		t.Fatalf("log is not oldest first: %+v", whole.Notes)
	}
	if whole.Notes[1].Files[0] != "pkg/a.go" || whole.Notes[1].Provenance.PhaseID != "implement" {
		t.Fatalf("note lost its files or provenance: %+v", whole.Notes[1])
	}

	filtered, err := h.app.WorkflowAgentListMemory(ctx, WorkflowAgentMemoryListInput{Kind: memory.KindWarning})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Notes) != 2 || filtered.Total != 3 {
		t.Fatalf("filtered = %d notes of %d, want 2 of 3", len(filtered.Notes), filtered.Total)
	}
	if _, err := h.app.WorkflowAgentListMemory(ctx, WorkflowAgentMemoryListInput{Kind: "bogus"}); err == nil {
		t.Fatal("an unknown kind filter was accepted")
	}

	// An empty campaign answers with an empty list rather than a null, and names
	// the log it read from.
	h.campaign(t, "quiet")
	empty, err := h.app.WorkflowAgentListMemory(
		h.phaseScope("quiet", "implement", "t"), WorkflowAgentMemoryListInput{})
	if err != nil {
		t.Fatal(err)
	}
	if empty.Notes == nil || len(empty.Notes) != 0 || empty.Path == "" {
		t.Fatalf("empty campaign = %+v", empty)
	}
}

// A torn line is counted, not hidden: a reader deciding whether the memory is
// complete has to know one was lost.
func TestListMemoryReportsUnreadableLines(t *testing.T) {
	h := newMemoryHarness(t)
	h.campaign(t, "root")
	ctx := h.phaseScope("root", "implement", "thread")
	if _, err := h.app.WorkflowAgentAddMemory(ctx, WorkflowAgentMemoryInput{
		Kind: memory.KindWarning, Text: "kept",
	}); err != nil {
		t.Fatal(err)
	}
	path, err := memory.NotesPath(h.app.workflowDataRoot(), "root")
	if err != nil {
		t.Fatal(err)
	}
	whole, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(whole, []byte(`{"kind":"warning","tex`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := h.app.WorkflowAgentListMemory(ctx, WorkflowAgentMemoryListInput{})
	if err != nil {
		t.Fatalf("a torn line was fatal: %v", err)
	}
	if len(result.Notes) != 1 || result.Skipped != 1 {
		t.Fatalf("result = %d notes, %d skipped", len(result.Notes), result.Skipped)
	}
}

// The digest an element's prompt carries is rendered live from the log, and a
// run whose tree cannot be resolved gets an empty digest rather than a failure:
// memory is context, and an element that runs without it still does the work.
func TestWorkflowMemoryDigestRendersTheTreeAndSurvivesAnUnresolvableRun(t *testing.T) {
	h := newMemoryHarness(t)
	h.campaign(t, "root", "lane")
	ctx := h.phaseScope("root", "plan", "thread")
	if _, err := h.app.WorkflowAgentAddMemory(ctx, WorkflowAgentMemoryInput{
		Kind: memory.KindHandoff, Text: "the wave-one handoff",
	}); err != nil {
		t.Fatal(err)
	}
	digest := string(h.app.workflowPromptAncestry("lane", def.Workflow{}).Memory)
	if !strings.Contains(digest, "the wave-one handoff") {
		t.Fatalf("a lane's digest does not carry the root's notes:\n%s", digest)
	}
	if !strings.Contains(digest, filepath.Join(memory.DirName, "root", memory.NotesFileName)) {
		t.Fatalf("digest does not name the log:\n%s", digest)
	}
	// A campaign with nothing recorded still gets the block: an element must
	// learn the mechanism exists before it can write the first note.
	h.campaign(t, "quiet")
	if quiet := string(h.app.workflowPromptAncestry("quiet", def.Workflow{}).Memory); !strings.Contains(quiet, "No notes recorded yet") {
		t.Fatalf("empty campaign digest = %q", quiet)
	}
	if missing := h.app.workflowPromptAncestry("no-such-run", def.Workflow{}).Memory; missing != "" {
		t.Fatalf("an unresolvable run produced a digest: %q", missing)
	}
}

// Lifecycle: the tree goes with the run RECORDS. Project deletion drops both;
// discard drops neither, because it leaves the rows in place.
func TestProjectDeletionRemovesTheMemoryTree(t *testing.T) {
	fixture := newProjectDeleteFixture(t, "memory-lifecycle")
	scope := transport.WithCallerScope(context.Background(), transport.CallerScope{
		Kind: transport.ScopeKindPhase, ThreadID: "phase-thread",
		ProjectID: fixture.project.ID, ItemID: fixture.child.ID, PhaseID: "run",
	})
	if _, err := fixture.app.WorkflowAgentAddMemory(scope, WorkflowAgentMemoryInput{
		Kind: memory.KindWarning, Text: "will be forgotten with the project",
	}); err != nil {
		t.Fatal(err)
	}
	dir, err := memory.Dir(fixture.app.workflowDataRoot(), fixture.root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("a child run's note did not create the root's tree: %v", err)
	}
	if _, err := fixture.app.DeleteProject(fixture.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the memory tree outlived the project's run records: %v", err)
	}
	// Removing a tree that was never created is not an error: a campaign that
	// recorded nothing has nothing to delete.
	if err := fixture.app.removeWorkflowMemoryTree("never-wrote"); err != nil {
		t.Fatalf("removing an absent tree failed: %v", err)
	}
}

// Discard leaves run records in place, so it leaves the memory alone — exactly
// as it leaves the narratives and envelopes of the campaign it discarded. The
// two lifecycles are one rule, and this is what holds them together.
func TestDiscardKeepsTheMemoryTree(t *testing.T) {
	fixture := newDiscardFixture(t, "memory-discard")
	scope := transport.WithCallerScope(context.Background(), transport.CallerScope{
		Kind: transport.ScopeKindPhase, ThreadID: "phase-thread",
		ProjectID: fixture.project.ID, ItemID: fixture.root.ID, PhaseID: "run",
	})
	if _, err := fixture.app.WorkflowAgentAddMemory(scope, WorkflowAgentMemoryInput{
		Kind: memory.KindLearning, Text: "survives the discard",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.app.WorkflowDiscardItem(fixture.root.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.app.store.GetWorkItem(fixture.root.ID); err != nil {
		t.Fatalf("discard deleted the run record, which changes what memory should do: %v", err)
	}
	notes, _, err := memory.ReadNotes(mustNotesPath(t, fixture.app, fixture.root.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0].Text != "survives the discard" {
		t.Fatalf("discard took the campaign's memory with it: %+v", notes)
	}
}

func mustNotesPath(t *testing.T, app *App, rootID string) string {
	t.Helper()
	path, err := memory.NotesPath(app.workflowDataRoot(), rootID)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

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

// A `read-only` phase runs in a session that denies every file write, so the
// narrative it is asked for arrives as prose and the runner lifts it into the
// file. Before this, a completed read-only run left every attempt directory
// empty, the wake pointed at a path nothing had created, and the triage seed read
// "narrative unavailable".
//
// The fixture mixes access levels in ONE run so both halves of the rule are
// proven against the same binary: the mock writes the narrative file whenever the
// prompt names a path, which only a writing phase's suffix does. So the read-only
// phase's file can only come from recovery, and the writing phase's can only come
// from the agent — and the recovery must leave that one alone.
func TestWorkflowNarrativeIsRecoveredOnlyWhenTheAgentWroteNone(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	projectRow = mustReloadProject(t, app.store, projectRow.ID)
	writeMixedAccessWorkflow(t, configRoot)
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, `
base_branch: main
reliability:
  watchdog: 1h
  backoff: [5ms]
`)
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": writeNarrativeClaude(t),
	}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)

	item, err := app.WorkflowStartRun(
		projectRow.ID, "mixed-access", "shared", "exercise narratives",
		json.RawMessage(`{}`), nil, "", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	t.Cleanup(func() {
		if item.WorktreePath != "" {
			_ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true)
		}
	})

	recovered := readAttemptNarrative(t, app, item.ID, "survey")
	if !strings.HasPrefix(recovered, workflowrunner.RecoveredNarrativeHeader+"\n\n") {
		t.Fatalf("read-only phase narrative is not marked as recovered:\n%s", recovered)
	}
	if !strings.Contains(recovered, narrativeMockProse) {
		t.Fatalf("read-only phase narrative lost the session's final message:\n%s", recovered)
	}

	authored := readAttemptNarrative(t, app, item.ID, "apply")
	if authored != narrativeMockAuthored {
		t.Fatalf("writing phase narrative = %q, want the agent's own file %q", authored, narrativeMockAuthored)
	}
	if strings.Contains(authored, workflowrunner.RecoveredNarrativeHeader) {
		t.Fatalf("recovery overwrote the agent's own narrative:\n%s", authored)
	}
}

// The whole reason `narrative` is a control field: Codex constrains EVERY
// assistant message of a schema'd turn, so a read-only element cannot send prose
// at all and the account has to ride in the envelope. End to end, the field
// becomes the attempt's narrative file — authored, not recovered — and never
// reaches the persisted envelope the gate and the wake read.
func TestWorkflowEnvelopeNarrativeBecomesTheAttemptNarrative(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	projectRow = mustReloadProject(t, app.store, projectRow.ID)
	writeUndeclaredAccessWorkflow(t, configRoot)
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, `
base_branch: main
reliability:
  watchdog: 1h
  backoff: [5ms]
`)
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": writeEnvelopeNarrativeClaude(t),
	}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)

	item, err := app.WorkflowStartRun(
		projectRow.ID, "undeclared-access", "shared", "exercise envelope narratives",
		json.RawMessage(`{}`), nil, "", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	t.Cleanup(func() {
		if item.WorktreePath != "" {
			_ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true)
		}
	})

	narrative := readAttemptNarrative(t, app, item.ID, "survey")
	if narrative != envelopeNarrativeAccount+"\n" {
		t.Fatalf("narrative = %q, want the envelope's own account", narrative)
	}
	if strings.Contains(narrative, workflowrunner.RecoveredNarrativeHeader) {
		t.Fatalf("an authored envelope narrative was marked as recovered:\n%s", narrative)
	}
	if strings.Contains(narrative, narrativeMockProse) {
		t.Fatalf("the envelope field lost to the session's prose:\n%s", narrative)
	}
	phases := listWorkflowPhases(t, app, item.ID)
	if len(phases) != 1 {
		t.Fatalf("phases = %+v", phases)
	}
	persisted := string(phases[0].OutputEnvelope)
	if strings.Contains(persisted, "narrative") || strings.Contains(persisted, envelopeNarrativeAccount) {
		t.Fatalf("the persisted envelope carried prose: %s", persisted)
	}
	if outputs := decodeEnvelopeOutputs(t, phases[0].OutputEnvelope); outputs["report"] != "deliverable.md" {
		t.Fatalf("stripping damaged the outputs = %v", outputs)
	}
}

const envelopeNarrativeAccount = "I surveyed the resolver and found one binding"

// writeEnvelopeNarrativeClaude is a mock read-only element that does what the
// suffix now asks: it puts its account in the envelope's `narrative` field. It
// also speaks prose, so the authored field has to beat the D39 recovery rather
// than merely fill in for it.
func writeEnvelopeNarrativeClaude(t *testing.T) string {
	t.Helper()
	status := `{"status":"done","outputs":{"report":"deliverable.md"},"question":null,"reason":null,` +
		`"narrative":"` + envelopeNarrativeAccount + `"}`
	script := `#!/bin/bash
while IFS= read -r line; do
  printf '%s\n' '{"type":"system","subtype":"init","session_id":"envelope-narrative","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
  printf '%s\n' '{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"` + narrativeMockProse + `"}]}}'
  printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":` + status + `}'
done
`
	return writeExecutable(t, "envelope-narrative-claude.sh", script)
}

func readAttemptNarrative(t *testing.T, app *App, itemID, phaseID string) string {
	t.Helper()
	path, err := workflowrunner.NarrativePath(app.workflowDataRoot(), itemID, phaseID, 1)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s narrative: %v", phaseID, err)
	}
	return string(contents)
}

const (
	narrativeMockProse    = "I surveyed the callers and found two"
	narrativeMockAuthored = "the agent wrote this itself\n"
)

// writeNarrativeClaude is a mock that always speaks prose and writes the
// narrative file only when the prompt names one — which is exactly the
// difference between the write-access suffix and the read-only one.
func writeNarrativeClaude(t *testing.T) string {
	t.Helper()
	status := `{"status":"done","outputs":{"report":"deliverable.md"},"question":null,"reason":null}`
	script := `#!/bin/bash
while IFS= read -r line; do
  narrative=$(printf '%s' "$line" | grep -o '/[^" ]*/narrative\.md' | head -1)
  if [ -n "$narrative" ]; then
    printf '` + narrativeMockAuthored + `' > "$narrative"
  fi
  printf '%s\n' '{"type":"system","subtype":"init","session_id":"narrative","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
  printf '%s\n' '{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"` + narrativeMockProse + `"}]}}'
  printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":` + status + `}'
done
`
	return writeExecutable(t, "narrative-claude.sh", script)
}
