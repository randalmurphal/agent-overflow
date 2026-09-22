package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestScopedTimelineTraversesAllHistoryAndRunMembers(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("scope", "claude")); err != nil {
		t.Fatal(err)
	}
	seedAnchorItem(t, s, "scope", "agent", 0, 0)
	seedAnchorItem(t, s, "scope", "other", 0, 1)
	for i := 0; i < 2500; i++ {
		item := Item{ID: fmt.Sprintf("child-%04d", i), ThreadID: "scope", TurnIndex: 0, ItemIndex: i + 2, ParentID: "agent", Kind: "tool_call", ToolName: "Bash", Role: "assistant", Status: "completed", Summary: strings.Repeat("x", 600), CreatedAt: 1}
		if i%100 == 0 {
			item.Kind = "assistant_text"
			item.ToolName = ""
		}
		if err := s.InsertItem(item); err != nil {
			t.Fatal(err)
		}
	}
	seedChildItem(t, s, "scope", "foreign-child", 0, 2502, "other", "wrong scope", "completed")
	selection := TimelineSelection{ScopeRootID: "agent"}
	page, err := s.ListThreadSliceAround(context.Background(), "scope", "", 40, 10, selection)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for pages := 0; ; pages++ {
		if pages > 100 {
			t.Fatal("paging did not advance")
		}
		if page.Scope == nil || page.Scope.Root.ID != "agent" {
			t.Fatal("missing root context")
		}
		for _, item := range page.Items {
			if item.ParentID != "agent" {
				t.Fatalf("foreign row: %s", item.ID)
			}
			seen[item.ID] = true
		}
		for _, run := range page.Runs {
			for run.UnshippedBefore > 0 {
				members, err := s.ListActivityRunMembers(context.Background(), "scope", ActivityRunMembersRequest{Selection: selection, RunFirstItemID: run.FirstItemID, LoadedFirstItemID: run.LoadedFirstItemID, LoadedLastItemID: run.LoadedLastItemID, Direction: ActivityRunMembersBefore, Limit: 20})
				if err != nil {
					t.Fatal(err)
				}
				if len(members.Items) == 0 || members.Stub.UnshippedBefore >= run.UnshippedBefore {
					t.Fatal("members did not advance")
				}
				for _, item := range members.Items {
					seen[item.ID] = true
				}
				run = members.Stub
			}
		}
		if !page.HasMoreOlder {
			break
		}
		previous := page.OldestCursor
		page, err = s.ListItemsBeforeCursor(context.Background(), "scope", previous, 40, 10, selection)
		if err != nil {
			t.Fatal(err)
		}
		if !cursorBefore(page.OldestCursor, previous) {
			t.Fatal("page did not advance")
		}
	}
	if len(seen) != 2500 {
		t.Fatalf("reached %d rows, want 2500", len(seen))
	}
	if _, err := s.ListThreadSliceAround(context.Background(), "scope", "foreign-child", 40, 10, selection); err == nil {
		t.Fatal("a foreign scope anchor was accepted")
	}
	_, err = s.ListActivityRunMembers(context.Background(), "scope", ActivityRunMembersRequest{Selection: TimelineSelection{ScopeRootID: "other"}, RunFirstItemID: "child-0001", Direction: ActivityRunMembersBefore, Limit: 10})
	if !errors.Is(err, ErrActivityRunStale) {
		t.Fatalf("cross-scope run: %v", err)
	}
}

func TestScopedTimelineEmptyToolsAndContextOnlyChange(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("scope", "claude")); err != nil {
		t.Fatal(err)
	}
	seedAnchorItem(t, s, "scope", "agent", 0, 0)
	selection := TimelineSelection{ScopeRootID: "agent"}
	page, err := s.ListThreadSliceAround(context.Background(), "scope", "", 40, 10, selection)
	if err != nil || len(page.Items) != 0 || page.Scope == nil {
		t.Fatalf("empty scope: %+v %v", page, err)
	}
	seedChildItem(t, s, "scope", "prose", 0, 1, "agent", "prose", "completed")
	seedToolChildItem(t, s, "scope", "tool", 0, 2, "agent", "tool", "completed")
	seedChildItem(t, s, "scope", "answer", 0, 3, "agent", "answer", "completed")
	tools := TimelineSelection{ScopeRootID: "agent", Tools: true}
	page, err = s.ListThreadSliceAround(context.Background(), "scope", "", 40, 10, tools)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "tool" {
		t.Fatalf("tool selection: %+v %v", page, err)
	}
	synced, err := s.SyncThreadWindow(context.Background(), "scope", "", 40, 10, UnknownHistoryStamp(), nil, selection)
	if err != nil {
		t.Fatal(err)
	}
	rows := []WindowDigestRow{}
	for _, row := range synced.Page.Items {
		rows = append(rows, WindowDigestRow{ID: row.ID, Rev: row.Rev})
	}
	held := &HeldWindow{OldestItemID: "prose", NewestItemID: "answer", Count: 3, Digest: WindowDigest(rows)}
	if err := s.InsertItem(Item{ID: "complete:agent:turn:second", ThreadID: "scope", TurnIndex: 0, ItemIndex: 4, Kind: "tool_completion", ToolName: "Task", Role: "assistant", Status: "completed", CompletionOf: "agent", CreatedAt: 10}); err != nil {
		t.Fatal(err)
	}
	synced, err = s.SyncThreadWindow(context.Background(), "scope", "", 40, 10, synced.Stamp, held, selection)
	if err != nil || synced.Status != SyncFresh || synced.Scope == nil || synced.Scope.Completion == nil || synced.Scope.Completion.ID != "complete:agent:turn:second" {
		t.Fatalf("context-only update: %+v %v", synced, err)
	}
	if _, err := s.ListThreadSliceAround(context.Background(), "scope", "", 40, 10, TimelineSelection{ScopeRootID: "missing"}); !errors.Is(err, ErrTimelineScopeGone) {
		t.Fatalf("missing root: %v", err)
	}
}

func TestMigrationV111ScopedTimelineIndexes(t *testing.T) {
	db := migrateThrough(t, 110)
	migrateFrom(t, db, 110)
	assertPlanUses(t, db, "idx_items_parent", `EXPLAIN QUERY PLAN SELECT id FROM items WHERE thread_id=? AND parent_id<>'' AND parent_id=? AND (turn_index,item_index)<(?,?) ORDER BY turn_index DESC,item_index DESC LIMIT 20`, "t", "root", 9, 9)
	assertPlanUses(t, db, "idx_import_history_items_parent", `EXPLAIN QUERY PLAN SELECT id FROM import_history_items WHERE chunk_id=? AND parent_id<>'' AND parent_id=? AND (turn_index,item_index)<(?,?) ORDER BY turn_index DESC,item_index DESC LIMIT 20`, "c", "root", 9, 9)
	for _, arm := range []struct{ table, owner, index string }{
		{"items", "thread_id", "idx_items_scope_lifecycle"},
		{"import_history_items", "chunk_id", "idx_import_history_items_scope_lifecycle"},
	} {
		assertPlanUses(t, db, arm.index, `EXPLAIN QUERY PLAN SELECT id FROM `+arm.table+` WHERE `+arm.owner+`=? AND kind='tool_call' AND `+jsonFieldExpr("meta", "$.transcript_root_id")+`=? ORDER BY turn_index DESC,item_index DESC LIMIT 1`, "t", "root")
	}

}

func TestScopedTimelineImportedAndLocalHistoryAgree(t *testing.T) {
	s := newTestStore(t)
	rows := []Item{
		{ID: "root", TurnIndex: 1, ItemIndex: 0, Kind: "tool_call", ToolName: "Agent", Status: "running", Meta: `{}`},
		{ID: "prompt", TurnIndex: 1, ItemIndex: 1, Kind: "user_text", ParentID: "root", Summary: "first task"},
		{ID: "tool", TurnIndex: 1, ItemIndex: 2, Kind: "tool_call", ToolName: "Bash", ParentID: "root"},
		{ID: "first-done", TurnIndex: 1, ItemIndex: 3, Kind: "tool_completion", CompletionOf: "root", Status: "completed"},
		{ID: "resume", TurnIndex: 2, ItemIndex: 0, Kind: "tool_call", ToolName: "SendMessage", Meta: `{"transcript_root_id":"root"}`, Status: "running"},
		{ID: "next-prompt", TurnIndex: 2, ItemIndex: 1, Kind: "user_text", ParentID: "root", Summary: "next task"},
		{ID: "answer", TurnIndex: 2, ItemIndex: 2, Kind: "assistant_text", ParentID: "root", Summary: "next answer"},
		{ID: "second-done", TurnIndex: 2, ItemIndex: 3, Kind: "tool_completion", CompletionOf: "resume", Status: "completed"},
	}
	for _, imported := range []bool{false, true} {
		threadID := fmt.Sprintf("scope-import-%t", imported)
		newImportTargetThread(t, s, threadID)
		batch := ImportBatch{}
		for _, row := range rows {
			row.ThreadID, row.Role, row.CreatedAt = threadID, "assistant", 1
			if row.Status == "" {
				row.Status = "completed"
			}
			if imported {
				batch.Rows = append(batch.Rows, ImportRow{Item: row})
			} else if err := s.InsertItem(row); err != nil {
				t.Fatal(err)
			}
		}
		if imported {
			if err := s.ApplyImportBatch(threadID, batch); err != nil {
				t.Fatal(err)
			}
		}
		selection := TimelineSelection{ScopeRootID: "root"}
		page, err := s.ListThreadSliceAround(context.Background(), threadID, "", 40, 10, selection)
		if err != nil {
			t.Fatal(err)
		}
		if got := itemIDs(page.Items); strings.Join(got, ",") != "prompt,tool,next-prompt,answer" {
			t.Fatalf("imported=%t rows=%v", imported, got)
		}
		if page.Scope.Lifecycle.ID != "resume" || page.Scope.Completion.ID != "second-done" {
			t.Fatalf("latest lifecycle: %+v", page.Scope)
		}
		canonical, err := s.ListThreadSliceAround(context.Background(), threadID, "", 40, 10, TimelineSelection{ScopeRootID: "resume"})
		if err != nil || canonical.Scope.Root.ID != "root" {
			t.Fatalf("canonical scope: %+v %v", canonical, err)
		}
		ticks, err := s.ListThreadUserMessageTicks(threadID, selection)
		if err != nil || len(ticks) != 2 || ticks[0].ID != "prompt" || ticks[1].ID != "next-prompt" {
			t.Fatalf("scoped nav: %+v %v", ticks, err)
		}
	}
}
