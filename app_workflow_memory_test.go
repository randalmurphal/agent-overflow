package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/memory"
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
