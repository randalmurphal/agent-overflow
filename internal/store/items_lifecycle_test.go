package store

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"database/sql"
)

// Migration v14 adds the tool-call lifecycle columns to items:
// status (TEXT CHECK IN …), is_background (INTEGER 0|1), and
// completion_of_item_id (TEXT). These tests pin:
//   - the three columns exist after migration
//   - defaults backfill existing rows safely ("completed", 0, "")
//   - CHECK constraints reject bogus values
//   - the completion-of partial index is created

func TestMigrationV14AddsLifecycleColumns(t *testing.T) {
	s := newTestStore(t)

	cols, err := tableColumns(s.db, "items")
	if err != nil {
		t.Fatalf("tableColumns(items): %v", err)
	}
	for _, want := range []string{"status", "is_background", "completion_of_item_id"} {
		if !cols[want] {
			t.Errorf("items.%s column missing (columns=%v)", want, cols)
		}
	}

	rows, err := s.db.Query("SELECT name FROM sqlite_master WHERE type='index' AND name = 'idx_items_completion_of'")
	if err != nil {
		t.Fatalf("query index: %v", err)
	}
	defer rows.Close()
	var found bool
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == "idx_items_completion_of" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	if !found {
		t.Error("idx_items_completion_of index missing after v14")
	}
}

// TestMigrationV14BackfillsExistingRows simulates an upgrade from a pre-v14
// database: pre-populate an item under the pre-v14 schema, then apply v14
// and confirm the row comes out with the defaults ("completed", 0, "").
func TestMigrationV14BackfillsExistingRows(t *testing.T) {
	db := openSQLiteDB(t)

	if err := configureDatabase(db); err != nil {
		t.Fatalf("configureDatabase: %v", err)
	}
	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("ensureMigrationTable: %v", err)
	}
	// Apply every migration except v14 so we're on the pre-v14 schema.
	for _, m := range migrations {
		if m.Version == 14 {
			continue
		}
		if err := applyMigration(db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.Version, err)
		}
	}

	if _, err := db.Exec(`INSERT INTO threads
		(id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES ('t-pre', 'Pre-v14', 'claude', '/tmp', '', 1000, 1000)`); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, summary, parent_tool_use_id, created_at)
		VALUES ('i-pre', 't-pre', 0, 0, 'text', 'assistant', 'body', '', 1000)`); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	// Find v14 in the migration list and apply it.
	var v14 *Migration
	for i := range migrations {
		if migrations[i].Version == 14 {
			v14 = &migrations[i]
			break
		}
	}
	if v14 == nil {
		t.Fatal("v14 migration missing from list")
	}
	if err := applyMigration(db, *v14); err != nil {
		t.Fatalf("apply v14: %v", err)
	}

	var status, completionOf string
	var isBackground int
	if err := db.QueryRow(`SELECT status, is_background, completion_of_item_id FROM items WHERE id = 'i-pre'`).
		Scan(&status, &isBackground, &completionOf); err != nil {
		t.Fatalf("read backfilled row: %v", err)
	}
	if status != "completed" {
		t.Errorf("status = %q, want %q", status, "completed")
	}
	if isBackground != 0 {
		t.Errorf("is_background = %d, want 0", isBackground)
	}
	if completionOf != "" {
		t.Errorf("completion_of_item_id = %q, want empty", completionOf)
	}
}

func TestMigrationV14StatusCheckRejectsBogusValue(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.db.Exec(`INSERT INTO threads (id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES ('t', 'T', 'claude', '/tmp', '', 1, 1)`); err != nil {
		t.Fatalf("seed thread: %v", err)
	}

	// Raw INSERT with a status value outside the allowed enum must fail.
	_, err := s.db.Exec(`INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, summary, parent_tool_use_id, status, is_background, completion_of_item_id, created_at)
		VALUES ('i', 't', 0, 0, 'text', 'assistant', '', '', 'weird', 0, '', 1)`)
	if err == nil {
		t.Fatal("INSERT with bogus status must violate CHECK constraint")
	}
	if !strings.Contains(err.Error(), "CHECK") && !strings.Contains(err.Error(), "constraint") {
		t.Errorf("error = %v, want CHECK constraint violation", err)
	}

	// Sanity: every allowed value succeeds.
	for i, status := range []string{"running", "completed", "errored", "declined"} {
		id := fmt.Sprintf("ok-%d", i)
		if _, err := s.db.Exec(`INSERT INTO items
			(id, thread_id, turn_index, item_index, kind, role, summary, parent_tool_use_id, status, is_background, completion_of_item_id, created_at)
			VALUES (?, 't', 0, ?, 'text', 'assistant', '', '', ?, 0, '', 1)`,
			id, i, status); err != nil {
			t.Errorf("INSERT with status=%q: %v", status, err)
		}
	}

	// UPDATE to a bogus value must also fail.
	if _, err := s.db.Exec(`UPDATE items SET status = 'nope' WHERE id = 'ok-0'`); err == nil {
		t.Error("UPDATE to bogus status must fail")
	}
}

func TestMigrationV14IsBackgroundCheckRejectsBogusValue(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.db.Exec(`INSERT INTO threads (id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES ('t', 'T', 'claude', '/tmp', '', 1, 1)`); err != nil {
		t.Fatalf("seed thread: %v", err)
	}

	// Only 0 and 1 are allowed; 2 must fail.
	if _, err := s.db.Exec(`INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, summary, parent_tool_use_id, status, is_background, completion_of_item_id, created_at)
		VALUES ('i', 't', 0, 0, 'text', 'assistant', '', '', 'completed', 2, '', 1)`); err == nil {
		t.Fatal("INSERT with is_background=2 must violate CHECK constraint")
	}
}

// --- Round-trip through store methods ---

func TestInsertItemRoundTripsLifecycleFields(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	if err := s.CreateThread(Thread{
		ID: "t", Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	in := Item{
		ID: "i-running", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", Summary: "ls",
		Status: "running", IsBackground: true,
		CreatedAt: now,
	}
	if err := s.InsertItem(in); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, ok, err := s.GetItem("i-running")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ok {
		t.Fatal("item missing")
	}
	if got.Status != "running" {
		t.Errorf("Status = %q, want %q", got.Status, "running")
	}
	if !got.IsBackground {
		t.Error("IsBackground: got false, want true")
	}
	if got.CompletionOfItemID != "" {
		t.Errorf("CompletionOfItemID: got %q, want empty", got.CompletionOfItemID)
	}
}

// TestInsertItemDefaultsStatusToCompleted pins the defaultStatus() coercion:
// existing callers that leave Status empty get "completed" so they don't
// trip the CHECK constraint.
func TestInsertItemDefaultsStatusToCompleted(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	if err := s.CreateThread(Thread{
		ID: "t", Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "i", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
		Kind: "text", Role: "assistant", Summary: "hi", CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, _, err := s.GetItem("i")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != "completed" {
		t.Errorf("Status = %q, want %q (default)", got.Status, "completed")
	}
}

// --- UpdateItemStatus ---

func TestUpdateItemStatusTransitionsRunningToCompleted(t *testing.T) {
	s := newTestStore(t)
	now := int64(1000)
	if err := s.CreateThread(Thread{
		ID: "t", Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "i", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", Summary: "ls",
		Status: "running", CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := s.UpdateItemStatus("i", "completed", "ls output", "", 2000); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, _, err := s.GetItem("i")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != "completed" {
		t.Errorf("Status = %q, want %q", got.Status, "completed")
	}
	if got.Summary != "ls output" {
		t.Errorf("Summary = %q, want %q", got.Summary, "ls output")
	}
	if got.CreatedAt != 2000 {
		t.Errorf("CreatedAt = %d, want 2000", got.CreatedAt)
	}

	// Thread's updated_at must move in the same transaction.
	thr, err := s.GetThread("t")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if thr.UpdatedAt != 2000 {
		t.Errorf("thread UpdatedAt = %d, want 2000", thr.UpdatedAt)
	}
}

func TestUpdateItemStatusLinksPayload(t *testing.T) {
	s := newTestStore(t)
	now := int64(1000)
	if err := s.CreateThread(Thread{
		ID: "t", Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "i", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", Summary: "ls",
		Status: "running", CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.InsertPayload(Payload{
		ID: "p", Kind: "tool_result", Meta: "{}", Data: []byte("output"), CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert payload: %v", err)
	}

	if err := s.UpdateItemStatus("i", "completed", "ls output", "p", 2000); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _, err := s.GetItem("i")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PayloadID != "p" {
		t.Errorf("PayloadID = %q, want p", got.PayloadID)
	}
}

// TestUpdateItemStatusErrorsOnMissingItem proves the RowsAffected guard
// fires; a silent no-op would mask a stale id in a caller's hand.
func TestUpdateItemStatusErrorsOnMissingItem(t *testing.T) {
	s := newTestStore(t)
	err := s.UpdateItemStatus("no-such-item", "completed", "x", "", 1)
	if err == nil {
		t.Fatal("expected error for missing item")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expected sql.ErrNoRows (wrapped), got %v", err)
	}
}

func TestUpdateItemStatusRejectsInvalidStatus(t *testing.T) {
	s := newTestStore(t)
	now := int64(1000)
	if err := s.CreateThread(Thread{
		ID: "t", Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "i", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", Status: "running", CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// A value outside the CHECK constraint must surface as an error.
	err := s.UpdateItemStatus("i", "halfway", "x", "", 2)
	if err == nil {
		t.Fatal("expected CHECK violation, got nil")
	}
}

// --- AppendCompletionItem ---

func TestAppendCompletionItemPairsLaunchAndCompletion(t *testing.T) {
	s := newTestStore(t)
	now := int64(1000)
	if err := s.CreateThread(Thread{
		ID: "t", Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	launch := Item{
		ID: "launch", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", Summary: "npm build",
		Status: "completed", IsBackground: true, CreatedAt: now,
	}
	if err := s.InsertItem(launch); err != nil {
		t.Fatalf("insert launch: %v", err)
	}

	// A sibling text item lands in between so item_index assignment has
	// something to bump past.
	if _, err := s.AppendItem(Item{
		ID: "text", ThreadID: "t", TurnIndex: 2, Kind: "text",
		Role: "assistant", Summary: "notes", CreatedAt: 1500,
	}); err != nil {
		t.Fatalf("append text: %v", err)
	}

	completion := Item{
		ID: "completion", ThreadID: "t", TurnIndex: 2,
		Kind: "background_done", Role: "assistant", Summary: "build ok",
		CreatedAt: 2000,
	}
	idx, err := s.AppendCompletionItem(launch, completion, nil)
	if err != nil {
		t.Fatalf("append completion: %v", err)
	}
	if idx != 1 {
		t.Errorf("completion item_index = %d, want 1 (after text at 0)", idx)
	}

	got, ok, err := s.GetItem("completion")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ok {
		t.Fatal("completion missing")
	}
	if !got.IsBackground {
		t.Error("completion IsBackground: got false, want true")
	}
	if got.CompletionOfItemID != "launch" {
		t.Errorf("CompletionOfItemID = %q, want %q", got.CompletionOfItemID, "launch")
	}
	if got.Kind != "background_done" {
		t.Errorf("Kind = %q, want background_done", got.Kind)
	}

	// Thread updated_at should match the completion's timestamp.
	thr, err := s.GetThread("t")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if thr.UpdatedAt != 2000 {
		t.Errorf("thread updated_at = %d, want 2000", thr.UpdatedAt)
	}
}

// TestAppendCompletionItemForcesInvariants proves callers can't
// accidentally override IsBackground/CompletionOfItemID by pre-setting
// them on the passed-in completion struct.
func TestAppendCompletionItemForcesInvariants(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	if err := s.CreateThread(Thread{
		ID: "t", Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	launch := Item{
		ID: "launch", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", IsBackground: true, CreatedAt: now,
	}
	if err := s.InsertItem(launch); err != nil {
		t.Fatalf("insert launch: %v", err)
	}

	completion := Item{
		ID: "completion", ThreadID: "t", TurnIndex: 0,
		Kind: "background_done", Role: "assistant",
		// Caller tries to pre-stamp lies:
		IsBackground:       false,
		CompletionOfItemID: "some-other-item",
		CreatedAt:          2,
	}
	if _, err := s.AppendCompletionItem(launch, completion, nil); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, _, err := s.GetItem("completion")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.IsBackground {
		t.Error("AppendCompletionItem must force IsBackground=true")
	}
	if got.CompletionOfItemID != "launch" {
		t.Errorf("AppendCompletionItem must force CompletionOfItemID=launch.ID, got %q", got.CompletionOfItemID)
	}
}

func TestAppendCompletionItemWithPayloadPersistsAtomically(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	if err := s.CreateThread(Thread{
		ID: "t", Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	launch := Item{
		ID: "launch", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", IsBackground: true, CreatedAt: now,
	}
	if err := s.InsertItem(launch); err != nil {
		t.Fatalf("insert launch: %v", err)
	}

	payload := &Payload{
		ID: "p", Kind: "command_output", Meta: `{"lineCount":12}`,
		Data: []byte("stdout..."), CreatedAt: 2,
	}
	completion := Item{
		ID: "completion", ThreadID: "t", TurnIndex: 0,
		Kind: "background_done", Role: "assistant", Summary: "done",
		CreatedAt: 2,
	}
	idx, err := s.AppendCompletionItem(launch, completion, payload)
	if err != nil {
		t.Fatalf("append with payload: %v", err)
	}
	if idx != 1 {
		t.Errorf("completion item_index = %d, want 1", idx)
	}

	got, _, err := s.GetItem("completion")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PayloadID != "p" {
		t.Errorf("completion PayloadID = %q, want p", got.PayloadID)
	}
	meta, err := s.GetPayloadMeta("p")
	if err != nil {
		t.Fatalf("get payload meta: %v", err)
	}
	if meta.Kind != "command_output" {
		t.Errorf("payload Kind = %q, want command_output", meta.Kind)
	}
}

// TestConcurrentAppendCompletionItemAssignsUniqueIndex ensures the
// MAX(item_index)+1 computation is race-safe even under contention, matching
// the guarantee AppendItem already offers.
func TestConcurrentAppendCompletionItemAssignsUniqueIndex(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	if err := s.CreateThread(Thread{
		ID: "t", Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	launch := Item{
		ID: "launch", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", IsBackground: true, CreatedAt: now,
	}
	if err := s.InsertItem(launch); err != nil {
		t.Fatalf("insert launch: %v", err)
	}

	const writers = 25
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, err := s.AppendCompletionItem(launch, Item{
				ID: fmt.Sprintf("done-%d", n), ThreadID: "t", TurnIndex: 0,
				Kind: "background_done", Role: "assistant", CreatedAt: int64(10 + n),
			}, nil)
			if err != nil {
				errs <- fmt.Errorf("completion %d: %w", n, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("goroutine error: %v", err)
	}

	items, err := s.ListTurnItems("t", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// launch + writers completion rows.
	if len(items) != 1+writers {
		t.Fatalf("expected %d items, got %d", 1+writers, len(items))
	}
	seen := make(map[int]bool, len(items))
	for _, it := range items {
		if seen[it.ItemIndex] {
			t.Errorf("duplicate item_index %d", it.ItemIndex)
		}
		seen[it.ItemIndex] = true
	}
	// Every completion must have the correct pointer back to the launch.
	for _, it := range items {
		if it.ID == "launch" {
			continue
		}
		if it.CompletionOfItemID != "launch" {
			t.Errorf("completion %s CompletionOfItemID = %q, want launch", it.ID, it.CompletionOfItemID)
		}
		if !it.IsBackground {
			t.Errorf("completion %s IsBackground = false, want true", it.ID)
		}
	}
}

// TestListItemsIncludesLifecycleFields proves the ListItems projection
// surfaces the new columns so thread hydration on the frontend sees them.
func TestListItemsIncludesLifecycleFields(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	if err := s.CreateThread(Thread{
		ID: "t", Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	launch := Item{
		ID: "launch", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", Summary: "npm build",
		Status: "completed", IsBackground: true, CreatedAt: now,
	}
	if err := s.InsertItem(launch); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := s.AppendCompletionItem(launch, Item{
		ID: "done", ThreadID: "t", TurnIndex: 0, Kind: "background_done",
		Role: "assistant", Summary: "ok", CreatedAt: 2,
	}, nil); err != nil {
		t.Fatalf("append completion: %v", err)
	}

	items, err := s.ListItems("t")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	var got Item
	for _, it := range items {
		if it.ID == "done" {
			got = it
		}
	}
	if got.ID == "" {
		t.Fatal("completion row missing from ListItems")
	}
	if got.CompletionOfItemID != "launch" {
		t.Errorf("CompletionOfItemID = %q, want launch", got.CompletionOfItemID)
	}
	if !got.IsBackground {
		t.Error("IsBackground = false, want true")
	}
	if got.Status != "completed" {
		t.Errorf("Status = %q, want completed", got.Status)
	}
}
