package store

import (
	"errors"
	"fmt"
	"os"
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

func TestItemsTableHasLifecycleColumns(t *testing.T) {
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

	var name string
	if err := s.db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='index' AND name = 'idx_items_completion_of'",
	).Scan(&name); err != nil {
		t.Errorf("idx_items_completion_of index missing: %v", err)
	}
}

func TestItemsStatusCheckRejectsBogusValue(t *testing.T) {
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

func TestItemsIsBackgroundCheckRejectsBogusValue(t *testing.T) {
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
	got, ok, err := s.GetThreadItem("t", "i-running")
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
		Kind: "assistant_text", Role: "assistant", Summary: "hi", CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, _, err := s.GetThreadItem("t", "i")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != "completed" {
		t.Errorf("Status = %q, want %q (default)", got.Status, "completed")
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
		Kind: "tool_call", Role: "assistant", Summary: "pnpm build",
		Status: "completed", IsBackground: true, CreatedAt: now,
	}
	if err := s.InsertItem(launch); err != nil {
		t.Fatalf("insert launch: %v", err)
	}

	// A sibling text item lands in between so item_index assignment has
	// something to bump past.
	if _, err := s.AppendItem(Item{
		ID: "text", ThreadID: "t", TurnIndex: 2, Kind: "assistant_text",
		Role: "assistant", Summary: "notes", CreatedAt: 1500,
	}); err != nil {
		t.Fatalf("append text: %v", err)
	}

	completion := Item{
		ID: "completion", ThreadID: "t", TurnIndex: 2,
		Kind: "tool_completion", Role: "assistant", Summary: "build ok",
		CreatedAt: 2000,
	}
	idx, err := s.AppendCompletionItem(launch, completion, nil)
	if err != nil {
		t.Fatalf("append completion: %v", err)
	}
	if idx != 1 {
		t.Errorf("completion item_index = %d, want 1 (after text at 0)", idx)
	}

	got, ok, err := s.GetThreadItem("t", "completion")
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

	// Background-task completion siblings are not a sidebar-bump
	// boundary — the turn-settle path owns the activity bump.
	thr, err := s.GetThread("t")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if thr.UpdatedAt != 1000 {
		t.Errorf("thread updated_at = %d, want 1000 (AppendCompletionItem must not bump activity)", thr.UpdatedAt)
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
		Kind: "tool_completion", Role: "assistant",
		// Caller tries to pre-stamp lies:
		IsBackground: false,
		CompletionOf: "some-other-item",
		CreatedAt:    2,
	}
	if _, err := s.AppendCompletionItem(launch, completion, nil); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, _, err := s.GetThreadItem("t", "completion")
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
		Kind: "tool_completion", Role: "assistant", Summary: "done",
		CreatedAt: 2,
	}
	idx, err := s.AppendCompletionItem(launch, completion, payload)
	if err != nil {
		t.Fatalf("append with payload: %v", err)
	}
	if idx != 1 {
		t.Errorf("completion item_index = %d, want 1", idx)
	}

	got, _, err := s.GetThreadItem("t", "completion")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PayloadID != "p" {
		t.Errorf("completion PayloadID = %q, want p", got.PayloadID)
	}
	meta, err := s.GetPayloadMeta("t", "p")
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
				Kind: "tool_completion", Role: "assistant", CreatedAt: int64(10 + n),
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
		Kind: "tool_call", Role: "assistant", Summary: "pnpm build",
		Status: "completed", IsBackground: true, CreatedAt: now,
	}
	if err := s.InsertItem(launch); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := s.AppendCompletionItem(launch, Item{
		ID: "done", ThreadID: "t", TurnIndex: 0, Kind: "tool_completion",
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
	data, err := s.GetPayloadData("t", "streaming-p")
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

	got, err := s.AppendItemSummary("t", "stream", "world", 3000)
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
	got2, err := s.AppendItemSummary("t", "stream", "!", 4000)
	if err != nil {
		t.Fatalf("append2: %v", err)
	}
	if got2.Summary != "hello world!" {
		t.Errorf("after 2nd append Summary = %q, want %q", got2.Summary, "hello world!")
	}

	// Streaming text appends are NOT a sidebar-bump boundary. This is
	// the regression guard for the "sidebar reshuffles on every chunk"
	// bug: AppendItemSummary leaves threads.updated_at alone so the
	// sort key stays anchored at the last interaction.
	thr, err := s.GetThread("t")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if thr.UpdatedAt != 1000 {
		t.Errorf("thread UpdatedAt = %d, want 1000 (AppendItemSummary must not bump activity)", thr.UpdatedAt)
	}
}

func TestAppendItemSummaryErrorsOnMissingItem(t *testing.T) {
	s := newTestStore(t)
	_, err := s.AppendItemSummary("t", "nonexistent", "x", 100)
	if err == nil {
		t.Fatal("expected error for missing item")
	}
}

func TestAppendItemSummaryZeroRowSemantics(t *testing.T) {
	for _, tc := range []struct {
		name   string
		append func(s *Store, threadID string, itemID string) (Item, error)
	}{
		{
			name: "plain",
			append: func(s *Store, threadID string, itemID string) (Item, error) {
				return s.AppendItemSummary(threadID, itemID, "late", 3000)
			},
		},
		{
			name: "tail",
			append: func(s *Store, threadID string, itemID string) (Item, error) {
				return s.AppendItemSummaryTail(threadID, itemID, "late", 6, 3000)
			},
		},
	} {
		t.Run(tc.name+"/missing row", func(t *testing.T) {
			s := newTestStore(t)
			_, err := tc.append(s, "t", "missing")
			if !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("err = %v, want sql.ErrNoRows", err)
			}
		})

		t.Run(tc.name+"/settled row", func(t *testing.T) {
			s := newTestStore(t)
			if err := s.CreateThread(Thread{
				ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
				CreatedAt: 1000, UpdatedAt: 1000,
			}); err != nil {
				t.Fatalf("create thread: %v", err)
			}
			if err := s.InsertItem(Item{
				ID: "settled", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
				Kind: "assistant_text", Role: "assistant", Status: "completed",
				Summary: "final", CreatedAt: 2000, UpdatedAt: 2000,
			}); err != nil {
				t.Fatalf("insert settled item: %v", err)
			}

			_, err := tc.append(s, "t", "settled")
			if !errors.Is(err, ErrItemSettled) {
				t.Fatalf("err = %v, want ErrItemSettled", err)
			}
			got, ok, err := s.GetThreadItem("t", "settled")
			if err != nil {
				t.Fatalf("get settled item: %v", err)
			}
			if !ok {
				t.Fatal("settled item missing")
			}
			if got.Summary != "final" {
				t.Fatalf("settled summary mutated to %q, want final", got.Summary)
			}
			if got.UpdatedAt != 2000 {
				t.Fatalf("settled UpdatedAt mutated to %d, want 2000", got.UpdatedAt)
			}
		})
	}
}

func TestAppendItemSummaryTailKeepsLatestRunes(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(Thread{
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: 1000, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "think", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
		Kind: "thinking", Role: "assistant", Status: "streaming",
		Summary: "abcd", CreatedAt: 2000, UpdatedAt: 2000,
	}); err != nil {
		t.Fatalf("insert item: %v", err)
	}

	// First append stays under the 6-rune cap → plain concat.
	got, err := s.AppendItemSummaryTail("t", "think", "ef", 6, 3000)
	if err != nil {
		t.Fatalf("append tail: %v", err)
	}
	if got.Summary != "abcdef" {
		t.Fatalf("summary under cap = %q, want %q", got.Summary, "abcdef")
	}

	// Second append crosses the cap → keeps the LAST 6 characters of
	// (summary || delta). Old head-cap behaviour would have stayed at
	// "abcdef" and silently dropped "ghij"; tail-cap is what makes the
	// 3-line tail viewport correct after streaming settle.
	got, err = s.AppendItemSummaryTail("t", "think", "ghij", 6, 4000)
	if err != nil {
		t.Fatalf("append tail past cap: %v", err)
	}
	if got.Summary != "efghij" {
		t.Fatalf("summary after tail-cap = %q, want %q", got.Summary, "efghij")
	}

	// Third append: another tail slice — the cap should track the
	// rolling end across multiple flushes, not freeze at the first
	// capped value.
	got, err = s.AppendItemSummaryTail("t", "think", "klmn", 6, 5000)
	if err != nil {
		t.Fatalf("append tail rolling: %v", err)
	}
	if got.Summary != "ijklmn" {
		t.Fatalf("summary after second tail-cap = %q, want %q", got.Summary, "ijklmn")
	}
}

func TestUpdateItemFieldsPartialUpdate(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(Thread{
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: 1000, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "i1", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant", Status: "streaming",
		Summary: "Hello world", Meta: `{"pathRefs":[]}`, Decision: "",
		CreatedAt: 2000, UpdatedAt: 2000,
	}); err != nil {
		t.Fatalf("insert item: %v", err)
	}

	t.Run("status-only preserves other fields", func(t *testing.T) {
		status := "completed"
		ts := int64(3000)
		if err := s.UpdateItemFields("t", "i1", ItemPartialUpdate{
			Status: &status, UpdatedAt: &ts,
		}); err != nil {
			t.Fatalf("update: %v", err)
		}
		got, found, err := s.GetThreadItem("t", "i1")
		if err != nil || !found {
			t.Fatalf("get: err=%v found=%v", err, found)
		}
		if got.Status != "completed" {
			t.Errorf("status: got %q, want %q", got.Status, "completed")
		}
		if got.Summary != "Hello world" {
			t.Errorf("summary should be preserved: got %q", got.Summary)
		}
		if got.Meta != `{"pathRefs":[]}` {
			t.Errorf("meta should be preserved: got %q", got.Meta)
		}
		if got.UpdatedAt != 3000 {
			t.Errorf("updatedAt: got %d, want 3000", got.UpdatedAt)
		}
	})

	t.Run("meta-only preserves other fields", func(t *testing.T) {
		meta := `{"pathRefs":[{"path":"foo.go"}]}`
		if err := s.UpdateItemFields("t", "i1", ItemPartialUpdate{
			Meta: &meta,
		}); err != nil {
			t.Fatalf("update: %v", err)
		}
		got, found, err := s.GetThreadItem("t", "i1")
		if err != nil || !found {
			t.Fatalf("get: err=%v found=%v", err, found)
		}
		if got.Meta != meta {
			t.Errorf("meta: got %q, want %q", got.Meta, meta)
		}
		if got.Status != "completed" {
			t.Errorf("status should be preserved: got %q", got.Status)
		}
		if got.Summary != "Hello world" {
			t.Errorf("summary should be preserved: got %q", got.Summary)
		}
	})

	t.Run("all fields", func(t *testing.T) {
		status := "errored"
		summary := "Error occurred"
		meta := `{"error":true}`
		decision := "declined"
		ts := int64(5000)
		if err := s.UpdateItemFields("t", "i1", ItemPartialUpdate{
			Status: &status, Summary: &summary, Meta: &meta, Decision: &decision, UpdatedAt: &ts,
		}); err != nil {
			t.Fatalf("update: %v", err)
		}
		got, found, err := s.GetThreadItem("t", "i1")
		if err != nil || !found {
			t.Fatalf("get: err=%v found=%v", err, found)
		}
		if got.Status != "errored" {
			t.Errorf("status: got %q", got.Status)
		}
		if got.Summary != "Error occurred" {
			t.Errorf("summary: got %q", got.Summary)
		}
		if got.Meta != `{"error":true}` {
			t.Errorf("meta: got %q", got.Meta)
		}
		if got.Decision != "declined" {
			t.Errorf("decision: got %q", got.Decision)
		}
		if got.UpdatedAt != 5000 {
			t.Errorf("updatedAt: got %d", got.UpdatedAt)
		}
	})

	t.Run("empty update errors", func(t *testing.T) {
		if err := s.UpdateItemFields("t", "i1", ItemPartialUpdate{}); err == nil {
			t.Fatal("expected error for empty update")
		}
	})

	t.Run("nonexistent row errors", func(t *testing.T) {
		status := "completed"
		if err := s.UpdateItemFields("t", "nonexistent", ItemPartialUpdate{Status: &status}); err == nil {
			t.Fatal("expected error for nonexistent row")
		}
	})
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
			if _, err := s.AppendItemSummary("t", id, delta, int64(2+i)); err != nil {
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

// TestAppendPayloadDataAppendsAsChunks mirrors AppendItemSummary at the API
// level while keeping the write path append-only. Appended bytes live in
// payload_chunks so streaming writers do not rewrite the cumulative blob.
func TestAppendPayloadDataAppendsAsChunks(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	if err := seedPayloadRow(s, "t", Payload{
		ID: "p", Kind: "command_output", Meta: `{"v":1}`,
		Data: []byte("hello "), CreatedAt: 1000,
	}); err != nil {
		t.Fatalf("insert payload: %v", err)
	}

	if err := s.AppendPayloadData("t", "p", []byte("world"), `{"v":2}`, 2000); err != nil {
		t.Fatalf("append: %v", err)
	}
	data, err := s.GetPayloadData("t", "p")
	if err != nil {
		t.Fatalf("get data: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("data = %q, want %q", data, "hello world")
	}
	var chunks int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM payload_chunks WHERE payload_id = 'p'`).Scan(&chunks); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if chunks != 1 {
		t.Errorf("chunks = %d, want 1", chunks)
	}
	meta, err := s.GetPayloadMeta("t", "p")
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

func TestPayloadChunkReadsSpanBaseAndAppendedChunks(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	if err := seedPayloadRow(s, "t", Payload{
		ID: "p", Kind: "command_output", Meta: `{"v":1}`,
		Data: []byte("abcd"), CreatedAt: 1,
	}); err != nil {
		t.Fatalf("insert payload: %v", err)
	}
	if err := s.AppendPayloadData("t", "p", []byte("efgh"), `{"v":2}`, 2); err != nil {
		t.Fatalf("append first chunk: %v", err)
	}
	if err := s.AppendPayloadData("t", "p", []byte("ijkl"), `{"v":3}`, 3); err != nil {
		t.Fatalf("append second chunk: %v", err)
	}

	chunk, total, complete, err := s.GetPayloadChunk("t", "p", 2, 7)
	if err != nil {
		t.Fatalf("chunk: %v", err)
	}
	if string(chunk) != "cdefghi" {
		t.Fatalf("chunk = %q, want cdefghi", chunk)
	}
	if total != 12 {
		t.Fatalf("total = %d, want 12", total)
	}
	if complete {
		t.Fatal("chunk should not complete payload")
	}

	tail, total, complete, err := s.GetPayloadChunk("t", "p", 9, 99)
	if err != nil {
		t.Fatalf("tail chunk: %v", err)
	}
	if string(tail) != "jkl" {
		t.Fatalf("tail = %q, want jkl", tail)
	}
	if total != 12 || !complete {
		t.Fatalf("tail result total=%d complete=%v, want total=12 complete=true", total, complete)
	}
}

func TestAppendPayloadDataErrorsOnMissingPayload(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	err := s.AppendPayloadData("t", "nope", []byte("x"), "{}", 1)
	if err == nil {
		t.Fatal("expected error for missing payload")
	}
}

func TestReplacePayloadDataReplacesInPlace(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	if err := seedPayloadRow(s, "t", Payload{
		ID: "p", Kind: "thinking", Meta: `{"v":1}`,
		Data: []byte("streamed draft"), CreatedAt: 1000,
	}); err != nil {
		t.Fatalf("insert payload: %v", err)
	}
	if err := s.AppendPayloadData("t", "p", []byte(" plus chunk"), `{"v":1}`, 1500); err != nil {
		t.Fatalf("append chunk before replace: %v", err)
	}

	if err := s.ReplacePayloadData("t", "p", []byte("final text"), `{"v":2}`, 2000); err != nil {
		t.Fatalf("replace: %v", err)
	}
	var chunks int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM payload_chunks WHERE payload_id = 'p'`).Scan(&chunks); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if chunks != 0 {
		t.Fatalf("chunks = %d, want 0 after replace", chunks)
	}
	data, err := s.GetPayloadData("t", "p")
	if err != nil {
		t.Fatalf("get data: %v", err)
	}
	if string(data) != "final text" {
		t.Errorf("data = %q, want final text", data)
	}
	meta, err := s.GetPayloadMeta("t", "p")
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

// TestGetPayloadPreviewSliceInsideSQLite covers the new substr-based
// preview path. Large payloads must never cross into Go memory past
// the requested head size.
func TestGetPayloadPreviewSliceInsideSQLite(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	// 64 KiB payload; preview should only pull the first 4 KiB.
	const total = 64 * 1024
	const maxBytes = 4 * 1024
	full := make([]byte, total)
	for i := range full {
		full[i] = byte('A' + (i % 26))
	}
	if err := seedPayloadRow(s, "t", Payload{
		ID: "p-big", Kind: "diff", Meta: "{}",
		Data: full, CreatedAt: 1,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	head, totalSize, complete, err := s.GetPayloadPreview("t", "p-big", maxBytes)
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
	head2, total2, complete2, err := s.GetPayloadPreview("t", "p-big", total*2)
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

func TestGetPayloadChunkSliceInsideSQLite(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	const total = 32 * 1024
	const first = 3 * 1024
	const second = 5 * 1024

	full := make([]byte, total)
	for i := range full {
		full[i] = byte('a' + (i % 26))
	}
	if err := seedPayloadRow(s, "t", Payload{
		ID: "p-chunk", Kind: "command_output", Meta: "{}",
		Data: full, CreatedAt: 1,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	chunk1, total1, complete1, err := s.GetPayloadChunk("t", "p-chunk", 0, first)
	if err != nil {
		t.Fatalf("chunk1: %v", err)
	}
	if total1 != total {
		t.Fatalf("total1 = %d, want %d", total1, total)
	}
	if complete1 {
		t.Fatal("chunk1 should not complete the payload")
	}
	if len(chunk1) != first {
		t.Fatalf("len(chunk1) = %d, want %d", len(chunk1), first)
	}
	for i := range chunk1 {
		if chunk1[i] != full[i] {
			t.Fatalf("chunk1[%d] = %v, want %v", i, chunk1[i], full[i])
		}
	}

	chunk2, total2, complete2, err := s.GetPayloadChunk("t", "p-chunk", len(chunk1), second)
	if err != nil {
		t.Fatalf("chunk2: %v", err)
	}
	if total2 != total {
		t.Fatalf("total2 = %d, want %d", total2, total)
	}
	if complete2 {
		t.Fatal("chunk2 should not complete the payload")
	}
	if len(chunk2) != second {
		t.Fatalf("len(chunk2) = %d, want %d", len(chunk2), second)
	}
	for i := range chunk2 {
		want := full[len(chunk1)+i]
		if chunk2[i] != want {
			t.Fatalf("chunk2[%d] = %v, want %v", i, chunk2[i], want)
		}
	}
}

func TestFindNotificationItemByTaskIDReturnsNewestNotification(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(Thread{
		ID: "t-notify", ProjectID: defaultTestProjectID, Title: "T",
		Provider: "claude", WorkspacePath: "/tmp", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "note-old", ThreadID: "t-notify", TurnIndex: 0, ItemIndex: 0,
		Kind: "notification", Role: "system", Summary: "older",
		Meta:      `{"task_id":"task-1","output_file_state":"loading"}`,
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("insert old notification: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "note-new", ThreadID: "t-notify", TurnIndex: 0, ItemIndex: 1,
		Kind: "notification", Role: "system", Summary: "newer",
		Meta:      `{"task_id":"task-1","output_file_state":"loaded"}`,
		CreatedAt: 2, UpdatedAt: 2,
	}); err != nil {
		t.Fatalf("insert new notification: %v", err)
	}

	got, found, err := s.FindNotificationItemByTaskID("t-notify", "task-1")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !found {
		t.Fatal("expected notification lookup to find a row")
	}
	if got.ID != "note-new" {
		t.Fatalf("got %q, want note-new", got.ID)
	}
	if got.Summary != "newer" {
		t.Fatalf("got summary %q, want newer", got.Summary)
	}
}

func TestGetThreadItemByPayloadIDScopesLookupToOwnerThread(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	for _, threadID := range []string{"t-a", "t-b"} {
		if err := s.CreateThread(Thread{
			ID: threadID, ProjectID: defaultTestProjectID, Title: threadID, Provider: "claude", WorkspacePath: "/tmp",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create thread %s: %v", threadID, err)
		}
	}
	if err := seedPayloadRow(s, "t-a", Payload{
		ID: "shared-payload", Kind: "command_output", Meta: "{}", Data: []byte("body"), CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert payload: %v", err)
	}
	if err := seedPayloadRow(s, "t-b", Payload{
		ID: "shared-payload", Kind: "command_output", Meta: "{}", Data: []byte("other body"), CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert second thread payload: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "owner-a", ThreadID: "t-a", TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", Summary: "owner a",
		PayloadID: "shared-payload", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert owner-a: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "owner-b", ThreadID: "t-b", TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", Summary: "owner b",
		PayloadID: "shared-payload", CreatedAt: now + 1, UpdatedAt: now + 1,
	}); err != nil {
		t.Fatalf("insert owner-b: %v", err)
	}

	gotA, foundA, err := s.GetThreadItemByPayloadID("t-a", "shared-payload")
	if err != nil {
		t.Fatalf("lookup t-a: %v", err)
	}
	if !foundA {
		t.Fatal("expected payload lookup to find thread owner")
	}
	if gotA.ID != "owner-a" {
		t.Fatalf("thread-scoped lookup returned %q, want owner-a", gotA.ID)
	}

	gotB, foundB, err := s.GetThreadItemByPayloadID("t-b", "shared-payload")
	if err != nil {
		t.Fatalf("lookup t-b: %v", err)
	}
	if !foundB {
		t.Fatal("expected payload lookup to find second thread owner")
	}
	if gotB.ID != "owner-b" {
		t.Fatalf("thread-scoped lookup returned %q, want owner-b", gotB.ID)
	}

	_, foundMissing, err := s.GetThreadItemByPayloadID("t-missing", "shared-payload")
	if err != nil {
		t.Fatalf("lookup missing thread: %v", err)
	}
	if foundMissing {
		t.Fatal("expected missing thread lookup to return found=false")
	}
}

// TestGetThreadItemScopesLookupToOwnerThread pins the thread isolation
// contract of GetThreadItem(threadID, id). Triage upserts items keyed by
// (thread_id, id), and the composite PK on items enforces that two
// threads can independently hold rows with the same id (e.g.
// `text:0:0`). GetThreadItem must respect that scoping — asking thread B
// for an id that lives only on thread A must return found=false, not
// silently return thread A's row.
func TestGetThreadItemScopesLookupToOwnerThread(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	for _, threadID := range []string{"t-a", "t-b"} {
		if err := s.CreateThread(Thread{
			ID: threadID, ProjectID: defaultTestProjectID, Title: threadID, Provider: "claude", WorkspacePath: "/tmp",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create thread %s: %v", threadID, err)
		}
	}
	if err := s.InsertItem(Item{
		ID: "shared-id", ThreadID: "t-a", TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant", Summary: "owner a",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert owner-a row: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "shared-id", ThreadID: "t-b", TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant", Summary: "owner b",
		CreatedAt: now + 1, UpdatedAt: now + 1,
	}); err != nil {
		t.Fatalf("insert owner-b row: %v", err)
	}

	gotA, foundA, err := s.GetThreadItem("t-a", "shared-id")
	if err != nil {
		t.Fatalf("lookup t-a: %v", err)
	}
	if !foundA {
		t.Fatal("expected lookup on owning thread to succeed")
	}
	if gotA.Summary != "owner a" {
		t.Fatalf("t-a lookup returned summary %q, want %q", gotA.Summary, "owner a")
	}

	gotB, foundB, err := s.GetThreadItem("t-b", "shared-id")
	if err != nil {
		t.Fatalf("lookup t-b: %v", err)
	}
	if !foundB {
		t.Fatal("expected lookup on second owning thread to succeed")
	}
	if gotB.Summary != "owner b" {
		t.Fatalf("t-b lookup returned summary %q, want %q", gotB.Summary, "owner b")
	}

	// A thread that has no row for shared-id must report not-found,
	// not leak either owner's row.
	if err := s.CreateThread(Thread{
		ID: "t-other", ProjectID: defaultTestProjectID, Title: "t-other", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create t-other: %v", err)
	}
	if _, found, err := s.GetThreadItem("t-other", "shared-id"); err != nil {
		t.Fatalf("lookup t-other: %v", err)
	} else if found {
		t.Fatal("expected lookup on unrelated thread to return found=false")
	}
}

// TestUpsertItemWithInputPayloadRoundTrip pins v44's two-payload
// upsert: launch persists (item, nil result, input payload), the
// item's input_payload_id ends up linked, and the data blob is
// recoverable via GetPayloadData.
//
// The follow-up "completion merge" upsert reuses the same launch row
// with a nil input payload and an empty InputPayloadID on the in-memory
// struct — the COALESCE+NULLIF dance in updateExistingItem must
// preserve the launch's payload reference rather than nulling it out.
// Without that contract, every Edit completion would orphan its launch
// payload row.
func TestUpsertItemWithInputPayloadRoundTrip(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	if err := s.CreateThread(Thread{
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	launch := Item{
		ID: "edit-1", ThreadID: "t", TurnIndex: 0,
		Kind: "tool_call", Role: "assistant", Status: "running",
		Summary: "Edit foo.go", ToolName: "Edit",
		Meta:      `{"toolName":"Edit"}`,
		CreatedAt: now, UpdatedAt: now,
	}
	inputPayload := &Payload{
		ID: "p-edit-input", Kind: "tool_call_input", Meta: `{"toolName":"Edit","total":42}`,
		Data: []byte(`{"old_string":"a","new_string":"b"}`), CreatedAt: now,
	}

	persistedLaunch, err := s.UpsertItemWithInputPayload(launch, nil, inputPayload)
	if err != nil {
		t.Fatalf("launch upsert: %v", err)
	}
	if persistedLaunch.InputPayloadID != "p-edit-input" {
		t.Errorf("launch InputPayloadID = %q, want p-edit-input", persistedLaunch.InputPayloadID)
	}
	if persistedLaunch.PayloadID != "" {
		t.Errorf("launch PayloadID = %q, want empty (no result payload)", persistedLaunch.PayloadID)
	}

	// Completion merge: caller passes the launch row again with the
	// same item id but an empty in-memory InputPayloadID — the row's
	// existing column value must be preserved.
	completion := persistedLaunch
	completion.InputPayloadID = ""
	completion.Status = "completed"
	completion.Summary = "Edit foo.go (done)"
	completion.UpdatedAt = now + 1

	persistedCompletion, err := s.UpsertItemWithInputPayload(completion, nil, nil)
	if err != nil {
		t.Fatalf("completion upsert: %v", err)
	}
	if persistedCompletion.InputPayloadID != "p-edit-input" {
		t.Errorf("completion clobbered InputPayloadID: got %q, want p-edit-input", persistedCompletion.InputPayloadID)
	}
	if persistedCompletion.Status != "completed" {
		t.Errorf("completion status = %q, want completed", persistedCompletion.Status)
	}

	// Payload bytes survive the second upsert — the input payload row
	// is not rewritten because the second call passed nil.
	data, err := s.GetPayloadData("t", "p-edit-input")
	if err != nil {
		t.Fatalf("GetPayloadData: %v", err)
	}
	if string(data) != `{"old_string":"a","new_string":"b"}` {
		t.Errorf("payload data round-trip mismatch: got %q", string(data))
	}
}

// TestGetThreadItemByPayloadIDResolvesInputPayloadID pins the v44
// behaviour that GetThreadItemByPayloadID resolves a payload id from
// either the legacy items.payload_id slot OR the new
// items.input_payload_id slot. Without this, the frontend's
// GetPayloadData lazy-load on a tool_call_input payload would fail the
// thread-scoping authz check (`payload not linked to thread`).
func TestGetThreadItemByPayloadIDResolvesInputPayloadID(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	if err := s.CreateThread(Thread{
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := seedPayloadRow(s, "t", Payload{
		ID: "p-input-1", Kind: "tool_call_input", Meta: "{}",
		Data: []byte(`{"old_string":"a","new_string":"b"}`), CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert input payload: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "edit-1", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", Summary: "Edit",
		ToolName: "Edit", InputPayloadID: "p-input-1",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert item: %v", err)
	}

	got, found, err := s.GetThreadItemByPayloadID("t", "p-input-1")
	if err != nil {
		t.Fatalf("lookup by input payload id: %v", err)
	}
	if !found {
		t.Fatal("lookup should resolve the item via input_payload_id")
	}
	if got.ID != "edit-1" {
		t.Errorf("resolved item id = %q, want edit-1", got.ID)
	}
	if got.InputPayloadID != "p-input-1" {
		t.Errorf("scanned input_payload_id = %q, want p-input-1", got.InputPayloadID)
	}

	// Cross-thread isolation still holds for the input slot.
	if err := s.CreateThread(Thread{
		ID: "t2", ProjectID: defaultTestProjectID, Title: "T2", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread t2: %v", err)
	}
	_, foundOther, err := s.GetThreadItemByPayloadID("t2", "p-input-1")
	if err != nil {
		t.Fatalf("lookup other thread: %v", err)
	}
	if foundOther {
		t.Fatal("input payload should not be visible from a non-owning thread")
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
	// Seed (should NOT match): background launch that has a completion
	// sibling. Background launches stay status=running by design, but the
	// sibling is the settled-state marker.
	if _, err := s.AppendItem(Item{
		ID: "skip-settled-bg", ThreadID: "t-reconcile", TurnIndex: 1,
		Kind: "tool_call", Role: "assistant", Status: "running",
		IsBackground: true, Summary: "Bash: settled", ToolName: "Bash",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed skip-settled-bg: %v", err)
	}
	if _, err := s.AppendItem(Item{
		ID: "skip-settled-bg-complete", ThreadID: "t-reconcile", TurnIndex: 2,
		Kind: "tool_completion", Role: "assistant", Status: "completed",
		IsBackground: true, CompletionOf: "skip-settled-bg",
		Summary: "Bash: settled complete", ToolName: "Bash",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed skip-settled-bg-complete: %v", err)
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

func TestListRecoverableClaudeBackgroundLaunchesFiltersToRecoverableRows(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)

	for _, thread := range []Thread{
		{ID: "t-claude", ProjectID: defaultTestProjectID, Title: "Claude", Provider: "claude", WorkspacePath: "/tmp", CreatedAt: now, UpdatedAt: now},
		{ID: "t-claudetui", ProjectID: defaultTestProjectID, Title: "Claude TUI", Provider: "claude-tui", WorkspacePath: "/tmp", CreatedAt: now, UpdatedAt: now},
		{ID: "t-codex", ProjectID: defaultTestProjectID, Title: "Codex", Provider: "codex", WorkspacePath: "/tmp", CreatedAt: now, UpdatedAt: now},
	} {
		if err := s.CreateThread(thread); err != nil {
			t.Fatalf("create thread %s: %v", thread.ID, err)
		}
	}

	seed := func(item Item) {
		item.TurnIndex = 1
		item.Kind = "tool_call"
		item.Role = "assistant"
		item.Status = "running"
		item.IsBackground = true
		item.Summary = item.ID
		if item.ToolName == "" {
			item.ToolName = "Bash"
		}
		item.CreatedAt = now
		item.UpdatedAt = now
		if _, err := s.AppendItem(item); err != nil {
			t.Fatalf("seed %s: %v", item.ID, err)
		}
	}

	// Recoverable: headless + interactive, with and without a task_id. The
	// completion sibling is keyed off the launch id, so a missing task_id
	// is fine — and claude-tui launches NEVER carry one (no task_started
	// reconstruction), so requiring it is exactly what hid them from
	// startup recovery and left them "running" forever after a restart.
	seed(Item{ID: "recover-claude-taskid", ThreadID: "t-claude", Meta: `{"task_id":"task-a"}`})
	seed(Item{ID: "recover-claude-no-taskid", ThreadID: "t-claude", Meta: `{}`})
	seed(Item{ID: "recover-claudetui-taskid", ThreadID: "t-claudetui", Meta: `{"task_id":"task-b"}`})
	seed(Item{ID: "recover-claudetui-no-taskid", ThreadID: "t-claudetui", Meta: `{}`})

	// Skipped: inactive (Codex-owned flag), Codex provider, already paired
	// with a completion sibling.
	seed(Item{ID: "skip-inactive", ThreadID: "t-claude", Meta: `{"task_id":"task-inactive","live_background_active":false}`})
	seed(Item{ID: "skip-codex", ThreadID: "t-codex", ToolName: "collab_agent", Meta: `{"task_id":"task-codex"}`})
	seed(Item{ID: "skip-completed-sibling", ThreadID: "t-claudetui", Meta: `{}`})

	if _, err := s.AppendItem(Item{
		ID: "skip-completed-sibling-done", ThreadID: "t-claudetui", TurnIndex: 2,
		Kind: "tool_completion", Role: "assistant", Status: "completed",
		IsBackground: true, CompletionOf: "skip-completed-sibling",
		Summary: "done", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed completion sibling: %v", err)
	}

	// Skipped: bypass the seed defaults to exercise the negative side of
	// the status='running' and is_background=1 predicates. A completed
	// background launch and a still-running non-background tool_call must
	// both be excluded — otherwise a future edit dropping either predicate
	// would slip through unnoticed.
	for _, item := range []Item{
		{ID: "skip-not-running", ThreadID: "t-claude", TurnIndex: 1, Kind: "tool_call", Role: "assistant", Status: "completed", IsBackground: true, ToolName: "Bash", Summary: "skip-not-running", Meta: `{"task_id":"task-done"}`, CreatedAt: now, UpdatedAt: now},
		{ID: "skip-not-background", ThreadID: "t-claude", TurnIndex: 1, Kind: "tool_call", Role: "assistant", Status: "running", IsBackground: false, ToolName: "Bash", Summary: "skip-not-background", Meta: `{"task_id":"task-fg"}`, CreatedAt: now, UpdatedAt: now},
	} {
		if _, err := s.AppendItem(item); err != nil {
			t.Fatalf("seed %s: %v", item.ID, err)
		}
	}

	got, err := s.ListRecoverableClaudeBackgroundLaunches()
	if err != nil {
		t.Fatalf("ListRecoverableClaudeBackgroundLaunches: %v", err)
	}

	gotIDs := make(map[string]bool, len(got))
	for _, it := range got {
		gotIDs[it.ID] = true
	}
	want := []string{
		"recover-claude-taskid",
		"recover-claude-no-taskid",
		"recover-claudetui-taskid",
		"recover-claudetui-no-taskid",
	}
	for _, id := range want {
		if !gotIDs[id] {
			t.Errorf("expected %q to be recoverable, missing from result", id)
		}
	}
	for _, id := range []string{"skip-inactive", "skip-codex", "skip-completed-sibling", "skip-not-running", "skip-not-background"} {
		if gotIDs[id] {
			t.Errorf("expected %q to be excluded, but it was returned", id)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(got), len(want), got)
	}
}

// TestListTurnItemsSansPayloadSkipsPayloadJoin verifies the narrow
// sibling of ListTurnItems returns items with PayloadKind / PayloadMeta
// left empty even when payload rows exist — the caller explicitly
// opted out of the JOIN. Status/summary/is_background/kind are all
// hydrated so the force-close path has what it needs.
func TestListTurnItemsSansPayloadSkipsPayloadJoin(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	if err := s.CreateThread(Thread{
		ID: "t-sp", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	payload := Payload{ID: "pl-1", Kind: "tool_call_result", Meta: `{"exitCode":0}`, Data: []byte("done"), CreatedAt: now}
	if _, err := s.UpsertItem(Item{
		ID: "it-with-payload", ThreadID: "t-sp", TurnIndex: 0, Kind: "tool_call",
		Role: "assistant", Status: "completed", Summary: "echo",
		PayloadID: "pl-1", CreatedAt: now, UpdatedAt: now,
	}, &payload); err != nil {
		t.Fatalf("upsert item with payload: %v", err)
	}

	bare, err := s.ListTurnItemsSansPayload("t-sp", 0)
	if err != nil {
		t.Fatalf("ListTurnItemsSansPayload: %v", err)
	}
	if len(bare) != 1 {
		t.Fatalf("len=%d, want 1", len(bare))
	}
	if bare[0].PayloadID != "pl-1" {
		t.Errorf("PayloadID = %q, want pl-1 (items column survives)", bare[0].PayloadID)
	}
	if bare[0].PayloadKind != "" {
		t.Errorf("PayloadKind = %q, want empty (JOIN skipped)", bare[0].PayloadKind)
	}
	if bare[0].PayloadMeta != "" {
		t.Errorf("PayloadMeta = %q, want empty (JOIN skipped)", bare[0].PayloadMeta)
	}
	if bare[0].Status != "completed" {
		t.Errorf("Status = %q, want completed", bare[0].Status)
	}

	// Confirm the full-JOIN sibling still hydrates payload metadata —
	// other callers rely on it.
	full, err := s.ListTurnItems("t-sp", 0)
	if err != nil {
		t.Fatalf("ListTurnItems: %v", err)
	}
	if full[0].PayloadKind != "tool_call_result" {
		t.Errorf("full.PayloadKind = %q, want tool_call_result (sibling still joins)", full[0].PayloadKind)
	}
}

func TestHasMatchingSystemItemScopesExactErrorRow(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	if err := s.CreateThread(Thread{
		ID: "t-match", ProjectID: defaultTestProjectID, Title: "T", Provider: "codex", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	if _, err := s.AppendItem(Item{
		ID: "error-1", ThreadID: "t-match", TurnIndex: 0, Kind: "error",
		Role: "system", Status: "completed", Summary: "same failure",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("append error item: %v", err)
	}

	found, err := s.HasMatchingSystemItem("t-match", 0, "error", "", "same failure")
	if err != nil {
		t.Fatalf("HasMatchingSystemItem exact: %v", err)
	}
	if !found {
		t.Fatal("expected exact system error match")
	}

	found, err = s.HasMatchingSystemItem("t-match", 0, "api_error", "", "same failure")
	if err != nil {
		t.Fatalf("HasMatchingSystemItem wrong kind: %v", err)
	}
	if found {
		t.Fatal("wrong kind matched")
	}
}

// TestForceCloseRunningToolCallsInTurnFlipsOnlyOrphanInlineTools pins
// the three-way filter in the new accessor: only rows that are
// (1) kind=tool_call, (2) status=running, (3) is_background=0 flip. A
// completed inline tool stays completed; a running bg tool stays
// running (invariant 24); a non-tool_call running row (streaming
// assistant_text) stays untouched.
func TestForceCloseRunningToolCallsInTurnFlipsOnlyOrphanInlineTools(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	if err := s.CreateThread(Thread{
		ID: "t-fc", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	if _, err := s.UpsertItem(Item{
		ID: "inline-orphan", ThreadID: "t-fc", TurnIndex: 0, Kind: "tool_call",
		Role: "assistant", Status: "running", Summary: "Bash: sleep 10",
		CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("upsert inline orphan: %v", err)
	}
	if _, err := s.UpsertItem(Item{
		ID: "inline-complete", ThreadID: "t-fc", TurnIndex: 0, Kind: "tool_call",
		Role: "assistant", Status: "completed", Summary: "Bash: true",
		CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("upsert inline complete: %v", err)
	}
	if _, err := s.UpsertItem(Item{
		ID: "bg-running", ThreadID: "t-fc", TurnIndex: 0, Kind: "tool_call",
		Role: "assistant", Status: "running", Summary: "Bash: long-running",
		IsBackground: true, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("upsert bg running: %v", err)
	}
	if _, err := s.UpsertItem(Item{
		ID: "text-streaming", ThreadID: "t-fc", TurnIndex: 0, Kind: "assistant_text",
		Role: "assistant", Status: "streaming", Summary: "thinking...",
		CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("upsert streaming text: %v", err)
	}

	updatedAt := now + 100
	summariser := func(prior string) string { return strings.TrimSpace(prior) + " — unresolved" }
	flipped, err := s.ForceCloseRunningToolCallsInTurn("t-fc", 0, summariser, updatedAt)
	if err != nil {
		t.Fatalf("ForceCloseRunningToolCallsInTurn: %v", err)
	}
	if len(flipped) != 1 {
		t.Fatalf("flipped=%d, want 1 (inline-orphan only)", len(flipped))
	}
	if flipped[0].ID != "inline-orphan" {
		t.Errorf("flipped[0].ID = %q, want inline-orphan", flipped[0].ID)
	}
	if flipped[0].Status != "errored" {
		t.Errorf("flipped[0].Status = %q, want errored", flipped[0].Status)
	}
	if !strings.HasSuffix(flipped[0].Summary, " — unresolved") {
		t.Errorf("flipped[0].Summary = %q, want trailing ' — unresolved'", flipped[0].Summary)
	}
	if flipped[0].UpdatedAt != updatedAt {
		t.Errorf("flipped[0].UpdatedAt = %d, want %d", flipped[0].UpdatedAt, updatedAt)
	}

	// Cross-check the persisted state matches what the accessor
	// returned.
	orphan, ok, err := s.GetThreadItem("t-fc", "inline-orphan")
	if err != nil || !ok {
		t.Fatalf("get orphan: found=%v err=%v", ok, err)
	}
	if orphan.Status != "errored" {
		t.Errorf("persisted orphan status = %q, want errored", orphan.Status)
	}

	done, ok, err := s.GetThreadItem("t-fc", "inline-complete")
	if err != nil || !ok {
		t.Fatalf("get done: found=%v err=%v", ok, err)
	}
	if done.Status != "completed" {
		t.Errorf("already-completed row flipped: status = %q, want completed", done.Status)
	}

	bg, ok, err := s.GetThreadItem("t-fc", "bg-running")
	if err != nil || !ok {
		t.Fatalf("get bg: found=%v err=%v", ok, err)
	}
	if bg.Status != "running" {
		t.Errorf("bg tool_call flipped despite is_background=1 (invariant 24): status = %q", bg.Status)
	}

	txt, ok, err := s.GetThreadItem("t-fc", "text-streaming")
	if err != nil || !ok {
		t.Fatalf("get text: found=%v err=%v", ok, err)
	}
	if txt.Status != "streaming" {
		t.Errorf("streaming text kind flipped by force-close: status = %q", txt.Status)
	}
}

// TestForceCloseRunningToolCallsInTurnEmptyNoOp confirms the no-rows
// path commits cleanly without a thread-touch write (the caller is
// expected to emit nothing).
func TestForceCloseRunningToolCallsInTurnEmptyNoOp(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	if err := s.CreateThread(Thread{
		ID: "t-noop", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	flipped, err := s.ForceCloseRunningToolCallsInTurn("t-noop", 0, func(s string) string { return s }, now+1)
	if err != nil {
		t.Fatalf("empty force-close: %v", err)
	}
	if flipped != nil {
		t.Errorf("empty force-close returned %+v, want nil", flipped)
	}
}

// TestFlipGhostBackgroundRowsOnStartFlipsOnlyRunningBackgroundToolCalls
// pins the narrow scope of the Phase-4 store-level flip: a row flips
// iff (kind=tool_call, status=running, is_background=1). Anything else
// stays untouched. The filter pushes into SQLite so a thread with deep
// history doesn't pay deserialization cost for rows the flip can't
// touch.
func TestFlipGhostBackgroundRowsOnStartFlipsOnlyRunningBackgroundToolCalls(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	if err := s.CreateThread(Thread{
		ID: "t-ghost", ProjectID: defaultTestProjectID, Title: "T", Provider: "codex", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Running background tool_call — must flip.
	if _, err := s.UpsertItem(Item{
		ID: "ghost-match", ThreadID: "t-ghost", TurnIndex: 0, Kind: "tool_call",
		Role: "assistant", Status: "running", Summary: "Bash: sleep 10",
		IsBackground: true, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("upsert ghost-match: %v", err)
	}
	// Completed background tool_call — must NOT flip.
	if _, err := s.UpsertItem(Item{
		ID: "ghost-done", ThreadID: "t-ghost", TurnIndex: 0, Kind: "tool_call",
		Role: "assistant", Status: "completed", Summary: "Bash: echo hi",
		IsBackground: true, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("upsert ghost-done: %v", err)
	}
	// Running inline (non-background) tool_call — must NOT flip.
	if _, err := s.UpsertItem(Item{
		ID: "ghost-inline", ThreadID: "t-ghost", TurnIndex: 0, Kind: "tool_call",
		Role: "assistant", Status: "running", Summary: "Read: /tmp/x",
		IsBackground: false, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("upsert ghost-inline: %v", err)
	}
	// Streaming (assistant_text, not tool_call) — must NOT flip even if
	// someone set is_background=true (no production caller does this; the
	// filter is intentionally defensive).
	if _, err := s.UpsertItem(Item{
		ID: "ghost-text", ThreadID: "t-ghost", TurnIndex: 0, Kind: "assistant_text",
		Role: "assistant", Status: "running", Summary: "thinking...",
		IsBackground: true, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("upsert ghost-text: %v", err)
	}

	updatedAt := now + 100
	summariser := func(prior string) string {
		return strings.TrimSpace(prior) + " — session ended"
	}
	flipped, err := s.FlipGhostBackgroundRowsOnStart("t-ghost", summariser, updatedAt)
	if err != nil {
		t.Fatalf("FlipGhostBackgroundRowsOnStart: %v", err)
	}
	if len(flipped) != 1 {
		t.Fatalf("flipped=%d, want 1 (ghost-match only)", len(flipped))
	}
	if flipped[0].ID != "ghost-match" {
		t.Errorf("flipped[0].ID = %q, want ghost-match", flipped[0].ID)
	}
	if flipped[0].Status != "errored" {
		t.Errorf("flipped[0].Status = %q, want errored", flipped[0].Status)
	}
	if flipped[0].Decision != "lost" {
		t.Errorf("flipped[0].Decision = %q, want lost", flipped[0].Decision)
	}
	if !strings.HasSuffix(flipped[0].Summary, " — session ended") {
		t.Errorf("flipped[0].Summary = %q, want trailing ' — session ended'", flipped[0].Summary)
	}
	if flipped[0].UpdatedAt != updatedAt {
		t.Errorf("flipped[0].UpdatedAt = %d, want %d", flipped[0].UpdatedAt, updatedAt)
	}

	// Cross-check persisted state matches the returned rows.
	match, ok, err := s.GetThreadItem("t-ghost", "ghost-match")
	if err != nil || !ok {
		t.Fatalf("get ghost-match: found=%v err=%v", ok, err)
	}
	if match.Status != "errored" || match.Decision != "lost" {
		t.Errorf("ghost-match persisted state wrong: status=%q decision=%q", match.Status, match.Decision)
	}

	// Everything else is unchanged.
	for _, id := range []string{"ghost-done", "ghost-inline", "ghost-text"} {
		it, ok, err := s.GetThreadItem("t-ghost", id)
		if err != nil || !ok {
			t.Fatalf("get %s: found=%v err=%v", id, ok, err)
		}
		if id == "ghost-done" && it.Status != "completed" {
			t.Errorf("%s persisted state wrong: status=%q, want completed", id, it.Status)
		}
		if id == "ghost-inline" && it.Status != "running" {
			t.Errorf("%s persisted state wrong: status=%q, want running", id, it.Status)
		}
		if id == "ghost-text" && it.Status != "running" {
			t.Errorf("%s persisted state wrong: status=%q, want running", id, it.Status)
		}
		if it.Decision != "" {
			t.Errorf("%s got decision %q without a flip", id, it.Decision)
		}
	}
}

// TestFlipGhostBackgroundRowsOnStartEmptyThreadNoOp pins the zero-
// ghost-rows fast path: the TX commits cleanly, no thread-touch
// runs, no rows returned.
func TestFlipGhostBackgroundRowsOnStartEmptyThreadNoOp(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	if err := s.CreateThread(Thread{
		ID: "t-ghost-empty", ProjectID: defaultTestProjectID, Title: "T", Provider: "codex", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Verify threads.updated_at did not bump.
	before, err := s.GetThread("t-ghost-empty")
	if err != nil {
		t.Fatalf("GetThread before: %v", err)
	}
	flipped, err := s.FlipGhostBackgroundRowsOnStart("t-ghost-empty", func(s string) string { return s }, now+1)
	if err != nil {
		t.Fatalf("empty ghost-flip: %v", err)
	}
	if flipped != nil {
		t.Errorf("empty ghost-flip returned %+v, want nil", flipped)
	}
	after, err := s.GetThread("t-ghost-empty")
	if err != nil {
		t.Fatalf("GetThread after: %v", err)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Errorf("empty ghost-flip bumped threads.updated_at: before=%d after=%d", before.UpdatedAt, after.UpdatedAt)
	}
}

// TestFlipGhostBackgroundRowsOnStartScopedPerThread pins cross-thread
// isolation: a flip on thread A must not touch thread B, even when B
// has an identical running+background row. Matters for the restart
// case where multiple Codex threads share the same store.
func TestFlipGhostBackgroundRowsOnStartScopedPerThread(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	for _, id := range []string{"t-ghost-a", "t-ghost-b"} {
		if err := s.CreateThread(Thread{
			ID: id, ProjectID: defaultTestProjectID, Title: "T", Provider: "codex", WorkspacePath: "/tmp",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create thread %s: %v", id, err)
		}
		if _, err := s.UpsertItem(Item{
			ID: id + "-row", ThreadID: id, TurnIndex: 0, Kind: "tool_call",
			Role: "assistant", Status: "running", Summary: "Bash: sleep",
			IsBackground: true, CreatedAt: now, UpdatedAt: now,
		}, nil); err != nil {
			t.Fatalf("upsert %s row: %v", id, err)
		}
	}

	_, err := s.FlipGhostBackgroundRowsOnStart("t-ghost-a",
		func(p string) string { return p + " — session ended" }, now+1)
	if err != nil {
		t.Fatalf("flip a: %v", err)
	}

	aRow, _, err := s.GetThreadItem("t-ghost-a", "t-ghost-a-row")
	if err != nil {
		t.Fatalf("get a row: %v", err)
	}
	if aRow.Status != "errored" {
		t.Errorf("t-ghost-a row status = %q, want errored", aRow.Status)
	}

	bRow, _, err := s.GetThreadItem("t-ghost-b", "t-ghost-b-row")
	if err != nil {
		t.Fatalf("get b row: %v", err)
	}
	if bRow.Status != "running" {
		t.Errorf("t-ghost-b row flipped by A's flip (cross-thread leak): status = %q", bRow.Status)
	}
	if bRow.Decision != "" {
		t.Errorf("t-ghost-b row picked up decision: %q", bRow.Decision)
	}
}

// seedStreamingItemWithPayload creates thread "t" plus a streaming item
// "stream" of itemKind linked to payload "pay" of payloadKind (initial data
// "base"). Shared fixture for the combined summary+payload flush accessors.
func seedStreamingItemWithPayload(t *testing.T, s *Store, itemKind, payloadKind string) {
	t.Helper()
	if err := s.CreateThread(Thread{
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: 1000, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := seedPayloadRow(s, "t", Payload{
		ID: "pay", Kind: payloadKind, Meta: `{"rev":1}`, Data: []byte("base"), CreatedAt: 2000,
	}); err != nil {
		t.Fatalf("insert payload: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "stream", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
		Kind: itemKind, Role: "assistant", Status: "streaming",
		Summary: "head ", PayloadID: "pay",
		CreatedAt: 2000, UpdatedAt: 2000,
	}); err != nil {
		t.Fatalf("insert item: %v", err)
	}
}

// TestAppendItemSummaryAndPayloadDataMatchesSequentialPair pins the
// combined single-transaction flush writer to the exact persisted state and
// return value of the former AppendItemSummary → AppendPayloadData pair.
func TestAppendItemSummaryAndPayloadDataMatchesSequentialPair(t *testing.T) {
	combined := newTestStore(t)
	sequential := newTestStore(t)
	seedStreamingItemWithPayload(t, combined, "thinking", "thinking")
	seedStreamingItemWithPayload(t, sequential, "thinking", "thinking")

	gotCombined, err := combined.AppendItemSummaryAndPayloadData("t", "stream", "world", "pay", []byte(" delta"), 3000)
	if err != nil {
		t.Fatalf("combined append: %v", err)
	}

	gotSequential, err := sequential.AppendItemSummary("t", "stream", "world", 3000)
	if err != nil {
		t.Fatalf("sequential summary append: %v", err)
	}
	if err := sequential.AppendPayloadData("t", "pay", []byte(" delta"), gotSequential.PayloadMeta, 3000); err != nil {
		t.Fatalf("sequential payload append: %v", err)
	}

	if gotCombined != gotSequential {
		t.Errorf("returned item diverged:\ncombined  = %#v\nsequential = %#v", gotCombined, gotSequential)
	}
	for name, s := range map[string]*Store{"combined": combined, "sequential": sequential} {
		data, err := s.GetPayloadData("t", "pay")
		if err != nil {
			t.Fatalf("%s payload data: %v", name, err)
		}
		if string(data) != "base delta" {
			t.Errorf("%s payload data = %q, want %q", name, data, "base delta")
		}
		meta, err := s.GetPayloadMeta("t", "pay")
		if err != nil {
			t.Fatalf("%s payload meta: %v", name, err)
		}
		if meta.Meta != `{"rev":1}` || meta.CreatedAt != 3000 {
			t.Errorf("%s payload meta = %#v, want meta {\"rev\":1} createdAt 3000", name, meta)
		}
	}
}

// TestAppendItemSummaryAndPayloadDataRollsBackOnPayloadFailure pins the
// combined transaction's atomicity: when the payload half fails (payload row
// missing), the summary half must roll back too — the row keeps its prior
// summary and updated_at.
func TestAppendItemSummaryAndPayloadDataRollsBackOnPayloadFailure(t *testing.T) {
	s := newTestStore(t)
	seedStreamingItemWithPayload(t, s, "assistant_text", "assistant_text")

	_, err := s.AppendItemSummaryAndPayloadData("t", "stream", "world", "missing-pay", []byte("x"), 3000)
	if err == nil {
		t.Fatal("expected error for missing payload row")
	}

	got, ok, err := s.GetThreadItem("t", "stream")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if !ok {
		t.Fatal("item missing")
	}
	if got.Summary != "head " {
		t.Errorf("summary = %q after failed combined append, want %q (rolled back)", got.Summary, "head ")
	}
	if got.UpdatedAt != 2000 {
		t.Errorf("updatedAt = %d after failed combined append, want 2000 (rolled back)", got.UpdatedAt)
	}
}

// TestAppendItemSummaryTailAndPayloadDataMatchesSequentialPair is the
// tail-bounded sibling: summary keeps only the last maxRunes characters
// while the payload chunk lands in the same transaction.
func TestAppendItemSummaryTailAndPayloadDataMatchesSequentialPair(t *testing.T) {
	combined := newTestStore(t)
	sequential := newTestStore(t)
	seedStreamingItemWithPayload(t, combined, "thinking", "thinking")
	seedStreamingItemWithPayload(t, sequential, "thinking", "thinking")

	const maxRunes = 8
	gotCombined, err := combined.AppendItemSummaryTailAndPayloadData("t", "stream", "world", maxRunes, "pay", []byte(" delta"), 3000)
	if err != nil {
		t.Fatalf("combined tail append: %v", err)
	}

	gotSequential, err := sequential.AppendItemSummaryTail("t", "stream", "world", maxRunes, 3000)
	if err != nil {
		t.Fatalf("sequential tail append: %v", err)
	}
	if err := sequential.AppendPayloadData("t", "pay", []byte(" delta"), gotSequential.PayloadMeta, 3000); err != nil {
		t.Fatalf("sequential payload append: %v", err)
	}

	if gotCombined != gotSequential {
		t.Errorf("returned item diverged:\ncombined  = %#v\nsequential = %#v", gotCombined, gotSequential)
	}
	// "head " + "world" = "head world" (10 runes) tail-bounded to 8.
	if gotCombined.Summary != "ad world" {
		t.Errorf("tail summary = %q, want %q", gotCombined.Summary, "ad world")
	}
	data, err := combined.GetPayloadData("t", "pay")
	if err != nil {
		t.Fatalf("payload data: %v", err)
	}
	if string(data) != "base delta" {
		t.Errorf("payload data = %q, want %q", data, "base delta")
	}
}

// TestUpsertItemWithPayloadAppendMatchesSequentialPair pins the streaming
// command-output flush writer: one transaction must produce the same
// persisted state and return value as the former AppendPayloadData →
// UpsertItem pair.
func TestUpsertItemWithPayloadAppendMatchesSequentialPair(t *testing.T) {
	combined := newTestStore(t)
	sequential := newTestStore(t)
	seedStreamingItemWithPayload(t, combined, "tool_call", "command_output")
	seedStreamingItemWithPayload(t, sequential, "tool_call", "command_output")

	update := Item{
		ID: "stream", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", Status: "streaming",
		Summary: "head ", PayloadID: "pay",
		CreatedAt: 2000, UpdatedAt: 3000,
	}

	gotCombined, err := combined.UpsertItemWithPayloadAppend(update, "pay", []byte(" delta"), `{"rev":2}`, 3000)
	if err != nil {
		t.Fatalf("combined upsert: %v", err)
	}

	if err := sequential.AppendPayloadData("t", "pay", []byte(" delta"), `{"rev":2}`, 3000); err != nil {
		t.Fatalf("sequential payload append: %v", err)
	}
	gotSequential, err := sequential.UpsertItem(update, nil)
	if err != nil {
		t.Fatalf("sequential upsert: %v", err)
	}

	if gotCombined != gotSequential {
		t.Errorf("returned item diverged:\ncombined  = %#v\nsequential = %#v", gotCombined, gotSequential)
	}
	for name, s := range map[string]*Store{"combined": combined, "sequential": sequential} {
		data, err := s.GetPayloadData("t", "pay")
		if err != nil {
			t.Fatalf("%s payload data: %v", name, err)
		}
		if string(data) != "base delta" {
			t.Errorf("%s payload data = %q, want %q", name, data, "base delta")
		}
		meta, err := s.GetPayloadMeta("t", "pay")
		if err != nil {
			t.Fatalf("%s payload meta: %v", name, err)
		}
		if meta.Meta != `{"rev":2}` || meta.CreatedAt != 3000 {
			t.Errorf("%s payload meta = %#v, want meta {\"rev\":2} createdAt 3000", name, meta)
		}
	}
}

// TestUpsertItemWithPayloadAppendRollsBackOnMissingPayload proves the item
// half of the combined write never lands when the payload append fails.
func TestUpsertItemWithPayloadAppendRollsBackOnMissingPayload(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(Thread{
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: 1000, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	item := Item{
		ID: "cmd", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", Status: "streaming",
		Summary: "$ ls", CreatedAt: 2000, UpdatedAt: 2000,
	}
	_, err := s.UpsertItemWithPayloadAppend(item, "missing-pay", []byte("x"), `{}`, 2000)
	if err == nil {
		t.Fatal("expected error for missing payload row")
	}

	_, ok, err := s.GetThreadItem("t", "cmd")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if ok {
		t.Fatal("item row landed despite payload append failure (no rollback)")
	}
}

func TestListIncompleteCodexSubagentOwnershipsIsCompactOrderedAndUnresolved(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "codex")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := s.CreateThread(makeThread("other", "codex")); err != nil {
		t.Fatalf("create other thread: %v", err)
	}
	seed := func(item Item) {
		t.Helper()
		item.Kind = "tool_call"
		item.Role = "assistant"
		item.Status = "completed"
		item.ToolName = "collab_agent"
		item.CreatedAt = 1000
		item.UpdatedAt = 1000
		if err := s.InsertItem(item); err != nil {
			t.Fatalf("seed %s: %v", item.ID, err)
		}
	}

	seed(Item{ID: "spawn-later", ThreadID: "t", TurnIndex: 1, ItemIndex: 0, IsBackground: true,
		Meta: `{"input":{"tool":"spawn_agent","receiverThreadIds":["child-later"]}}`})
	seed(Item{ID: "spawn-first", ThreadID: "t", TurnIndex: 0, ItemIndex: 3, IsBackground: true,
		Meta: `{"input":{"tool":"spawnAgent","receiverThreadIds":["child-first"]}}`})
	seed(Item{ID: "settled", ThreadID: "t", TurnIndex: 0, ItemIndex: 4, IsBackground: true,
		Meta: `{"input":{"tool":"spawn_agent","receiverThreadIds":["child-settled"]}}`})
	seed(Item{ID: "send-input", ThreadID: "t", TurnIndex: 0, ItemIndex: 5, IsBackground: true,
		Meta: `{"input":{"tool":"send_input","receiverThreadIds":["child-first"]}}`})
	seed(Item{ID: "foreground", ThreadID: "t", TurnIndex: 0, ItemIndex: 6,
		Meta: `{"input":{"tool":"spawn_agent","receiverThreadIds":["child-foreground"]}}`})
	seed(Item{ID: "other-thread", ThreadID: "other", TurnIndex: 0, ItemIndex: 0, IsBackground: true,
		Meta: `{"input":{"tool":"spawn_agent","receiverThreadIds":["child-other"]}}`})
	if err := s.InsertItem(Item{
		ID:           "settled-answer",
		ThreadID:     "t",
		TurnIndex:    0,
		ItemIndex:    7,
		Kind:         "tool_completion",
		Role:         "assistant",
		Status:       "completed",
		CompletionOf: "settled",
		CreatedAt:    1001,
		UpdatedAt:    1001,
	}); err != nil {
		t.Fatalf("seed settled answer: %v", err)
	}

	got, err := s.ListIncompleteCodexSubagentOwnerships("t")
	if err != nil {
		t.Fatalf("list ownerships: %v", err)
	}
	if len(got) != 2 || got[0].ItemID != "spawn-first" || got[1].ItemID != "spawn-later" {
		t.Fatalf("ownerships = %+v, want unresolved launches in transcript order", got)
	}
	if got[0].Meta != `{"input":{"tool":"spawnAgent","receiverThreadIds":["child-first"]}}` {
		t.Fatalf("first metadata = %s", got[0].Meta)
	}
}

// TestCompletionSiblingProbesUseIndex is the guard behind the named SQL
// fragments in items_lifecycle.go. The completion-sibling probe carries a
// semantically redundant `c.completion_of <> ”` term for one reason: SQLite
// uses a partial index only when the query's predicates textually imply the
// index's WHERE clause, and the correlated `c.completion_of = items.id` does
// not. Without the term every probe here degrades to scanning the thread's
// whole items slice — a silent regression, correct in every result it returns
// and seconds slower per call on a large thread.
//
// So the plan is asserted rather than the SQL. Each case is one of the outer
// query SHAPES items_lifecycle.go splices the fragment into (a bare EXISTS, an
// INDEXED BY count, a JOIN through threads, a payload LEFT JOIN, an UPDATE,
// and the one unscoped boot query), built from the same consts the shipped
// accessors use. TestCompletionSiblingProbesAreNotSpelledInline is the other
// half: it is what keeps a new call site from bypassing the const and
// therefore this test.
func TestCompletionSiblingProbesUseIndex(t *testing.T) {
	s := newTestStore(t)

	cases := []struct {
		name  string
		query string
		args  []any
	}{
		{
			// HasLiveBackgroundToolCall / HasQueueBlockingBackgroundToolCall.
			name: "thread-scoped EXISTS",
			query: `SELECT EXISTS(
			    SELECT 1 FROM items
			     WHERE thread_id = ?
			       AND ` + liveBackgroundLaunchSQL + `
			       AND ` + noCompletionSiblingSQL + `
			     LIMIT 1
			)`,
			args: []any{"thread-plan"},
		},
		{
			// CountLiveRunningBackgroundToolCalls.
			name: "INDEXED BY count",
			query: `SELECT COUNT(*)
			   FROM items INDEXED BY idx_items_live_background
			  WHERE thread_id = ?
			    AND ` + liveBackgroundLaunchSQL + `
			    AND ` + noCompletionSiblingIndexedSQL,
			args: []any{"thread-plan"},
		},
		{
			// HasLiveCodexSubagentLaunch / CountLiveCodexSubagentLaunches.
			name: "provider join",
			query: `SELECT EXISTS(
			    SELECT 1 FROM items
			    JOIN threads ON threads.id = items.thread_id
			     WHERE items.thread_id = ?
			       AND threads.provider = 'codex'
			       AND items.kind = 'tool_call'
			       AND items.status = 'completed'
			       AND items.tool_name = 'collab_agent'
			       AND items.is_background = 1
			       AND ` + noCompletionSiblingSQL + `
			     LIMIT 1
			)`,
			args: []any{"thread-plan"},
		},
		{
			// ListRunningBackgroundToolCalls / ListIncompleteCodexSubagentLaunches /
			// GetIncompleteCodexSubagentLaunch.
			name: "payload left join list",
			query: `SELECT ` + itemColumns + `
			   FROM items
			   LEFT JOIN payloads ON payloads.thread_id = items.thread_id AND payloads.id = items.payload_id
			  WHERE items.thread_id = ?
			    AND items.kind = 'tool_call'
			    AND items.status = 'running'
			    AND items.is_background = 1
			    AND ` + noCompletionSiblingSQL + `
			  ORDER BY items.turn_index, items.item_index`,
			args: []any{"thread-plan"},
		},
		{
			// ListIncompleteCodexSubagentOwnerships.
			name: "compact ownership list",
			query: `SELECT items.id, items.meta
			   FROM items
			  WHERE items.thread_id = ?
			    AND items.kind = 'tool_call'
			    AND items.tool_name = 'collab_agent'
			    AND items.is_background = 1
			    AND json_extract(items.meta, '$.input.tool') IN ('spawn_agent', 'spawnAgent')
			    AND ` + noCompletionSiblingSQL + `
			  ORDER BY items.turn_index, items.item_index`,
			args: []any{"thread-plan"},
		},
		{
			// MarkLiveBackgroundToolCallsInactive / MarkLiveCodexSubagentLaunchesInactive.
			name: "update",
			query: `UPDATE items
			    SET updated_at = ?
			  WHERE thread_id = ?
			    AND ` + liveBackgroundLaunchSQL + `
			    AND ` + noCompletionSiblingSQL,
			args: []any{int64(1), "thread-plan"},
		},
		{
			// ListRecoverableClaudeBackgroundLaunches — the one query in the
			// file with no thread scope, so its plan is the one that matters
			// most at multi-GB history sizes.
			name: "unscoped boot sweep",
			query: `SELECT ` + itemColumns + `
			   FROM items
			   JOIN threads ON threads.id = items.thread_id
			   LEFT JOIN payloads ON payloads.thread_id = items.thread_id AND payloads.id = items.payload_id
			  WHERE threads.provider IN ('claude', 'claude-tui')
			    AND items.kind = 'tool_call'
			    AND items.status = 'running'
			    AND items.is_background = 1
			    AND ` + noCompletionSiblingSQL,
		},
		{
			// ListLiveCodexSubagentLaunches, where the probe sits inside an OR.
			name: "probe inside a disjunction",
			query: `SELECT ` + itemColumns + `
			   FROM items
			   JOIN threads ON threads.id = items.thread_id
			   LEFT JOIN payloads ON payloads.thread_id = items.thread_id AND payloads.id = items.payload_id
			  WHERE items.thread_id = ?
			    AND threads.provider = 'codex'
			    AND items.kind = 'tool_call'
			    AND items.status = 'completed'
			    AND items.tool_name = 'collab_agent'
			    AND items.is_background = 1
			    AND (
			      ` + noCompletionSiblingSQL + `
			      OR json_extract(items.meta, '$.live_background_active') = 1
			    )
			  ORDER BY items.turn_index, items.item_index`,
			args: []any{"thread-plan"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertPlanUses(t, s.db, "idx_items_completion_of",
				"EXPLAIN QUERY PLAN "+tc.query, tc.args...)
		})
	}
}

// TestCompletionSiblingProbesAreNotSpelledInline keeps every completion-sibling
// probe in items_lifecycle.go routed through the named consts, which is what
// makes the plan test above a guard over the whole file rather than over seven
// strings that happen to live in a test. A new probe written out by hand would
// otherwise be unmeasured — and, because the redundant planner term is easy to
// read as noise, is exactly the one somebody would write without it.
func TestCompletionSiblingProbesAreNotSpelledInline(t *testing.T) {
	source, err := os.ReadFile("items_lifecycle.go")
	if err != nil {
		t.Fatalf("read items_lifecycle.go: %v", err)
	}
	// The two const declarations are the only place the probe body may appear.
	const body = "SELECT 1 FROM items c"
	got := strings.Count(string(source), body)
	if got != 2 {
		t.Errorf("items_lifecycle.go spells the completion-sibling probe body %d times, want 2 "+
			"(the noCompletionSiblingSQL / noCompletionSiblingIndexedSQL declarations); "+
			"splice the const instead so TestCompletionSiblingProbesUseIndex covers the new call site", got)
	}
	// Same for the live-background launch predicate.
	const launchTerm = "AND is_background = 1\n\t    AND parent_id = ''"
	if got := strings.Count(string(source), launchTerm); got != 1 {
		t.Errorf("items_lifecycle.go spells the live background launch predicate %d times, want 1 "+
			"(the liveBackgroundLaunchSQL declaration)", got)
	}
}
