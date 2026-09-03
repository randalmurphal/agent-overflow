package store

import (
	"reflect"
	"testing"
)

func TestSharedImportHistoryDeduplicatesWithoutAliasingLogicalThreads(t *testing.T) {
	s := newTestStore(t)
	for _, threadID := range []string{"shared-a", "shared-b"} {
		newImportTargetThread(t, s, threadID)
		if err := s.ApplyImportBatch(threadID, importBatchFixture(threadID)); err != nil {
			t.Fatalf("apply import to %s: %v", threadID, err)
		}
	}

	assertCount := func(name, query string, want int) {
		t.Helper()
		var got int
		if err := s.db.QueryRow(query).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		if got != want {
			t.Fatalf("%s count = %d, want %d", name, got, want)
		}
	}
	assertCount("shared chunks", `SELECT COUNT(*) FROM import_history_chunks`, 1)
	assertCount("shared items", `SELECT COUNT(*) FROM import_history_items`, 2)
	assertCount("shared payloads", `SELECT COUNT(*) FROM import_history_payloads`, 2)
	assertCount("thread mappings", `SELECT COUNT(*) FROM thread_import_chunks`, 2)
	assertCount("local items", `SELECT COUNT(*) FROM items`, 0)
	assertCount("local payloads", `SELECT COUNT(*) FROM payloads`, 0)

	a, err := s.ListItems("shared-a")
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	b, err := s.ListItems("shared-b")
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(a) != len(b) || len(a) != 2 {
		t.Fatalf("logical lengths A/B = %d/%d, want 2/2", len(a), len(b))
	}
	for i := range a {
		a[i].ThreadID = ""
		b[i].ThreadID = ""
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("shared branches render differently:\nA=%+v\nB=%+v", a, b)
	}

	if err := s.UpdatePayloadMeta("shared-a", "payload-out", `{"branch":"a"}`); err != nil {
		t.Fatalf("update A payload: %v", err)
	}
	aMeta, err := s.GetPayloadMeta("shared-a", "payload-out")
	if err != nil {
		t.Fatalf("read A payload: %v", err)
	}
	bMeta, err := s.GetPayloadMeta("shared-b", "payload-out")
	if err != nil {
		t.Fatalf("read B payload: %v", err)
	}
	if aMeta.Meta != `{"branch":"a"}` || bMeta.Meta != `{"exitCode":0}` {
		t.Fatalf("payload copy-on-write leaked: A=%s B=%s", aMeta.Meta, bMeta.Meta)
	}
	assertCount("A payload overlay", `SELECT COUNT(*) FROM payloads WHERE thread_id = 'shared-a'`, 1)

	changed := "changed only in A"
	if err := s.UpdateItemFields("shared-a", "item-tool", ItemPartialUpdate{Summary: &changed}); err != nil {
		t.Fatalf("update A item: %v", err)
	}
	aItem, found, err := s.GetThreadItem("shared-a", "item-tool")
	if err != nil || !found {
		t.Fatalf("read A item: %v", err)
	}
	bItem, found, err := s.GetThreadItem("shared-b", "item-tool")
	if err != nil || !found {
		t.Fatalf("read B item: %v", err)
	}
	if aItem.Summary != changed || bItem.Summary != "Bash" {
		t.Fatalf("item copy-on-write leaked: A=%q B=%q", aItem.Summary, bItem.Summary)
	}

	var revA, revB, epochA, epochB int64
	if err := s.db.QueryRow(
		`SELECT history_rev, history_epoch FROM threads WHERE id = 'shared-a'`,
	).Scan(&revA, &epochA); err != nil {
		t.Fatalf("read A stamps: %v", err)
	}
	if err := s.db.QueryRow(
		`SELECT history_rev, history_epoch FROM threads WHERE id = 'shared-b'`,
	).Scan(&revB, &epochB); err != nil {
		t.Fatalf("read B stamps: %v", err)
	}
	if revA != 4 || epochA != 0 || revB != 2 || epochB != 0 {
		t.Fatalf("copy-on-write stamps A=%d/%d B=%d/%d, want 4/0 2/0", revA, epochA, revB, epochB)
	}
}

func TestSharedImportHistoryStructuralCutMaterializesOnlyTargetThread(t *testing.T) {
	s := newTestStore(t)
	for _, threadID := range []string{"cut-a", "cut-b"} {
		newImportTargetThread(t, s, threadID)
		if err := s.ApplyImportBatch(threadID, importBatchFixture(threadID)); err != nil {
			t.Fatalf("apply import to %s: %v", threadID, err)
		}
	}

	deleted, stamp, err := s.DeleteConversationFromTurn("cut-a", 1)
	if err != nil {
		t.Fatalf("cut A: %v", err)
	}
	if deleted != 2 || stamp.Rev != 4 || stamp.Epoch != 2 {
		t.Fatalf("cut result deleted=%d stamp=%+v, want 2 and 4/2", deleted, stamp)
	}
	a, err := s.ListItems("cut-a")
	if err != nil {
		t.Fatalf("list cut A: %v", err)
	}
	b, err := s.ListItems("cut-b")
	if err != nil {
		t.Fatalf("list intact B: %v", err)
	}
	if len(a) != 0 || len(b) != 2 {
		t.Fatalf("post-cut logical lengths A/B = %d/%d, want 0/2", len(a), len(b))
	}
	var mappingsA, mappingsB, chunks int
	if err := s.db.QueryRow(
		`SELECT
		   (SELECT COUNT(*) FROM thread_import_chunks WHERE thread_id = 'cut-a'),
		   (SELECT COUNT(*) FROM thread_import_chunks WHERE thread_id = 'cut-b'),
		   (SELECT COUNT(*) FROM import_history_chunks)`,
	).Scan(&mappingsA, &mappingsB, &chunks); err != nil {
		t.Fatalf("read mapping state: %v", err)
	}
	if mappingsA != 0 || mappingsB != 1 || chunks != 1 {
		t.Fatalf("mapping state A/B/chunks = %d/%d/%d, want 0/1/1", mappingsA, mappingsB, chunks)
	}
}

func TestSharedImportHistoryPagingAndSubagentExpansionUseLogicalTimeline(t *testing.T) {
	s := newTestStore(t)
	newImportTargetThread(t, s, "shared-window")
	batch := importBatchFixture("shared-window")
	batch.Rows[1].Item.ParentID = "item-user"
	if err := s.ApplyImportBatch("shared-window", batch); err != nil {
		t.Fatalf("apply import: %v", err)
	}

	page, err := s.ListThreadSliceAround("shared-window", "", 200)
	if err != nil {
		t.Fatalf("list slice: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "item-user" {
		t.Fatalf("top-level page = %+v, want only item-user", page.Items)
	}
	descendants, err := s.ListSubagentDescendants("shared-window", "item-user")
	if err != nil {
		t.Fatalf("list descendants: %v", err)
	}
	if len(descendants) != 1 || descendants[0].ID != "item-tool" || descendants[0].PayloadKind != "command_output" {
		t.Fatalf("descendants = %+v", descendants)
	}
}

func TestSharedImportHistoryEditDiffReadsHonorPayloadOverlays(t *testing.T) {
	s := newTestStore(t)
	newImportTargetThread(t, s, "shared-edits")
	batch := importBatchFixture("shared-edits")
	batch.Rows[1].Payload.Kind = "tool_result"
	batch.Rows[1].Payload.Meta = `{"source":"import"}`
	batch.Rows[1].Payload.Data = []byte("--- a/file\n+++ b/file\n")
	if err := s.ApplyImportBatch("shared-edits", batch); err != nil {
		t.Fatalf("apply import: %v", err)
	}

	assertEdit := func(wantMeta, wantData string) {
		t.Helper()
		items, err := s.ListEditDiffItems("shared-edits")
		if err != nil {
			t.Fatalf("list edit diff items: %v", err)
		}
		if len(items) != 1 || items[0].ItemID != "item-tool" || items[0].PayloadMeta != wantMeta {
			t.Fatalf("edit items = %+v, want imported item with meta %s", items, wantMeta)
		}
		patches, err := s.ListTurnEditDiffPatches("shared-edits", 1)
		if err != nil {
			t.Fatalf("list turn edit patches: %v", err)
		}
		if len(patches) != 1 || patches[0].PayloadID != "payload-out" || string(patches[0].Data) != wantData {
			t.Fatalf("edit patches = %+v, want payload-out %q", patches, wantData)
		}
	}

	assertEdit(`{"source":"import"}`, "--- a/file\n+++ b/file\n")
	if err := s.ReplacePayloadData(
		"shared-edits",
		"payload-out",
		[]byte("--- a/new\n+++ b/new\n"),
		`{"source":"overlay"}`,
		importTurnComplete+1,
	); err != nil {
		t.Fatalf("replace imported payload through local overlay: %v", err)
	}
	assertEdit(`{"source":"overlay"}`, "--- a/new\n+++ b/new\n")
}

func TestSharedImportHistoryActionablePlanProbeUsesImportedPayload(t *testing.T) {
	s := newTestStore(t)
	newImportTargetThread(t, s, "shared-plan")
	batch := importBatchFixture("shared-plan")
	batch.Rows = batch.Rows[1:]
	batch.Rows[0].Item.ID = "plan-1"
	batch.Rows[0].Item.PayloadID = "plan-payload"
	batch.Rows[0].Payload.ID = "plan-payload"
	batch.Rows[0].Payload.Kind = "proposed_plan"
	if err := s.ApplyImportBatch("shared-plan", batch); err != nil {
		t.Fatalf("apply import: %v", err)
	}
	if _, err := s.EnsureProposedPlanState("shared-plan", "plan-1", importTurnComplete+1); err != nil {
		t.Fatalf("ensure proposed plan state: %v", err)
	}

	threads, err := s.ListThreadsWithItems()
	if err != nil {
		t.Fatalf("list threads with items: %v", err)
	}
	if len(threads) != 1 || !threads[0].HasActionableProposedPlan {
		t.Fatalf("threads = %+v, want imported actionable plan", threads)
	}
}
