package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// The run map's reads answer for a TREE, so every assertion here is about what
// a caller cannot do for itself: membership, grouping, ordering, the
// projections' refusal to carry payload columns, and — the reason the walk
// moved into SQLite — what corrupt linkage does to a read that cannot walk it
// one row at a time.

func seedRunMapTree(t *testing.T, s *Store) {
	t.Helper()
	root := testWorkItem("root", "project-a", "running", 10)
	root.Snapshot = json.RawMessage(`{"workflow":{"id":"campaign","phases":[]}}`)
	wave := testWorkItem("wave", "project-a", "needs-human", 20)
	wave.ParentItemID = "root"
	wave.ParentPhaseID = "next"
	wave.ParentAttempt = 1
	wave.CallDepth = 1
	other := testWorkItem("other-tree", "project-a", "running", 30)
	for _, item := range []WorkItem{root, wave, other} {
		if err := s.CreateWorkItem(item); err != nil {
			t.Fatalf("create %s: %v", item.ID, err)
		}
	}

	takeover, err := json.Marshal(map[string]any{"kind": "taken-over", "at": 5})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := json.Marshal(map[string]any{"decision": "approve", "note": "looks right"})
	if err != nil {
		t.Fatal(err)
	}
	phases := []WorkItemPhase{
		// Deliberately inserted out of order so the read's ordering is doing the
		// work rather than the insertion order.
		{ItemID: "wave", PhaseID: "port", Attempt: 1, Status: "running", StartedAt: 300},
		{ItemID: "root", PhaseID: "plan", Attempt: 2, Status: "completed", StartedAt: 120, EndedAt: 130, Intervention: gate},
		{ItemID: "root", PhaseID: "plan", Attempt: 1, Status: "parked", StartedAt: 100, EndedAt: 110,
			ParkCause: "worktree would not cut", Intervention: takeover, ThreadID: "thread-plan"},
		{ItemID: "other-tree", PhaseID: "plan", Attempt: 1, Status: "running", StartedAt: 400},
	}
	for _, phase := range phases {
		if err := s.CreateWorkItemPhase(phase); err != nil {
			t.Fatalf("create phase %s/%s/%d: %v", phase.ItemID, phase.PhaseID, phase.Attempt, err)
		}
	}

	units := []WorkItemUnit{
		{ItemID: "wave", PhaseID: "port", Attempt: 1, UnitID: "port-join", UnitIndex: 2,
			Kind: WorkItemUnitKindJoin, Provider: "claude", Status: WorkItemUnitPending, UnitAttempt: 1},
		{ItemID: "wave", PhaseID: "port", Attempt: 1, UnitID: "port-1", UnitIndex: 1,
			Kind: WorkItemUnitKindUnit, Provider: "codex", Status: WorkItemUnitPending, UnitAttempt: 1},
		{ItemID: "wave", PhaseID: "port", Attempt: 1, UnitID: "port-0", UnitIndex: 0,
			Kind: WorkItemUnitKindUnit, Provider: "claude", Status: WorkItemUnitRunning, UnitAttempt: 2,
			ThreadID: "thread-port-0", StartedAt: 310},
		{ItemID: "other-tree", PhaseID: "port", Attempt: 1, UnitID: "port-0", UnitIndex: 0,
			Kind: WorkItemUnitKindUnit, Provider: "claude", Status: WorkItemUnitPending, UnitAttempt: 1},
	}
	if err := s.CreateWorkItemUnits(units); err != nil {
		t.Fatalf("create units: %v", err)
	}
}

// seedCalledRun writes one called run, which is the only shape the CHECKs admit
// for a parented row: linkage is all-or-nothing.
func seedCalledRun(t *testing.T, s *Store, id, parentID string, callDepth int, createdAt int64) {
	t.Helper()
	item := testWorkItem(id, "project-a", "running", createdAt)
	item.Source = "call"
	item.ParentItemID = parentID
	item.ParentPhaseID = "next"
	item.ParentAttempt = 1
	item.CallDepth = callDepth
	if err := s.CreateWorkItem(item); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
}

// collectTreeRuns is the tree scan with the visitor these assertions need: the
// production caller projects each run and drops its snapshot, so the streaming
// shape is what is under test, and a slice is only how a test reads it back.
func collectTreeRuns(t *testing.T, s *Store, rootID string, maxDepth, maxMembers int) ([]WorkItemTreeRun, error) {
	t.Helper()
	runs := make([]WorkItemTreeRun, 0)
	_, err := s.ReadWorkItemTree(t.Context(), rootID, maxDepth, maxMembers, func(run WorkItemTreeRun) error {
		runs = append(runs, run)
		return nil
	})
	return runs, err
}

func treeRunIDs(runs []WorkItemTreeRun) []string {
	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.ID)
	}
	return ids
}

func TestReadWorkItemTreeCarriesSnapshotsAndStopsAtTheTree(t *testing.T) {
	s := newTestStore(t)
	seedRunMapTree(t, s)

	runs, err := collectTreeRuns(t, s, "root", 8, 64)
	if err != nil {
		t.Fatalf("list tree runs: %v", err)
	}
	if got := treeRunIDs(runs); len(got) != 2 || got[0] != "root" || got[1] != "wave" {
		t.Fatalf("tree runs = %v, want root then wave and nothing from the other tree", got)
	}
	if len(runs[0].Snapshot) == 0 {
		t.Fatal("the run map's read dropped the frozen snapshot it projects skeletons from")
	}
	if len(runs[0].Budget) == 0 {
		t.Fatal("the run map's read dropped the budget the root's ceiling comes from")
	}
	if runs[1].ParentItemID != "root" || runs[1].ParentPhaseID != "next" || runs[1].CallDepth != 1 {
		t.Fatalf("call linkage = %#v", runs[1])
	}
	if runs[1].State != "needs-human" {
		t.Fatalf("state = %q", runs[1].State)
	}
	// A one-run tree is a real tree, not an error.
	if solo, err := collectTreeRuns(t, s, "other-tree", 8, 64); err != nil || len(solo) != 1 {
		t.Fatalf("childless root = %#v, %v", solo, err)
	}
	if _, err := collectTreeRuns(t, s, "absent", 8, 64); err == nil {
		t.Fatal("a root with no row was answered as an empty tree")
	}
	if _, err := collectTreeRuns(t, s, "root", 0, 64); err == nil {
		t.Fatal("an unbounded depth was accepted")
	}
	if _, err := collectTreeRuns(t, s, "root", 8, 0); err == nil {
		t.Fatal("an unbounded member cap was accepted")
	}
}

func TestReadWorkItemTreeOrdersParentsBeforeTheirChildren(t *testing.T) {
	s := newTestStore(t)
	root := testWorkItem("root", "project-a", "running", 10)
	if err := s.CreateWorkItem(root); err != nil {
		t.Fatal(err)
	}
	// Created newest-first down the chain and with LYING call_depths further
	// down, so ordering that leaned on either would put a child before its
	// parent. (The schema only guarantees the column is non-zero exactly when a
	// parent reference exists — not that it counts the calls.)
	seedCalledRun(t, s, "wave-1", "root", 3, 90)
	seedCalledRun(t, s, "wave-2", "wave-1", 1, 80)
	seedCalledRun(t, s, "lane", "wave-2", 1, 70)
	seedCalledRun(t, s, "audit", "wave-1", 2, 95)

	runs, err := collectTreeRuns(t, s, "root", 8, 64)
	if err != nil {
		t.Fatalf("list tree runs: %v", err)
	}
	if got := treeRunIDs(runs); len(got) != 5 {
		t.Fatalf("tree runs = %v", got)
	}
	seen := map[string]bool{}
	for _, run := range runs {
		if run.ParentItemID != "" && !seen[run.ParentItemID] {
			t.Fatalf("run %s arrived before its parent %s (order %v)", run.ID, run.ParentItemID, treeRunIDs(runs))
		}
		seen[run.ID] = true
	}
	// Within a level, creation order — the order the calls were made in.
	if runs[2].ID != "wave-2" || runs[3].ID != "audit" {
		t.Fatalf("within-level order = %v", treeRunIDs(runs))
	}
}

func TestWorkItemTreeRootResolvesFromAnyMemberAndTreatsAnOrphanAsARoot(t *testing.T) {
	s := newTestStore(t)
	root := testWorkItem("root", "project-a", "running", 10)
	if err := s.CreateWorkItem(root); err != nil {
		t.Fatal(err)
	}
	seedCalledRun(t, s, "wave", "root", 1, 20)
	seedCalledRun(t, s, "lane", "wave", 2, 30)
	// A run whose named parent's row is gone. The linkage CHECKs keep the
	// reference itself intact, so the read must answer with the run rather than
	// with nothing.
	seedCalledRun(t, s, "orphan", "deleted-parent", 4, 40)

	for _, from := range []string{"root", "wave", "lane"} {
		node, err := s.WorkItemTreeRoot(from, 8)
		if err != nil {
			t.Fatalf("root from %s: %v", from, err)
		}
		if node.ID != "root" || node.ParentItemID != "" {
			t.Fatalf("root from %s = %#v", from, node)
		}
	}

	node, err := s.WorkItemTreeRoot("orphan", 8)
	if err != nil {
		t.Fatalf("orphan root: %v", err)
	}
	// The dangling reference SURVIVES on the answer: it is how the caller tells
	// an orphan from a true root and logs the fact.
	if node.ID != "orphan" || node.ParentItemID != "deleted-parent" || node.CallDepth != 4 {
		t.Fatalf("orphan root = %#v", node)
	}

	if _, err := s.WorkItemTreeRoot("no-such-run", 8); err == nil {
		t.Fatal("a run with no row resolved to a tree of its own")
	}
	if _, err := s.WorkItemTreeRoot("", 8); err == nil {
		t.Fatal("an empty id was accepted")
	}
	if _, err := s.WorkItemTreeRoot("lane", 0); err == nil {
		t.Fatal("an unbounded depth was accepted")
	}
}

func TestWorkItemTreeReadsRefuseLinkageDeeperThanTheCap(t *testing.T) {
	s := newTestStore(t)
	root := testWorkItem("root", "project-a", "running", 10)
	if err := s.CreateWorkItem(root); err != nil {
		t.Fatal(err)
	}
	parent := "root"
	for depth := 1; depth <= 6; depth++ {
		id := fmt.Sprintf("wave-%d", depth)
		seedCalledRun(t, s, id, parent, depth, int64(10+depth))
		parent = id
	}

	if _, err := s.WorkItemTreeRoot("wave-6", 3); !errors.Is(err, ErrWorkItemTreeTooDeep) {
		t.Fatalf("ancestor chain past the cap = %v, want ErrWorkItemTreeTooDeep", err)
	}
	if node, err := s.WorkItemTreeRoot("wave-6", 6); err != nil || node.ID != "root" {
		t.Fatalf("chain exactly at the cap = %#v, %v", node, err)
	}
	if _, err := collectTreeRuns(t, s, "root", 3, 64); !errors.Is(err, ErrWorkItemTreeTooDeep) {
		t.Fatalf("tree deeper than the cap = %v, want ErrWorkItemTreeTooDeep", err)
	}
	if runs, err := collectTreeRuns(t, s, "root", 6, 64); err != nil || len(runs) != 7 {
		t.Fatalf("tree exactly at the cap = %v, %v", treeRunIDs(runs), err)
	}
}

func TestWorkItemTreeReadsTerminateOnALinkageCycle(t *testing.T) {
	s := newTestStore(t)
	// The schema's CHECKs make a parent reference all-or-nothing; they do not
	// make it acyclic, and there is no FK to refuse a forward reference. So this
	// is writable, and both reads have to survive it.
	seedCalledRun(t, s, "a", "b", 1, 10)
	seedCalledRun(t, s, "b", "a", 1, 20)

	// Upward: every step is a new depth, so the depth cap is what terminates it —
	// and a run whose ancestry never ends has no root to answer with.
	if _, err := s.WorkItemTreeRoot("a", 16); !errors.Is(err, ErrWorkItemTreeTooDeep) {
		t.Fatalf("cyclic ancestor chain = %v, want ErrWorkItemTreeTooDeep", err)
	}

	// Downward: the walk TERMINATES — MIN(depth) collapses the several depths the
	// cycle stamps on one id, so it neither hangs nor duplicates — and then the
	// read REFUSES, because "a parent is always seen before its children" has no
	// witness here: `a`'s parent is `b`, which the anchor-first order can only
	// place after it. Answering with that order would hand every consumer a tree
	// built one pass at a time a child whose parent it has not seen.
	_, err := collectTreeRuns(t, s, "a", 16, 64)
	if !errors.Is(err, ErrWorkItemTreeCyclicLinkage) {
		t.Fatalf("cyclic tree = %v, want ErrWorkItemTreeCyclicLinkage", err)
	}

	// A run OUTSIDE the cycle keeps its own tree: `parent_item_id` is one column,
	// so every member of a cycle has its parent inside it, and no acyclic anchor
	// can walk into one. That is why the app never sees this refusal — its anchor
	// comes from the upward walk, which refuses an in-cycle run first — and why
	// the downward refusal still has to exist: the method is exported, and its
	// ordering promise is made to every caller rather than to that one.
	outside := testWorkItem("outside", "project-a", "running", 5)
	if err := s.CreateWorkItem(outside); err != nil {
		t.Fatal(err)
	}
	if runs, err := collectTreeRuns(t, s, "outside", 16, 64); err != nil || len(runs) != 1 {
		t.Fatalf("tree beside a cycle = %v, %v", treeRunIDs(runs), err)
	}
}

func TestReadWorkItemTreeRefusesARunThatIsItsOwnParent(t *testing.T) {
	s := newTestStore(t)
	// The degenerate cycle, and the one the "a parent arrives before its child"
	// bookkeeping cannot catch on its own: the row IS its parent, so it arrives
	// exactly once, and set semantics collapse the walk to that single member. A
	// consumer nesting it under the parent the row names would nest it under
	// itself.
	seedCalledRun(t, s, "loop", "loop", 1, 10)

	if _, err := s.WorkItemTreeRoot("loop", 16); !errors.Is(err, ErrWorkItemTreeTooDeep) {
		t.Fatalf("self-parented ancestry = %v, want ErrWorkItemTreeTooDeep", err)
	}
	_, err := collectTreeRuns(t, s, "loop", 16, 64)
	if !errors.Is(err, ErrWorkItemTreeCyclicLinkage) {
		t.Fatalf("self-parented tree = %v, want ErrWorkItemTreeCyclicLinkage", err)
	}
}

func TestReadWorkItemTreeRefusesATreeOverTheMemberCap(t *testing.T) {
	s := newTestStore(t)
	root := testWorkItem("root", "project-a", "running", 10)
	if err := s.CreateWorkItem(root); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 5; index++ {
		seedCalledRun(t, s, fmt.Sprintf("wave-%d", index), "root", 1, int64(20+index))
	}

	if runs, err := collectTreeRuns(t, s, "root", 8, 6); err != nil || len(runs) != 6 {
		t.Fatalf("tree exactly at the cap = %v, %v", treeRunIDs(runs), err)
	}
	_, err := collectTreeRuns(t, s, "root", 8, 5)
	if !errors.Is(err, ErrWorkItemTreeTooLarge) {
		t.Fatalf("over-cap tree error = %v, want ErrWorkItemTreeTooLarge", err)
	}
}

func TestListWorkItemTreeAutoResumesReportsOnlyArmedRunsInTheTree(t *testing.T) {
	s := newTestStore(t)
	seedRunMapTree(t, s)
	if err := s.SetWorkItemAutoResumeAt("wave", 9_000); err != nil {
		t.Fatalf("arm wave: %v", err)
	}
	if err := s.SetWorkItemAutoResumeAt("other-tree", 8_000); err != nil {
		t.Fatalf("arm other tree: %v", err)
	}

	resumes, err := listWorkItemTreeAutoResumes(s.reader(), "root")
	if err != nil {
		t.Fatalf("list auto resumes: %v", err)
	}
	if len(resumes) != 1 || resumes[0].ItemID != "wave" || resumes[0].At != 9_000 {
		t.Fatalf("auto resumes = %#v, want only the armed run inside the tree", resumes)
	}
	if err := s.SetWorkItemAutoResumeAt("wave", 0); err != nil {
		t.Fatalf("disarm wave: %v", err)
	}
	if resumes, err = listWorkItemTreeAutoResumes(s.reader(), "root"); err != nil || len(resumes) != 0 {
		t.Fatalf("disarmed run = %#v, %v", resumes, err)
	}
	if _, err := listWorkItemTreeAutoResumes(s.reader(), ""); err == nil {
		t.Fatal("an empty root id was accepted")
	}
}

func TestListWorkItemTreePhaseStatusesGroupsByRunAndProjectsInterventionKind(t *testing.T) {
	s := newTestStore(t)
	seedRunMapTree(t, s)

	statuses, err := listWorkItemTreePhaseStatuses(s.reader(), "root")
	if err != nil {
		t.Fatalf("list phase statuses: %v", err)
	}
	if len(statuses) != 3 {
		t.Fatalf("phase statuses = %#v, want the tree's attempts and nothing from the other tree", statuses)
	}
	if statuses[0].ItemID != "root" || statuses[0].Attempt != 1 ||
		statuses[1].ItemID != "root" || statuses[1].Attempt != 2 ||
		statuses[2].ItemID != "wave" {
		t.Fatalf("phase order = %#v, want per-run attempt order", statuses)
	}
	first := statuses[0]
	if first.InterventionKind != "taken-over" || first.ParkCause != "worktree would not cut" ||
		first.ThreadID != "thread-plan" || first.StartedAt != 100 || first.EndedAt != 110 {
		t.Fatalf("parked attempt = %#v", first)
	}
	// A human gate decision records a decision and a note, no kind: the map must
	// read that as "no typed intervention" rather than as malformed JSON.
	if statuses[1].InterventionKind != "" {
		t.Fatalf("gate-decision intervention kind = %q, want empty", statuses[1].InterventionKind)
	}
	// An attempt with no intervention at all holds the empty string, which is
	// not JSON — the read must not choke on it.
	if statuses[2].InterventionKind != "" {
		t.Fatalf("untouched attempt kind = %q, want empty", statuses[2].InterventionKind)
	}
	if _, err := listWorkItemTreePhaseStatuses(s.reader(), "  "); err != nil {
		t.Fatalf("an id no run holds must read as an empty tree: %v", err)
	}
	if _, err := listWorkItemTreePhaseStatuses(s.reader(), ""); err == nil {
		t.Fatal("an empty root id was accepted")
	}
}

func TestListWorkItemTreeUnitStatusesGroupsByRunInLaunchOrder(t *testing.T) {
	s := newTestStore(t)
	seedRunMapTree(t, s)

	statuses, err := listWorkItemTreeUnitStatuses(s.reader(), "root")
	if err != nil {
		t.Fatalf("list unit statuses: %v", err)
	}
	if len(statuses) != 3 {
		t.Fatalf("unit statuses = %#v, want only the wave's units", statuses)
	}
	for index, want := range []string{"port-0", "port-1", "port-join"} {
		if statuses[index].UnitID != want || statuses[index].ItemID != "wave" {
			t.Fatalf("unit %d = %#v, want %s", index, statuses[index], want)
		}
	}
	running := statuses[0]
	if running.Status != WorkItemUnitRunning || running.UnitAttempt != 2 || running.Provider != "claude" ||
		running.ThreadID != "thread-port-0" || running.StartedAt != 310 || running.Kind != WorkItemUnitKindUnit {
		t.Fatalf("running unit = %#v", running)
	}
	if statuses[2].Kind != WorkItemUnitKindJoin {
		t.Fatalf("join kind = %q", statuses[2].Kind)
	}
	if _, err := listWorkItemTreeUnitStatuses(s.reader(), ""); err == nil {
		t.Fatal("an empty root id was accepted")
	}
}
