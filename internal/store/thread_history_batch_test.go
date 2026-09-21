package store

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

const (
	historyTurnStart    = 1_700_000_100_000
	historyTurnComplete = 1_700_000_160_000
)

// historyTurns lists a thread's turns oldest first. The store's own
// listing is newest first, which is the wrong order to compare two
// threads' history in.
func historyTurns(t *testing.T, s *Store, threadID string) []Turn {
	t.Helper()
	turns, err := s.ListRecentTurns(threadID, 100)
	if err != nil {
		t.Fatalf("list turns %s: %v", threadID, err)
	}
	slices.Reverse(turns)
	return turns
}

func threadHistoryFixture(threadID string) ThreadHistoryBatch {
	return ThreadHistoryBatch{
		Turns: []Turn{
			{TurnID: threadID + ":0", TurnIndex: 0, StartedAt: historyTurnStart},
			{TurnID: threadID + ":1", TurnIndex: 1, StartedAt: historyTurnStart + 1000},
		},
		Completions: []TurnCompletion{
			{TurnID: threadID + ":0", CompletedAt: historyTurnComplete, StopReason: "end_turn"},
			{TurnID: threadID + ":1", CompletedAt: historyTurnComplete + 1000, StopReason: "end_turn"},
		},
		Rows: []HistoryRow{
			{Item: Item{
				ID: "h-user-0", TurnIndex: 0, ItemIndex: 0, Kind: "user_text", Role: "user",
				Summary: "start the sweep", CreatedAt: historyTurnStart, UpdatedAt: historyTurnStart,
			}},
			{Item: Item{
				ID: "h-answer-0", TurnIndex: 0, ItemIndex: 1, Kind: "assistant_text", Role: "assistant",
				Summary: "sweeping now", CreatedAt: historyTurnStart, UpdatedAt: historyTurnStart,
			}},
			{Item: Item{
				ID: "h-user-1", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user",
				Summary: "show me the log", CreatedAt: historyTurnStart + 1000, UpdatedAt: historyTurnStart + 1000,
			}},
			{
				Item: Item{
					ID: "h-tool-1", TurnIndex: 1, ItemIndex: 1, Kind: "tool_call", Role: "assistant",
					Summary: "Bash", ToolName: "Bash", PayloadID: "h-payload-1",
					CreatedAt: historyTurnStart + 1000, UpdatedAt: historyTurnComplete + 1000,
				},
				Payload: &Payload{
					ID: "h-payload-1", Kind: "command_output", Meta: `{"exitCode":0}`,
					Data: []byte("scan complete\n"), CreatedAt: historyTurnComplete + 1000,
				},
			},
		},
	}
}

func TestInsertThreadHistoryWritesTurnsItemsAndPayloadsAtomically(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-history")

	if err := s.InsertThreadHistory("t-history", threadHistoryFixture("t-history")); err != nil {
		t.Fatalf("insert thread history: %v", err)
	}

	items, err := s.ListItems("t-history")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("listed %d items, want 4", len(items))
	}
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	if got, want := strings.Join(ids, ","), "h-user-0,h-answer-0,h-user-1,h-tool-1"; got != want {
		t.Errorf("item order = %s, want %s", got, want)
	}

	data, total, _, err := s.GetPayloadChunk("t-history", "h-payload-1", 0, 64)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if total != len("scan complete\n") || string(data) != "scan complete\n" {
		t.Errorf("payload = %q (%d bytes), want the row's own data", string(data), total)
	}

	turns := historyTurns(t, s, "t-history")
	if len(turns) != 2 {
		t.Fatalf("listed %d turns, want 2", len(turns))
	}
	for i, want := range []int64{historyTurnComplete, historyTurnComplete + 1000} {
		if turns[i].CompletedAt == nil || *turns[i].CompletedAt != want {
			t.Errorf("turn %d completed_at = %v, want %d", i, turns[i].CompletedAt, want)
		}
	}

	// One revision per item, exactly as four one-at-a-time inserts move it:
	// nothing here suppresses the history trigger.
	var revision int64
	if err := s.db.QueryRow(`SELECT history_rev FROM threads WHERE id = 't-history'`).Scan(&revision); err != nil {
		t.Fatalf("read history_rev: %v", err)
	}
	if revision != 4 {
		t.Errorf("history_rev = %d, want one per item (4)", revision)
	}

	// The settle-time search hook ran per row, so the batch is searchable
	// without waiting for the background index build.
	indexed := searchIndexRows(t, s, "t-history")
	for _, id := range []string{"h-user-0", "h-answer-0", "h-user-1", "h-tool-1"} {
		if _, ok := indexed[id]; !ok {
			t.Errorf("item %s is not in the search index", id)
		}
	}
	hits, err := s.SearchThreads("sweeping", ThreadSearchFilter{ThreadIDs: []string{"t-history"}, Limit: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].ItemID != "h-answer-0" {
		t.Errorf("search hits = %+v, want the assistant row", hits)
	}
}

func TestInsertThreadHistoryRollsBackOnFailure(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-history-atomic")

	batch := threadHistoryFixture("t-history-atomic")
	// A duplicate id inside the batch fails the last insert, after the
	// turns and the earlier rows have already been written in this tx.
	batch.Rows = append(batch.Rows, HistoryRow{Item: batch.Rows[0].Item})
	if err := s.InsertThreadHistory("t-history-atomic", batch); err == nil {
		t.Fatal("duplicate item id was accepted")
	}

	items, err := s.ListItems("t-history-atomic")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("failed batch left %d items behind", len(items))
	}
	turns := historyTurns(t, s, "t-history-atomic")
	if len(turns) != 0 {
		t.Errorf("failed batch left %d turns behind", len(turns))
	}
	var revision int64
	if err := s.db.QueryRow(
		`SELECT history_rev FROM threads WHERE id = 't-history-atomic'`,
	).Scan(&revision); err != nil {
		t.Fatalf("read history_rev: %v", err)
	}
	if revision != 0 {
		t.Errorf("failed batch left history_rev = %d, want 0", revision)
	}
}

func TestInsertThreadHistoryRefusesRowsOfAnotherThread(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-history-scope")
	mustCreateThread(t, s, "t-history-other")

	cases := []struct {
		name  string
		mutar func(*ThreadHistoryBatch)
		want  string
	}{
		{
			name:  "turn of another thread",
			mutar: func(b *ThreadHistoryBatch) { b.Turns[0].ThreadID = "t-history-other" },
			want:  "belongs to thread t-history-other",
		},
		{
			name:  "item of another thread",
			mutar: func(b *ThreadHistoryBatch) { b.Rows[0].Item.ThreadID = "t-history-other" },
			want:  "belongs to thread t-history-other",
		},
		{
			name: "payload the item does not reference",
			mutar: func(b *ThreadHistoryBatch) {
				b.Rows[3].Item.PayloadID = "h-payload-missing"
			},
			want: "not the payload",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			batch := threadHistoryFixture("t-history-scope")
			tc.mutar(&batch)
			err := s.InsertThreadHistory("t-history-scope", batch)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
			items, listErr := s.ListItems("t-history-scope")
			if listErr != nil {
				t.Fatalf("list items: %v", listErr)
			}
			if len(items) != 0 {
				t.Errorf("refused batch wrote %d items", len(items))
			}
		})
	}

	if err := s.InsertThreadHistory("", threadHistoryFixture("t-history-scope")); err == nil {
		t.Error("an empty thread id was accepted")
	}
}

// TestInsertThreadHistoryMatchesOneAtATimeWrites pins the batch to the
// writers it replaces: the same rows through InsertTurn / InsertItem /
// InsertItemWithPayload / UpdateTurnCompleted must leave the same thread.
func TestInsertThreadHistoryMatchesOneAtATimeWrites(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-history-batched")
	mustCreateThread(t, s, "t-history-one-by-one")

	if err := s.InsertThreadHistory("t-history-batched", threadHistoryFixture("t-history-batched")); err != nil {
		t.Fatalf("insert batched history: %v", err)
	}

	single := threadHistoryFixture("t-history-one-by-one")
	for _, turn := range single.Turns {
		turn.ThreadID = "t-history-one-by-one"
		if err := s.InsertTurn(turn); err != nil {
			t.Fatalf("insert turn %s: %v", turn.TurnID, err)
		}
	}
	for _, row := range single.Rows {
		row.Item.ThreadID = "t-history-one-by-one"
		var err error
		if row.Payload == nil {
			err = s.InsertItem(row.Item)
		} else {
			err = s.InsertItemWithPayload(row.Item, *row.Payload)
		}
		if err != nil {
			t.Fatalf("insert item %s: %v", row.Item.ID, err)
		}
	}
	for _, completion := range single.Completions {
		if err := s.UpdateTurnCompleted(
			completion.TurnID, completion.CompletedAt, completion.StopReason, "", "", "",
		); err != nil {
			t.Fatalf("complete turn %s: %v", completion.TurnID, err)
		}
	}

	describeItems := func(threadID string) string {
		items, err := s.ListItems(threadID)
		if err != nil {
			t.Fatalf("list items %s: %v", threadID, err)
		}
		var out strings.Builder
		for _, item := range items {
			fmt.Fprintf(&out, "%s|%d|%d|%s|%s|%s|%s|%d\n",
				item.ID, item.TurnIndex, item.ItemIndex, item.Kind, item.Role, item.Status, item.Summary, item.Rev)
		}
		return out.String()
	}
	if batched, single := describeItems("t-history-batched"), describeItems("t-history-one-by-one"); batched != single {
		t.Errorf("batched rows:\n%s\none-at-a-time rows:\n%s", batched, single)
	}

	describeTurns := func(threadID string) string {
		var out strings.Builder
		for _, turn := range historyTurns(t, s, threadID) {
			completed := int64(-1)
			if turn.CompletedAt != nil {
				completed = *turn.CompletedAt
			}
			fmt.Fprintf(&out, "%d|%d|%d|%s\n", turn.TurnIndex, turn.StartedAt, completed, turn.StopReason)
		}
		return out.String()
	}
	if batched, single := describeTurns("t-history-batched"), describeTurns("t-history-one-by-one"); batched != single {
		t.Errorf("batched turns:\n%s\none-at-a-time turns:\n%s", batched, single)
	}

	var batchedRev, singleRev int64
	if err := s.db.QueryRow(
		`SELECT history_rev FROM threads WHERE id = 't-history-batched'`,
	).Scan(&batchedRev); err != nil {
		t.Fatalf("read batched history_rev: %v", err)
	}
	if err := s.db.QueryRow(
		`SELECT history_rev FROM threads WHERE id = 't-history-one-by-one'`,
	).Scan(&singleRev); err != nil {
		t.Fatalf("read one-at-a-time history_rev: %v", err)
	}
	if batchedRev != singleRev {
		t.Errorf("history_rev = %d batched, %d one at a time", batchedRev, singleRev)
	}
}

func TestInsertThreadHistoryIndexesEveryBatchAndSkipsStreamingRows(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "history-search")
	var batch ThreadHistoryBatch
	want := map[string]bool{}
	for i := 0; i < 259; i++ {
		item := Item{ID: fmt.Sprintf("row-%03d", i), ThreadID: "history-search", ItemIndex: i, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "batchneedle", CreatedAt: 1}
		if i%7 == 0 {
			item.Status = "streaming"
		} else {
			want[item.ID] = true
		}
		batch.Rows = append(batch.Rows, HistoryRow{Item: item})
	}
	if err := s.InsertThreadHistory("history-search", batch); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchThreads("batchneedle", ThreadSearchFilter{ThreadIDs: []string{"history-search"}, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		if !want[hit.ItemID] {
			t.Fatalf("unexpected search hit %s", hit.ItemID)
		}
		delete(want, hit.ItemID)
	}
	if len(want) != 0 {
		t.Fatalf("%d rows missing from search", len(want))
	}
}
