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

// Migration v15 finalizes the unified-item schema. These tests pin:
//   - the renamed lifecycle/parent columns exist after migration
//   - the destructive reset semantics when upgrading older schemas
//   - CHECK constraints reject bogus values
//   - the completion-of partial index is created

func TestMigrationV14AddsLifecycleColumns(t *testing.T) {
	s := newTestStore(t)

	cols, err := tableColumns(s.db, "items")
	if err != nil {
		t.Fatalf("tableColumns(items): %v", err)
	}
	for _, want := range []string{"status", "is_background", "completion_of", "parent_id", "tool_name", "decision", "meta"} {
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

// TestMigrationV14BackfillsExistingRows simulates an upgrade from a pre-v15
// database: pre-populate an item under the pre-v15 schema, then apply v15
// and confirm the destructive reset dropped legacy chat rows.
func TestMigrationV14BackfillsExistingRows(t *testing.T) {
	db := openSQLiteDB(t)

	if err := configureDatabase(db); err != nil {
		t.Fatalf("configureDatabase: %v", err)
	}
	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("ensureMigrationTable: %v", err)
	}
	// Apply every migration except v15 (and anything that follows it)
	// so we're on the pre-v15 schema. Later migrations that assume the
	// v15-shaped items table (e.g. v16's idx_items_payload_id) would
	// break against the pre-v15 layout.
	for _, m := range migrations {
		if m.Version >= 15 {
			continue
		}
		if err := applyMigration(db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.Version, err)
		}
	}

	if _, err := db.Exec(`INSERT INTO projects
		(id, path, name, created_at, updated_at)
		VALUES ('p-pre', '/tmp/test', 'Pre-v14 Project', 1000, 1000)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO threads
		(id, project_id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES ('t-pre', 'p-pre', 'Pre-v14', 'claude', '/tmp', '', 1000, 1000)`); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, summary, parent_tool_use_id, created_at)
		VALUES ('i-pre', 't-pre', 0, 0, 'text', 'assistant', 'body', '', 1000)`); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	// Find v15 in the migration list and apply it.
	var v15 *Migration
	for i := range migrations {
		if migrations[i].Version == 15 {
			v15 = &migrations[i]
			break
		}
	}
	if v15 == nil {
		t.Fatal("v15 migration missing from list")
	}
	if err := applyMigration(db, *v15); err != nil {
		t.Fatalf("apply v15: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&count); err != nil {
		t.Fatalf("count items after v15 reset: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected v15 reset to clear legacy items, got %d", count)
	}
}

func TestMigrationV14StatusCheckRejectsBogusValue(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.db.Exec(`INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES ('t', ?, 'T', 'claude', '/tmp', '', 1, 1)`, defaultTestProjectID); err != nil {
		t.Fatalf("seed thread: %v", err)
	}

	// Raw INSERT with a status value outside the allowed enum must fail.
	_, err := s.db.Exec(`INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, summary, parent_id, status, is_background, completion_of, created_at, updated_at)
		VALUES ('i', 't', 0, 0, 'assistant_text', 'assistant', '', '', 'weird', 0, '', 1, 1)`)
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
			(id, thread_id, turn_index, item_index, kind, role, summary, parent_id, status, is_background, completion_of, created_at, updated_at)
			VALUES (?, 't', 0, ?, 'assistant_text', 'assistant', '', '', ?, 0, '', 1, 1)`,
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

	if _, err := s.db.Exec(`INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES ('t', ?, 'T', 'claude', '/tmp', '', 1, 1)`, defaultTestProjectID); err != nil {
		t.Fatalf("seed thread: %v", err)
	}

	// Only 0 and 1 are allowed; 2 must fail.
	if _, err := s.db.Exec(`INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, summary, parent_id, status, is_background, completion_of, created_at, updated_at)
		VALUES ('i', 't', 0, 0, 'assistant_text', 'assistant', '', '', 'completed', 2, '', 1, 1)`); err == nil {
		t.Fatal("INSERT with is_background=2 must violate CHECK constraint")
	}
}

// --- Round-trip through store methods ---

func TestInsertItemRoundTripsLifecycleFields(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	if err := s.CreateThread(Thread{
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
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
	if got.CompletionOf != "" {
		t.Errorf("CompletionOf: got %q, want empty", got.CompletionOf)
	}
}

// TestInsertItemDefaultsStatusToCompleted pins the defaultStatus() coercion:
// existing callers that leave Status empty get "completed" so they don't
// trip the CHECK constraint.
func TestInsertItemDefaultsStatusToCompleted(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	if err := s.CreateThread(Thread{
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "i", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
		Kind:      "assistant_text", Role: "assistant", Summary: "hi", CreatedAt: now,
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
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
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
	if got.CreatedAt != 1000 {
		t.Errorf("CreatedAt = %d, want 1000", got.CreatedAt)
	}
	if got.UpdatedAt != 2000 {
		t.Errorf("UpdatedAt = %d, want 2000", got.UpdatedAt)
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
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
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
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
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
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
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
		ID: "text", ThreadID: "t", TurnIndex: 2, Kind:      "assistant_text",
		Role: "assistant", Summary: "notes", CreatedAt: 1500,
	}); err != nil {
		t.Fatalf("append text: %v", err)
	}

	completion := Item{
		ID: "completion", ThreadID: "t", TurnIndex: 2,
		Kind:      "tool_completion", Role: "assistant", Summary: "build ok",
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
	if got.CompletionOf != "launch" {
		t.Errorf("CompletionOf = %q, want %q", got.CompletionOf, "launch")
	}
	if got.Kind != "tool_completion" {
		t.Errorf("Kind = %q, want tool_completion", got.Kind)
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
// accidentally override IsBackground/CompletionOf by pre-setting
// them on the passed-in completion struct.
func TestAppendCompletionItemForcesInvariants(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	if err := s.CreateThread(Thread{
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
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
		Kind:      "tool_completion", Role: "assistant",
		// Caller tries to pre-stamp lies:
		IsBackground:       false,
		CompletionOf: "some-other-item",
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
	if got.CompletionOf != "launch" {
		t.Errorf("AppendCompletionItem must force CompletionOf=launch.ID, got %q", got.CompletionOf)
	}
}

func TestAppendCompletionItemWithPayloadPersistsAtomically(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	if err := s.CreateThread(Thread{
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
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
		Kind:      "tool_completion", Role: "assistant", Summary: "done",
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
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
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
				Kind:      "tool_completion", Role: "assistant", CreatedAt: int64(10 + n),
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
		if it.CompletionOf != "launch" {
			t.Errorf("completion %s CompletionOf = %q, want launch", it.ID, it.CompletionOf)
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
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
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
		ID: "done", ThreadID: "t", TurnIndex: 0, Kind:      "tool_completion",
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
	if got.CompletionOf != "launch" {
		t.Errorf("CompletionOf = %q, want launch", got.CompletionOf)
	}
	if !got.IsBackground {
		t.Error("IsBackground = false, want true")
	}
	if got.Status != "completed" {
		t.Errorf("Status = %q, want completed", got.Status)
	}
}

// TestUpsertItemIdempotentPreservesItemIndex pins the upsert semantics
// triage relies on: calling UpsertItem twice with the same (thread, id)
// must produce exactly one row whose item_index is the one assigned at
// first insert, even when the second call supplies a different summary
// and/or a payload. Without this invariant a streaming text update would
// re-assign item_index on every delta and the timeline would reorder
// mid-stream.
//
// The second call also exercises the payload upsert path — the refreshed
// payload must land in the same transaction so the re-read returns the
// updated row + payload consistently.
func TestUpsertItemIdempotentPreservesItemIndex(t *testing.T) {
	s := newTestStore(t)
	now := int64(1000)
	if err := s.CreateThread(Thread{
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Seed two prior items so the natural MAX(item_index) is 1 — the
	// first UpsertItem call should land at index 2, and the second call
	// must NOT bump it to 3 or reset it to 0.
	for i, id := range []string{"prior-0", "prior-1"} {
		if err := s.InsertItem(Item{
			ID: id, ThreadID: "t", TurnIndex: 0, ItemIndex: i,
			Kind: "assistant_text", Role: "assistant", Summary: "seed",
			Status: "completed", CreatedAt: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	first := Item{
		ID: "streaming", ThreadID: "t", TurnIndex: 0,
		Kind: "assistant_text", Role: "assistant",
		Status: "streaming", Summary: "first",
		CreatedAt: 2000, UpdatedAt: 2000,
	}
	persistedFirst, err := s.UpsertItem(first, nil)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if persistedFirst.ItemIndex != 2 {
		t.Fatalf("first upsert item_index = %d, want 2 (MAX+1)", persistedFirst.ItemIndex)
	}

	second := Item{
		ID: "streaming", ThreadID: "t", TurnIndex: 0,
		Kind: "assistant_text", Role: "assistant",
		Status: "completed", Summary: "first + second",
		CreatedAt: 3000, UpdatedAt: 3000,
	}
	payload := &Payload{
		ID:        "streaming-p",
		Kind:      "assistant_text",
		Meta:      `{"preview":"first + second"}`,
		Data:      []byte("first + second"),
		CreatedAt: 3000,
	}
	persistedSecond, err := s.UpsertItem(second, payload)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if persistedSecond.ItemIndex != persistedFirst.ItemIndex {
		t.Errorf("item_index drifted across upserts: first=%d, second=%d",
			persistedFirst.ItemIndex, persistedSecond.ItemIndex)
	}
	if persistedSecond.Summary != "first + second" {
		t.Errorf("summary not refreshed by second upsert: got %q", persistedSecond.Summary)
	}
	if persistedSecond.Status != "completed" {
		t.Errorf("status not refreshed: got %q", persistedSecond.Status)
	}
	if persistedSecond.PayloadID != "streaming-p" {
		t.Errorf("payload not linked to upserted row: got %q", persistedSecond.PayloadID)
	}
	// CreatedAt must be pinned to the first insert — triage relies on this
	// invariant so timestamps stay stable across streaming updates.
	if persistedSecond.CreatedAt != persistedFirst.CreatedAt {
		t.Errorf("created_at changed on upsert: first=%d, second=%d",
			persistedFirst.CreatedAt, persistedSecond.CreatedAt)
	}

	// Exactly one row for (thread, id).
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM items WHERE thread_id = ? AND id = ?`,
		"t", "streaming",
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row after two upserts, got %d", count)
	}

	// Payload landed atomically — its data column matches the second call.
	data, err := s.GetPayloadData("streaming-p")
	if err != nil {
		t.Fatalf("get payload data: %v", err)
	}
	if string(data) != "first + second" {
		t.Errorf("payload data = %q, want %q", string(data), "first + second")
	}
}

// TestAppendItemSummaryConcatenatesInPlace pins the hot-path streaming
// behaviour: AppendItemSummary must append the delta to the existing
// summary column in a single UPDATE and return the re-read row. The
// item's item_index and created_at must not change across calls —
// exact same invariants as UpsertItem relies on.
func TestAppendItemSummaryConcatenatesInPlace(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(Thread{
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: 1000, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Seed a streaming assistant_text row with its first delta.
	first := Item{
		ID: "stream", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant",
		Status: "streaming", Summary: "hello ",
		CreatedAt: 2000, UpdatedAt: 2000,
	}
	if err := s.InsertItem(first); err != nil {
		t.Fatalf("insert first: %v", err)
	}

	got, err := s.AppendItemSummary("stream", "world", 3000)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if got.Summary != "hello world" {
		t.Errorf("after append Summary = %q, want %q", got.Summary, "hello world")
	}
	if got.CreatedAt != 2000 {
		t.Errorf("CreatedAt changed across append: got %d, want 2000", got.CreatedAt)
	}
	if got.UpdatedAt != 3000 {
		t.Errorf("UpdatedAt = %d, want 3000", got.UpdatedAt)
	}
	if got.ItemIndex != 0 {
		t.Errorf("ItemIndex drifted: got %d, want 0", got.ItemIndex)
	}

	// A second append chains onto the new state, not the original.
	got2, err := s.AppendItemSummary("stream", "!", 4000)
	if err != nil {
		t.Fatalf("append2: %v", err)
	}
	if got2.Summary != "hello world!" {
		t.Errorf("after 2nd append Summary = %q, want %q", got2.Summary, "hello world!")
	}

	// Thread updated_at moves in the same transaction.
	thr, err := s.GetThread("t")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if thr.UpdatedAt != 4000 {
		t.Errorf("thread UpdatedAt = %d, want 4000", thr.UpdatedAt)
	}
}

func TestAppendItemSummaryErrorsOnMissingItem(t *testing.T) {
	s := newTestStore(t)
	_, err := s.AppendItemSummary("nonexistent", "x", 100)
	if err == nil {
		t.Fatal("expected error for missing item")
	}
}

// BenchmarkTextDeltaGrowth approximates the streaming hot path: a
// single row receives many deltas totalling the same final size. With
// the old GetThreadItem → concat → UpsertItem path the work was
// quadratic; AppendItemSummary is linear.
//
// Run with `go test ./internal/store -bench=BenchmarkTextDeltaGrowth -count=1`.
func BenchmarkTextDeltaGrowth(b *testing.B) {
	s := newBenchStore(b)
	if err := s.CreateThread(Thread{
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		b.Fatalf("create thread: %v", err)
	}
	delta := "abcdefghijklmnopqrstuvwxyz0123456789" // 36 bytes
	const deltas = 200

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		id := "stream-" + b.Name() + "-" + strconvItoa(n)
		if err := s.InsertItem(Item{
			ID: id, ThreadID: "t", TurnIndex: n, ItemIndex: 0,
			Kind: "assistant_text", Role: "assistant",
			Status: "streaming", Summary: delta, CreatedAt: 1,
		}); err != nil {
			b.Fatalf("insert: %v", err)
		}
		for i := 1; i < deltas; i++ {
			if _, err := s.AppendItemSummary(id, delta, int64(2+i)); err != nil {
				b.Fatalf("append: %v", err)
			}
		}
	}
}

// newBenchStore is a test/benchmark store helper that avoids the
// t.Cleanup indirection newTestStore uses so benchmarks don't pay
// for teardown bookkeeping.
func newBenchStore(b *testing.B) *Store {
	b.Helper()
	s, err := New(":memory:")
	if err != nil {
		b.Fatalf("new store: %v", err)
	}
	b.Cleanup(func() { s.Close() })
	now := time.Now().UnixMilli()
	if err := s.CreateProject(Project{
		ID:        defaultTestProjectID,
		Path:      "/tmp/test",
		Name:      "Default Test Project",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		b.Fatalf("seed default project: %v", err)
	}
	return s
}

// strconvItoa inlined to keep benchmark free of strconv import diffs.
func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestAppendPayloadDataAppendsInPlace mirrors AppendItemSummary: the
// payload.data blob extends in one UPDATE without reading the full
// blob into Go memory first.
func TestAppendPayloadDataAppendsInPlace(t *testing.T) {
	s := newTestStore(t)
	if err := s.InsertPayload(Payload{
		ID: "p", Kind: "command_output", Meta: `{"v":1}`,
		Data: []byte("hello "), CreatedAt: 1000,
	}); err != nil {
		t.Fatalf("insert payload: %v", err)
	}

	if err := s.AppendPayloadData("p", []byte("world"), `{"v":2}`, 2000); err != nil {
		t.Fatalf("append: %v", err)
	}
	data, err := s.GetPayloadData("p")
	if err != nil {
		t.Fatalf("get data: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("data = %q, want %q", data, "hello world")
	}
	meta, err := s.GetPayloadMeta("p")
	if err != nil {
		t.Fatalf("get meta: %v", err)
	}
	if meta.Meta != `{"v":2}` {
		t.Errorf("meta = %q, want %q", meta.Meta, `{"v":2}`)
	}
	if meta.CreatedAt != 2000 {
		t.Errorf("CreatedAt = %d, want 2000", meta.CreatedAt)
	}
}

func TestAppendPayloadDataErrorsOnMissingPayload(t *testing.T) {
	s := newTestStore(t)
	err := s.AppendPayloadData("nope", []byte("x"), "{}", 1)
	if err == nil {
		t.Fatal("expected error for missing payload")
	}
}

// TestGetPayloadPreviewSliceInsideSQLite covers the new substr-based
// preview path. Large payloads must never cross into Go memory past
// the requested head size.
func TestGetPayloadPreviewSliceInsideSQLite(t *testing.T) {
	s := newTestStore(t)
	// 64 KiB payload; preview should only pull the first 4 KiB.
	const total = 64 * 1024
	const maxBytes = 4 * 1024
	full := make([]byte, total)
	for i := range full {
		full[i] = byte('A' + (i % 26))
	}
	if err := s.InsertPayload(Payload{
		ID: "p-big", Kind: "diff", Meta: "{}",
		Data: full, CreatedAt: 1,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	head, totalSize, complete, err := s.GetPayloadPreview("p-big", maxBytes)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if totalSize != total {
		t.Errorf("totalSize = %d, want %d", totalSize, total)
	}
	if complete {
		t.Error("complete flag should be false when head < total")
	}
	if len(head) != maxBytes {
		t.Errorf("len(head) = %d, want %d", len(head), maxBytes)
	}
	for i := 0; i < maxBytes; i++ {
		if head[i] != full[i] {
			t.Fatalf("head[%d] = %v, want %v", i, head[i], full[i])
		}
	}

	// Fully-contained request returns the whole blob and complete=true.
	head2, total2, complete2, err := s.GetPayloadPreview("p-big", total*2)
	if err != nil {
		t.Fatalf("preview2: %v", err)
	}
	if total2 != total {
		t.Errorf("total2 = %d, want %d", total2, total)
	}
	if !complete2 {
		t.Error("complete flag should be true when maxBytes >= total")
	}
	if len(head2) != total {
		t.Errorf("full read length = %d, want %d", len(head2), total)
	}
}

// TestGetItemByPayloadIDUsesIndex verifies the direct lookup replaces
// the O(threads × items) walk the app layer used to do. A missing
// payload returns (zero, false, nil); a hit returns the owning item.
func TestGetItemByPayloadIDUsesIndex(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(Thread{
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := s.InsertPayload(Payload{
		ID: "p", Kind: "diff", Meta: "{}", Data: []byte("body"), CreatedAt: 1,
	}); err != nil {
		t.Fatalf("insert payload: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "owner", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", Summary: "s",
		PayloadID: "p", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("insert owner: %v", err)
	}

	got, found, err := s.GetItemByPayloadID("p")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !found {
		t.Fatal("expected payload lookup to find owner")
	}
	if got.ID != "owner" {
		t.Errorf("got item id %q, want owner", got.ID)
	}
	if got.PayloadKind != "diff" {
		t.Errorf("got PayloadKind = %q, want diff", got.PayloadKind)
	}

	// Missing payload: no error, found=false.
	_, found2, err := s.GetItemByPayloadID("no-such-payload")
	if err != nil {
		t.Fatalf("missing lookup error: %v", err)
	}
	if found2 {
		t.Error("expected found=false for missing payload")
	}
}

// TestItemsDecisionCHECKRejectsBogusValue pins the v15 CHECK constraint
// on items.decision. Only the enumerated set is legal at the SQL layer;
// triage and the frontend depend on this to prune impossible states at
// the store boundary (an invalid decision reaching the frontend would
// quietly break ToolDecisionChip branching).
func TestItemsDecisionCHECKRejectsBogusValue(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	if err := s.CreateThread(Thread{
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Bogus decision must trigger the CHECK constraint.
	_, err := s.db.Exec(`INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, summary, parent_id, status,
		 is_background, completion_of, tool_name, decision, meta, created_at, updated_at)
		VALUES ('bogus', 't', 0, 0, 'tool_call', 'assistant', '', '', 'completed',
		 0, '', '', 'maybe', '{}', 1, 1)`)
	if err == nil {
		t.Fatal("INSERT with decision='maybe' must violate CHECK constraint")
	}
	if !strings.Contains(err.Error(), "CHECK") && !strings.Contains(err.Error(), "constraint") {
		t.Errorf("error = %v, want CHECK constraint violation", err)
	}

	// Every legal decision inserts successfully. Table-driven so a future
	// addition fails obviously (and reminds the dev to update the test).
	cases := []struct {
		name     string
		decision string
	}{
		{name: "empty", decision: ""},
		{name: "approved", decision: "approved"},
		{name: "declined", decision: "declined"},
		{name: "amended", decision: "amended"},
		{name: "timeout", decision: "timeout"},
		{name: "lost", decision: "lost"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := fmt.Sprintf("ok-decision-%d", i)
			if _, err := s.db.Exec(`INSERT INTO items
				(id, thread_id, turn_index, item_index, kind, role, summary, parent_id, status,
				 is_background, completion_of, tool_name, decision, meta, created_at, updated_at)
				VALUES (?, 't', 0, ?, 'tool_call', 'assistant', '', '', 'completed',
				 0, '', '', ?, '{}', 1, 1)`, id, i, tc.decision); err != nil {
				t.Errorf("INSERT decision=%q: %v", tc.decision, err)
			}
		})
	}

	// UPDATE to a bogus value must also fail — covers the case where
	// runtime Go code accidentally writes a new invalid decision onto
	// an existing row.
	if _, err := s.db.Exec(`UPDATE items SET decision = 'banana' WHERE id = 'ok-decision-0'`); err == nil {
		t.Error("UPDATE to bogus decision must fail")
	}
}

// TestListRunningBackgroundToolCallsFiltersCorrectly exercises the
// store-level query the on-reopen reconciler uses. We seed a mixed set
// of item rows and assert only the running + is_background=1 +
// kind=tool_call rows come back.
//
// Without the push-down filter the reconciler would have to scan every
// item on every reopen; the test guards against a regression that
// would accidentally return, e.g., completed rows or inline tool calls.
func TestListRunningBackgroundToolCallsFiltersCorrectly(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	if err := s.CreateThread(Thread{
		ID: "t-reconcile", ProjectID: defaultTestProjectID, Title: "T", Provider: "codex", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Seed (must match): running background tool_call
	if _, err := s.AppendItem(Item{
		ID: "match-running-bg", ThreadID: "t-reconcile", TurnIndex: 1,
		Kind: "tool_call", Role: "assistant", Status: "running",
		IsBackground: true, Summary: "Bash: sleep 999", ToolName: "Bash",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed match-running-bg: %v", err)
	}
	// Seed (should NOT match): completed background tool_call
	if _, err := s.AppendItem(Item{
		ID: "skip-completed-bg", ThreadID: "t-reconcile", TurnIndex: 1,
		Kind: "tool_call", Role: "assistant", Status: "completed",
		IsBackground: true, Summary: "Bash: done", ToolName: "Bash",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed skip-completed-bg: %v", err)
	}
	// Seed (should NOT match): running inline (non-background) tool_call
	if _, err := s.AppendItem(Item{
		ID: "skip-running-inline", ThreadID: "t-reconcile", TurnIndex: 1,
		Kind: "tool_call", Role: "assistant", Status: "running",
		IsBackground: false, Summary: "Read: /tmp/x", ToolName: "Read",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed skip-running-inline: %v", err)
	}
	// Seed (should NOT match): running background non-tool_call kind
	if _, err := s.AppendItem(Item{
		ID: "skip-running-non-tool", ThreadID: "t-reconcile", TurnIndex: 1,
		Kind: "assistant_text", Role: "assistant", Status: "running",
		IsBackground: true, Summary: "streaming text",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed skip-running-non-tool: %v", err)
	}
	// Seed a second running bg on a different thread — must NOT come
	// back when scoping to t-reconcile.
	if err := s.CreateThread(Thread{
		ID: "t-other", ProjectID: defaultTestProjectID, Title: "Other", Provider: "codex", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create other thread: %v", err)
	}
	if _, err := s.AppendItem(Item{
		ID: "skip-other-thread", ThreadID: "t-other", TurnIndex: 1,
		Kind: "tool_call", Role: "assistant", Status: "running",
		IsBackground: true, Summary: "Bash: other", ToolName: "Bash",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed skip-other-thread: %v", err)
	}

	got, err := s.ListRunningBackgroundToolCalls("t-reconcile")
	if err != nil {
		t.Fatalf("ListRunningBackgroundToolCalls: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].ID != "match-running-bg" {
		t.Fatalf("got[0].ID = %q, want match-running-bg", got[0].ID)
	}
}

// TestListRunningBackgroundToolCallsEmptyThread returns no rows and no
// error when the thread has no items — the reconciler tolerates an
// empty thread and this pins that contract.
func TestListRunningBackgroundToolCallsEmptyThread(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	if err := s.CreateThread(Thread{
		ID: "t-empty", ProjectID: defaultTestProjectID, Title: "Empty", Provider: "codex", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	got, err := s.ListRunningBackgroundToolCalls("t-empty")
	if err != nil {
		t.Fatalf("ListRunningBackgroundToolCalls: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d rows on empty thread, want 0", len(got))
	}
}
