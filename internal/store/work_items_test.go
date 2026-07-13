package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
)

func testWorkItem(id, projectID, state string, sortPosition int, createdAt int64) WorkItem {
	return WorkItem{
		ID: id, ProjectID: projectID, Goal: "ship it", WorkflowID: "build",
		WorkflowScope: "project", State: state, SortPosition: sortPosition,
		Seeds: json.RawMessage(`{"ticket":"AO-1"}`), StepMode: true,
		Budget: json.RawMessage(`{"kind":"tokens","limit":1000}`),
		Source: "manual", CreatedAt: createdAt,
	}
}

func TestWorkItemCRUDListAndTransitions(t *testing.T) {
	s := newTestStore(t)
	items := []WorkItem{
		testWorkItem("queued-later", "project-a", "queued", 2, 20),
		testWorkItem("running", "project-a", "running", 0, 10),
		testWorkItem("queued-first", "project-a", "queued", 1, 30),
		testWorkItem("other-project", "project-b", "queued", 0, 1),
	}
	for _, item := range items {
		if err := s.CreateWorkItem(item); err != nil {
			t.Fatalf("create %s: %v", item.ID, err)
		}
	}

	queued, err := s.ListWorkItems(WorkItemListFilter{ProjectID: "project-a", States: []string{"queued"}})
	if err != nil {
		t.Fatalf("list queued: %v", err)
	}
	if len(queued) != 2 || queued[0].ID != "queued-first" || queued[1].ID != "queued-later" {
		t.Fatalf("queued order = %#v", queued)
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
		everyProject[0].ID != "other-project" || everyProject[1].ID != "running" ||
		everyProject[2].ID != "queued-first" || everyProject[3].ID != "queued-later" {
		t.Fatalf("every-project order = %#v", everyProject)
	}
	crossProjectQueued, err := s.ListWorkItems(WorkItemListFilter{States: []string{"queued"}})
	if err != nil {
		t.Fatalf("list cross-project queued: %v", err)
	}
	if len(crossProjectQueued) != 3 {
		t.Fatalf("cross-project queued = %d, want 3", len(crossProjectQueued))
	}

	snapshot := json.RawMessage(`{"id":"build","version":1}`)
	if err := s.UpdateWorkItemRunStart("queued-first", snapshot, "/tmp/wt", "ao/item", "main", 40); err != nil {
		t.Fatalf("run start: %v", err)
	}
	if err := s.UpdateWorkItemState("queued-first", "running", "", 0); err != nil {
		t.Fatalf("set running: %v", err)
	}
	if err := s.UpdateWorkItemState("queued-first", "needs-human", "gate", 50); err != nil {
		t.Fatalf("set needs-human: %v", err)
	}
	disposition := json.RawMessage(`{"action":"merged","policy":"manual","at":50}`)
	digest := json.RawMessage(`{"whatHappened":"Paused.","whatItNeeds":"Review."}`)
	if err := s.UpdateWorkItemDisposition("queued-first", disposition); err != nil {
		t.Fatalf("set disposition: %v", err)
	}
	if err := s.UpdateWorkItemDigest("queued-first", digest); err != nil {
		t.Fatalf("set digest: %v", err)
	}
	got, err := s.GetWorkItem("queued-first")
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
	if count, err := s.CountWorkItemsInStates("queued", "running"); err != nil || count != 3 {
		t.Fatalf("queued/running count = %d err=%v, want 3", count, err)
	}
	if count, err := s.CountProjectWorkItemsInStates("project-a", "queued", "running"); err != nil || count != 2 {
		t.Fatalf("project-a queued/running count = %d err=%v, want 2", count, err)
	}
}

func TestWorkItemTriageAssociationAndPhaseThreadLookup(t *testing.T) {
	s := newTestStore(t)
	item := testWorkItem("triage-link", defaultTestProjectID, "needs-human", 0, 1)
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
	item := testWorkItem("attention", "project-a", "needs-human", 0, 1)
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
	item := testWorkItem("workspace", "project-a", "running", 0, 1)
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

func TestWorkItemSummaryAndNextSortPosition(t *testing.T) {
	s := newTestStore(t)
	first := testWorkItem("first", "project-a", "queued", 4, 1)
	first.Snapshot = json.RawMessage(`{"workflow":{"prompt":"large"}}`)
	first.Digest = json.RawMessage(`{"whatHappened":"heavy detail"}`)
	first.Disposition = json.RawMessage(`{"action":"merged","policy":"manual","at":2}`)
	if err := s.CreateWorkItem(first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	second := testWorkItem("second", "project-a", "done", 9, 2)
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
	position, err := s.NextWorkItemSortPosition("project-a")
	if err != nil {
		t.Fatalf("next sort position: %v", err)
	}
	if position != 10 {
		t.Fatalf("next sort position = %d, want 10", position)
	}
	emptyPosition, err := s.NextWorkItemSortPosition("project-empty")
	if err != nil {
		t.Fatalf("empty next sort position: %v", err)
	}
	if emptyPosition != 0 {
		t.Fatalf("empty next sort position = %d, want 0", emptyPosition)
	}
}

func TestListWorkItemsUnresolvedExcludesDisposedRegardlessOfState(t *testing.T) {
	s := newTestStore(t)
	receipt := json.RawMessage(`{"action":"discarded","policy":"manual","at":10}`)
	items := []WorkItem{
		testWorkItem("queued", "project-a", "queued", 0, 1),
		testWorkItem("running", "project-a", "running", 1, 2),
		testWorkItem("needs-human", "project-a", "needs-human", 2, 3),
		testWorkItem("done", "project-a", "done", 3, 4),
		testWorkItem("failed", "project-a", "failed", 4, 5),
		testWorkItem("cancelled", "project-a", "cancelled", 5, 6),
		testWorkItem("other-project", "project-b", "failed", 6, 7),
	}
	items[4].Disposition = receipt
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
	want := []string{"queued", "running", "needs-human", "done"}
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
	orderSQL := ` ORDER BY w.sort_position ASC, w.created_at ASC`
	assertPlanUses(t, s.db, "idx_work_items_state_sort",
		selectSQL+qualifiedUnresolvedWorkItemsPredicate("w.")+orderSQL)
	assertPlanUses(t, s.db, "idx_work_items_project_sort",
		selectSQL+`w.project_id = ? AND `+qualifiedUnresolvedWorkItemsPredicate("w.")+orderSQL,
		"project-a")
	assertPlanUses(t, s.db, "idx_work_item_phases_item_started",
		selectSQL+`w.project_id = ? AND `+qualifiedUnresolvedWorkItemsPredicate("w.")+orderSQL,
		"project-a")
}

func TestListWorkItemSummariesIncludesPersistedPhaseProgress(t *testing.T) {
	s := newTestStore(t)
	item := testWorkItem("running", "project-a", "running", 0, 1)
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

func TestReorderQueuedWorkItemsIsAtomic(t *testing.T) {
	s := newTestStore(t)
	for _, item := range []WorkItem{
		testWorkItem("one", "project-a", "queued", 5, 1),
		testWorkItem("two", "project-a", "queued", 6, 2),
		testWorkItem("running", "project-a", "running", 7, 3),
	} {
		if err := s.CreateWorkItem(item); err != nil {
			t.Fatalf("create %s: %v", item.ID, err)
		}
	}
	if err := s.ReorderQueuedWorkItems("project-a", []string{"two", "one"}); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	got, err := s.ListWorkItems(WorkItemListFilter{ProjectID: "project-a", States: []string{"queued"}})
	if err != nil {
		t.Fatalf("list reordered: %v", err)
	}
	if got[0].ID != "two" || got[0].SortPosition != 0 || got[1].ID != "one" || got[1].SortPosition != 1 {
		t.Fatalf("reordered items = %#v", got)
	}

	if err := s.ReorderQueuedWorkItems("project-a", []string{"one", "running"}); err == nil {
		t.Fatal("reorder including running item succeeded")
	}
	one, err := s.GetWorkItem("one")
	if err != nil {
		t.Fatalf("get one after rollback: %v", err)
	}
	if one.SortPosition != 1 {
		t.Fatalf("failed reorder partially committed position %d, want 1", one.SortPosition)
	}
	if err := s.ReorderQueuedWorkItems("project-a", []string{"one", "one"}); err == nil {
		t.Fatal("reorder with duplicate ids succeeded")
	}
	one, err = s.GetWorkItem("one")
	if err != nil {
		t.Fatalf("get one after duplicate rejection: %v", err)
	}
	if one.SortPosition != 1 {
		t.Fatalf("duplicate reorder mutated position %d, want 1", one.SortPosition)
	}
}
