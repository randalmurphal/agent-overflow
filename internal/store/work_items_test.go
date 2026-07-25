package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
)

func testWorkItem(id, projectID, state string, createdAt int64) WorkItem {
	return WorkItem{
		ID: id, ProjectID: projectID, Goal: "ship it", WorkflowID: "build",
		WorkflowScope: "project", State: state,
		Seeds: json.RawMessage(`{"ticket":"AO-1"}`), StepMode: true,
		Budget: json.RawMessage(`{"kind":"tokens","limit":1000}`),
		Source: "manual", CreatedAt: createdAt,
	}
}

func TestWorkItemCRUDListAndTransitions(t *testing.T) {
	s := newTestStore(t)
	items := []WorkItem{
		testWorkItem("second", "project-a", "running", 20),
		testWorkItem("first", "project-a", "running", 10),
		testWorkItem("parked", "project-a", "needs-human", 30),
		testWorkItem("other-project", "project-b", "running", 1),
	}
	for _, item := range items {
		if err := s.CreateWorkItem(item); err != nil {
			t.Fatalf("create %s: %v", item.ID, err)
		}
	}

	running, err := s.ListWorkItems(WorkItemListFilter{ProjectID: "project-a", States: []string{"running"}})
	if err != nil {
		t.Fatalf("list running: %v", err)
	}
	if len(running) != 2 || running[0].ID != "first" || running[1].ID != "second" {
		t.Fatalf("running order = %#v", running)
	}
	all, err := s.ListWorkItems(WorkItemListFilter{ProjectID: "project-a"})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all project-a items = %d, want 3", len(all))
	}
	everyProject, err := s.ListWorkItemSummaries(WorkItemListFilter{})
	if err != nil {
		t.Fatalf("list every project: %v", err)
	}
	if len(everyProject) != 4 ||
		everyProject[0].ID != "other-project" || everyProject[1].ID != "first" ||
		everyProject[2].ID != "second" || everyProject[3].ID != "parked" {
		t.Fatalf("every-project order = %#v", everyProject)
	}
	crossProjectRunning, err := s.ListWorkItems(WorkItemListFilter{States: []string{"running"}})
	if err != nil {
		t.Fatalf("list cross-project running: %v", err)
	}
	if len(crossProjectRunning) != 3 {
		t.Fatalf("cross-project running = %d, want 3", len(crossProjectRunning))
	}

	snapshot := json.RawMessage(`{"id":"build","version":1}`)
	if err := s.UpdateWorkItemRunStart("first", snapshot, "/tmp/wt", "ao/item", "main", 40); err != nil {
		t.Fatalf("run start: %v", err)
	}
	if err := s.UpdateWorkItemState("first", "running", "", 0); err != nil {
		t.Fatalf("set running: %v", err)
	}
	if err := s.UpdateWorkItemState("first", "needs-human", "gate", 50); err != nil {
		t.Fatalf("set needs-human: %v", err)
	}
	disposition := json.RawMessage(`{"action":"merged","policy":"manual","at":50}`)
	digest := json.RawMessage(`{"whatHappened":"Paused.","whatItNeeds":"Review."}`)
	if err := s.UpdateWorkItemDisposition("first", disposition); err != nil {
		t.Fatalf("set disposition: %v", err)
	}
	if err := s.UpdateWorkItemDigest("first", digest); err != nil {
		t.Fatalf("set digest: %v", err)
	}
	got, err := s.GetWorkItem("first")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != "needs-human" || got.Reason != "gate" || got.EndedAt != 50 ||
		got.WorktreePath != "/tmp/wt" || got.Branch != "ao/item" || got.BaseBranch != "main" ||
		got.StartedAt != 40 || string(got.Snapshot) != string(snapshot) || !got.StepMode ||
		string(got.Disposition) != string(disposition) || string(got.Digest) != string(digest) {
		t.Fatalf("updated item = %#v", got)
	}
	if err := s.UpdateWorkItemState("missing", "done", "", 60); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing state update error = %v, want sql.ErrNoRows", err)
	}
	if err := s.UpdateWorkItemRunStart("missing", snapshot, "/tmp/wt", "ao/item", "main", 40); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing run-start update error = %v, want sql.ErrNoRows", err)
	}
	if count, err := s.CountWorkItemsInStates("running", "needs-human"); err != nil || count != 4 {
		t.Fatalf("running/needs-human count = %d err=%v, want 4", count, err)
	}
	if count, err := s.CountProjectWorkItemsInStates("project-a", "running"); err != nil || count != 1 {
		t.Fatalf("project-a running count = %d err=%v, want 1", count, err)
	}
}

func TestWorkItemTriageAssociationAndPhaseThreadLookup(t *testing.T) {
	s := newTestStore(t)
	item := testWorkItem("triage-link", defaultTestProjectID, "needs-human", 1)
	item.Reason = "taken-over"
	if err := s.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWorkItemPhase(WorkItemPhase{
		ItemID: item.ID, PhaseID: "work", Attempt: 1, ThreadID: "phase-thread",
		Status: "parked", StartedAt: 2, EndedAt: 3,
	}); err != nil {
		t.Fatal(err)
	}
	triageThread := Thread{
		ID: "triage-thread", ProjectID: defaultTestProjectID, Title: "Triage",
		Provider: "codex", Model: "gpt-5", WorkspacePath: "/tmp/project",
		Mode: "workflow-triage", CreatedAt: 4, UpdatedAt: 4,
	}
	if err := s.CreateWorkItemTriageThread(item.ID, triageThread); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TriageThreadID != "triage-thread" || got.Reason != "taken-over" {
		t.Fatalf("work item link = %+v", got)
	}
	owner, err := s.GetWorkItemByPhaseThread("phase-thread")
	if err != nil {
		t.Fatal(err)
	}
	if owner.ID != item.ID || owner.TriageThreadID != "triage-thread" {
		t.Fatalf("phase thread owner = %+v", owner)
	}
	orphan := triageThread
	orphan.ID = "orphan-triage-thread"
	if err := s.CreateWorkItemTriageThread("missing-item", orphan); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing item atomic create error = %v, want sql.ErrNoRows", err)
	}
	if _, err := s.GetThread(orphan.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("atomic rollback left orphan thread: %v", err)
	}
}

func TestGetWorkItemAttentionContextOmitsSnapshotAndPhaseDiagnostics(t *testing.T) {
	s := newTestStore(t)
	item := testWorkItem("attention", "project-a", "needs-human", 1)
	item.Goal = "Review checks"
	item.Reason = "gate"
	item.WorktreePath = "/tmp/worktree"
	item.Digest = json.RawMessage(`{"whatHappened":"paused","whatItNeeds":"review"}`)
	item.Snapshot = json.RawMessage(`{"workflow":{"phases":[{"id":"verify","driver":"tool","check":"go-test","prompt":"large payload"}]}}`)
	if err := s.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWorkItemPhase(WorkItemPhase{
		ItemID: item.ID, PhaseID: "verify", Attempt: 1, Status: "parked",
		InputEnvelope:  json.RawMessage(`{"large":"input"}`),
		OutputEnvelope: json.RawMessage(`{"status":"question","question":"Continue?"}`),
		GateTrace:      json.RawMessage(`{"large":"trace"}`), NarrativePath: "/tmp/path",
		StartedAt: 2, EndedAt: 3,
	}); err != nil {
		t.Fatal(err)
	}
	context, err := s.GetWorkItemAttentionContext(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if context.Item.ID != item.ID || context.Item.ProjectID != item.ProjectID ||
		context.Item.Goal != item.Goal || context.Item.Reason != item.Reason ||
		context.Item.WorktreePath != item.WorktreePath || context.PhaseID != "verify" ||
		context.Check != "go-test" || string(context.OutputEnvelope) != `{"status":"question","question":"Continue?"}` {
		t.Fatalf("attention context = %+v", context)
	}
	if len(context.Item.Snapshot) != 0 || len(context.Item.Seeds) != 0 || len(context.Item.Budget) != 0 {
		t.Fatalf("attention context hydrated heavy item fields: %+v", context.Item)
	}
}

func TestUpdateWorkItemWorkspace(t *testing.T) {
	s := newTestStore(t)
	item := testWorkItem("workspace", "project-a", "running", 1)
	if err := s.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateWorkItemWorkspace(item.ID, "/tmp/worktree", "ao-workflow-build", "main"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorktreePath != "/tmp/worktree" || got.Branch != "ao-workflow-build" || got.BaseBranch != "main" {
		t.Fatalf("workspace fields = (%q, %q, %q)", got.WorktreePath, got.Branch, got.BaseBranch)
	}
	if err := s.UpdateWorkItemWorkspace("missing", "/tmp/worktree", "branch", "main"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing workspace update error = %v, want sql.ErrNoRows", err)
	}
}

func TestWorkItemSummaryOmitsHeavyPayloads(t *testing.T) {
	s := newTestStore(t)
	first := testWorkItem("first", "project-a", "running", 1)
	first.Snapshot = json.RawMessage(`{"workflow":{"prompt":"large"}}`)
	first.Digest = json.RawMessage(`{"whatHappened":"heavy detail"}`)
	first.Disposition = json.RawMessage(`{"action":"merged","policy":"manual","at":2}`)
	if err := s.CreateWorkItem(first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	second := testWorkItem("second", "project-a", "done", 2)
	if err := s.CreateWorkItem(second); err != nil {
		t.Fatalf("create second: %v", err)
	}

	summaries, err := s.ListWorkItemSummaries(WorkItemListFilter{ProjectID: "project-a"})
	if err != nil {
		t.Fatalf("list summaries: %v", err)
	}
	if len(summaries) != 2 || summaries[0].ID != "first" || summaries[1].ID != "second" {
		t.Fatalf("summary order = %#v", summaries)
	}
	if len(summaries[0].Snapshot) != 0 || len(summaries[0].Seeds) != 0 || len(summaries[0].Budget) != 0 || len(summaries[0].Digest) != 0 {
		t.Fatalf("summary includes heavy payloads: %#v", summaries[0])
	}
	if string(summaries[0].Disposition) != string(first.Disposition) {
		t.Fatalf("summary disposition = %s, want %s", summaries[0].Disposition, first.Disposition)
	}
	if summaries[0].Goal != first.Goal || summaries[0].State != first.State {
		t.Fatalf("summary lost list fields: %#v", summaries[0])
	}
}

func TestListWorkItemsUnresolvedExcludesDisposedRegardlessOfState(t *testing.T) {
	s := newTestStore(t)
	receipt := json.RawMessage(`{"action":"discarded","policy":"manual","at":10}`)
	items := []WorkItem{
		testWorkItem("running", "project-a", "running", 2),
		testWorkItem("needs-human", "project-a", "needs-human", 3),
		testWorkItem("done", "project-a", "done", 4),
		testWorkItem("failed", "project-a", "failed", 5),
		testWorkItem("cancelled", "project-a", "cancelled", 6),
		testWorkItem("other-project", "project-b", "failed", 7),
	}
	items[3].Disposition = receipt
	for _, item := range items {
		if err := s.CreateWorkItem(item); err != nil {
			t.Fatalf("create %s: %v", item.ID, err)
		}
	}

	got, err := s.ListWorkItemSummaries(WorkItemListFilter{
		ProjectID: "project-a", UnresolvedOnly: true,
	})
	if err != nil {
		t.Fatalf("list unresolved: %v", err)
	}
	want := []string{"running", "needs-human", "done"}
	if len(got) != len(want) {
		t.Fatalf("unresolved = %#v, want ids %v", got, want)
	}
	for index, id := range want {
		if got[index].ID != id {
			t.Fatalf("unresolved[%d] = %q, want %q", index, got[index].ID, id)
		}
	}
}

func TestListWorkItemsUnresolvedUsesStateIndexes(t *testing.T) {
	s := newTestStore(t)
	selectSQL := `EXPLAIN QUERY PLAN SELECT ` + workItemSummaryListColumns + workItemSummaryProgressColumns +
		` FROM work_items AS w` + workItemSummaryProgressJoin + ` WHERE `
	orderSQL := ` ORDER BY w.created_at ASC, w.id ASC`
	assertPlanUses(t, s.db, "idx_work_items_state_created",
		selectSQL+qualifiedUnresolvedWorkItemsPredicate("w.")+orderSQL)
	assertPlanUses(t, s.db, "idx_work_items_project_created",
		selectSQL+`w.project_id = ? AND `+qualifiedUnresolvedWorkItemsPredicate("w.")+orderSQL,
		"project-a")
	assertPlanUses(t, s.db, "idx_work_item_phases_item_started",
		selectSQL+`w.project_id = ? AND `+qualifiedUnresolvedWorkItemsPredicate("w.")+orderSQL,
		"project-a")
}

func TestListWorkItemSummariesIncludesPersistedPhaseProgress(t *testing.T) {
	s := newTestStore(t)
	item := testWorkItem("running", "project-a", "running", 1)
	item.Snapshot = json.RawMessage(`{"workflow":{"phases":[{"id":"plan"},{"id":"implement"},{"id":"verify"}]}}`)
	if err := s.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []WorkItemPhase{
		{ItemID: item.ID, PhaseID: "plan", Attempt: 1, Status: "completed", StartedAt: 1, EndedAt: 2},
		{ItemID: item.ID, PhaseID: "implement", Attempt: 1, Status: "running", StartedAt: 3},
	} {
		if err := s.CreateWorkItemPhase(phase); err != nil {
			t.Fatal(err)
		}
	}

	items, err := s.ListWorkItemSummaries(WorkItemListFilter{ProjectID: item.ProjectID})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("summaries = %+v, want one", items)
	}
	got := items[0]
	if got.CurrentPhaseID != "implement" || got.CurrentPhaseOrdinal != 2 || got.PhaseCount != 3 {
		t.Fatalf("summary progress = %+v, want implement 2/3", got)
	}
	if len(got.Snapshot) != 0 {
		t.Fatalf("summary loaded frozen snapshot: %s", got.Snapshot)
	}
}

// testCalledWorkItem builds a run created by a call phase: linked to its caller,
// carrying the caller's workspace, and never carrying a budget of its own.
func testCalledWorkItem(id, parentID, parentPhase string, attempt, depth int, createdAt int64) WorkItem {
	item := testWorkItem(id, "project-a", "running", createdAt)
	item.Budget = nil
	item.Source = "call"
	item.SourceRef = parentID + "/" + parentPhase
	item.ParentItemID = parentID
	item.ParentPhaseID = parentPhase
	item.ParentAttempt = attempt
	item.CallDepth = depth
	return item
}

func TestWorkItemCallLinkageRoundTripsAndListsChildren(t *testing.T) {
	s := newTestStore(t)
	rows := []WorkItem{
		testWorkItem("root", "project-a", "running", 10),
		testCalledWorkItem("child-a", "root", "audit", 1, 1, 20),
		testCalledWorkItem("child-b", "root", "audit", 2, 1, 30),
		testCalledWorkItem("child-c", "root", "review", 1, 1, 40),
		testCalledWorkItem("grandchild", "child-a", "audit", 1, 2, 50),
		testWorkItem("unrelated", "project-a", "running", 60),
	}
	for _, row := range rows {
		if err := s.CreateWorkItem(row); err != nil {
			t.Fatalf("create %s: %v", row.ID, err)
		}
	}

	child, err := s.GetWorkItem("child-a")
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentItemID != "root" || child.ParentPhaseID != "audit" ||
		child.ParentAttempt != 1 || child.CallDepth != 1 || child.Source != "call" {
		t.Fatalf("call linkage round trip = %+v", child)
	}
	if root, err := s.GetWorkItem("root"); err != nil {
		t.Fatal(err)
	} else if root.ParentItemID != "" || root.CallDepth != 0 {
		t.Fatalf("root carries linkage: %+v", root)
	}

	children, err := s.ListWorkItemChildren("root")
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 3 || children[0].ID != "child-a" || children[2].ID != "child-c" {
		t.Fatalf("children of root = %#v", children)
	}
	// One attempt's invocation, not the phase's: a rerun of a call phase creates
	// a new attempt with a fresh child, and the parent waits on that one alone.
	attemptChildren, err := s.ListWorkItemCallChildren("root", "audit", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(attemptChildren) != 1 || attemptChildren[0].ID != "child-b" {
		t.Fatalf("call children of root/audit/2 = %#v", attemptChildren)
	}
	if none, err := s.ListWorkItemCallChildren("root", "audit", 3); err != nil {
		t.Fatal(err)
	} else if len(none) != 0 {
		t.Fatalf("unstarted attempt has children: %#v", none)
	}
	if leaves, err := s.ListWorkItemChildren("unrelated"); err != nil {
		t.Fatal(err)
	} else if len(leaves) != 0 {
		t.Fatalf("childless item reported children: %#v", leaves)
	}

	// An empty parent id would otherwise select every root run at once.
	if _, err := s.ListWorkItemChildren(""); err == nil {
		t.Fatal("empty parent id must be refused")
	}
	if _, err := s.ListWorkItemCallChildren("root", "", 1); err == nil {
		t.Fatal("empty parent phase id must be refused")
	}

	summaries, err := s.ListWorkItemSummaries(WorkItemListFilter{ParentItemID: "root"})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 3 || summaries[0].ID != "child-a" || summaries[0].ParentPhaseID != "audit" {
		t.Fatalf("child summaries = %#v", summaries)
	}
	// A project listing still lists every run, callers and callees alike.
	all, err := s.ListWorkItemSummaries(WorkItemListFilter{ProjectID: "project-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(rows) {
		t.Fatalf("project listing = %d items, want %d", len(all), len(rows))
	}
}

func TestWorkItemRejectsPartialCallLinkage(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateWorkItem(testWorkItem("root", "project-a", "running", 10)); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		break_ func(*WorkItem)
	}{
		{"no phase", func(item *WorkItem) { item.ParentPhaseID = "" }},
		{"no attempt", func(item *WorkItem) { item.ParentAttempt = 0 }},
		{"no depth", func(item *WorkItem) { item.CallDepth = 0 }},
		{"depth without parent", func(item *WorkItem) {
			item.ParentItemID = ""
			item.ParentPhaseID = ""
			item.ParentAttempt = 0
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := testCalledWorkItem("child-"+tc.name, "root", "audit", 1, 1, 20)
			tc.break_(&item)
			if err := s.CreateWorkItem(item); err == nil {
				t.Fatal("partial call linkage was accepted")
			}
		})
	}
}
