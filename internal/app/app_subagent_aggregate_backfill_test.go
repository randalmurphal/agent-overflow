package app

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// TestSubagentAggregateBackfillRunsTheListToEmpty drives migration v121's
// deferred phase over threads whose anchors predate the stamps. It stamps
// every listed thread; a thread whose stamp writes never land (its rows
// move under every batch) does not hold up the threads after it; a halt
// joins the loop while it waits on that thread; and the next start
// resumes from the list and ends on its own once the list is empty.
func TestSubagentAggregateBackfillRunsTheListToEmpty(t *testing.T) {
	app, dbPath := newTestAppWithStorePath(t)
	threads := []string{"a-moving", "b-legacy", "c-legacy"}
	for _, id := range threads {
		seedThread(t, app, id, 1)
		for i, row := range []store.Item{
			{ID: "agent", Kind: "tool_call", ToolName: "Agent", Summary: "Agent: work", Status: "running"},
			{ID: "step-1", Kind: "tool_call", ToolName: "Bash", Summary: "Bash: one", Status: "completed", ParentID: "agent"},
			{ID: "step-2", Kind: "tool_call", ToolName: "Bash", Summary: "Bash: two", Status: "completed", ParentID: "agent"},
		} {
			row.ThreadID, row.ItemIndex, row.Role, row.Meta, row.CreatedAt, row.UpdatedAt = id, i, "assistant", "{}", 1, 1
			if err := app.store.InsertItem(row); err != nil {
				t.Fatalf("seed %s/%s: %v", id, row.ID, err)
			}
		}
	}
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open second handle: %v", err)
	}
	t.Cleanup(func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close second handle: %v", err)
		}
	})
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := raw.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	// The state migration v121 leaves an existing thread in: no card on
	// any row, and the thread listed for the deferred phase.
	for _, id := range threads {
		exec(`UPDATE threads SET history_bulk_load = 1 WHERE id = ?`, id)
		exec(`UPDATE items SET meta = json_remove(meta, '$.subagentDescendantCount', '$.subagentLatestChildSummary',
		    '$.subagentTranscriptDescendantCount', '$.subagentLatestToolSummary', '$.subagentLatestToolTurnIndex',
		    '$.subagentLatestToolItemIndex', '$.subagentAggregateState') WHERE thread_id = ?`, id)
		exec(`UPDATE threads SET history_bulk_load = 0 WHERE id = ?`, id)
		exec(`INSERT INTO subagent_aggregate_backfill (thread_id) VALUES (?)`, id)
	}
	exec(`CREATE TRIGGER test_backfill_rows_move BEFORE UPDATE ON items WHEN OLD.thread_id = 'a-moving'
	  BEGIN SELECT RAISE(IGNORE); END`)

	listed := func() []string {
		t.Helper()
		rows, err := raw.Query(`SELECT thread_id FROM subagent_aggregate_backfill ORDER BY thread_id`)
		if err != nil {
			t.Fatalf("read backfill list: %v", err)
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan backfill list: %v", err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate backfill list: %v", err)
		}
		return ids
	}
	waitListed := func(want []string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !slices.Equal(listed(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("backfill list is %v, want %v", listed(), want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	assertStamped := func(id string) {
		t.Helper()
		var meta string
		if err := raw.QueryRow(`SELECT meta FROM items WHERE thread_id = ? AND id = 'agent'`, id).Scan(&meta); err != nil {
			t.Fatalf("read %s's anchor: %v", id, err)
		}
		if !strings.Contains(meta, `"subagentDescendantCount":2`) || !strings.Contains(meta, `"subagentAggregateState"`) {
			t.Fatalf("%s's anchor meta %s, want its stamped card counting two rows", id, meta)
		}
	}
	halt := func() {
		t.Helper()
		done := make(chan struct{})
		go func() {
			app.subagentBackfill.halt()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("halt did not join the backfill loop")
		}
	}

	app.startSubagentAggregateBackfill()
	app.startSubagentAggregateBackfill()
	// Cancelling the app's context first lets the join finish after a
	// failed halt, so the failure reports instead of hanging the package.
	t.Cleanup(func() {
		app.appCancel()
		app.subagentBackfill.halt()
	})
	waitListed([]string{"a-moving"})
	assertStamped("b-legacy")
	assertStamped("c-legacy")
	halt()

	exec(`DROP TRIGGER test_backfill_rows_move`)
	finished := make(chan struct{})
	go func() {
		app.runSubagentAggregateBackfill(context.Background())
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatalf("the backfill did not end on its own; list is %v", listed())
	}
	if ids := listed(); len(ids) != 0 {
		t.Fatalf("the backfill ended with %v still listed", ids)
	}
	assertStamped("a-moving")
}
