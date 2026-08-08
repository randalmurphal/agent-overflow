package sessionimport

import (
	"testing"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// TestNewCursorTakesTheLastPositionNotTheLastRow: a tool_call is upserted
// in place across its whole life, so the batch's slice order is LAUNCH
// order — a launch early in a turn can be settled after rows that follow
// it. The cursor is the position the thread ends at.
func TestNewCursorTakesTheLastPositionNotTheLastRow(t *testing.T) {
	batch := store.ImportBatch{Rows: []store.ImportRow{
		{Item: store.Item{ID: "tool", TurnIndex: 2, ItemIndex: 0}},
		{Item: store.Item{ID: "text", TurnIndex: 2, ItemIndex: 3}},
		{Item: store.Item{ID: "early", TurnIndex: 1, ItemIndex: 9}},
	}}
	cursor := NewCursor(batch, nil)
	if cursor.TurnIndex != 2 || cursor.ItemIndex != 3 {
		t.Fatalf("cursor = %d/%d, want 2/3", cursor.TurnIndex, cursor.ItemIndex)
	}
}

func TestNewCursorOfAnEmptyBatchSitsBelowEveryRow(t *testing.T) {
	cursor := NewCursor(store.ImportBatch{}, nil)
	if cursor != EmptyCursor {
		t.Fatalf("cursor = %+v, want %+v", cursor, EmptyCursor)
	}
	if cursor.TurnIndex != -1 || cursor.ItemIndex != -1 {
		t.Fatalf("empty cursor = %d/%d, want -1/-1 so item 0 of turn 0 reads as growth",
			cursor.TurnIndex, cursor.ItemIndex)
	}
}

// TestNewCursorTakesTheLastSourceCoordinateSeen: events that produce no
// row still advance the file position a refresh must resume from.
func TestNewCursorTakesTheLastSourceCoordinateSeen(t *testing.T) {
	events := []importir.Event{
		{ProviderEvent: provider.ProviderEvent{Kind: provider.EventUserText},
			SourceUUID: "line:0", SourceOffset: 40},
		{ProviderEvent: provider.ProviderEvent{Kind: provider.EventTurnComplete},
			SourceUUID: "line:40", SourceOffset: 90},
	}
	cursor := NewCursor(store.ImportBatch{}, events)
	if cursor.SourceUUID != "line:40" || cursor.SourceOffset != 90 {
		t.Fatalf("source cursor = %q/%d, want line:40/90", cursor.SourceUUID, cursor.SourceOffset)
	}
}

// TestDivergedComparesThePair is the reason the cursor is two numbers.
// item_index restarts at 0 in every turn, so an item-index-only guard
// answers this case backwards: the import's last row is item 4, the live
// row is item 0, and 0 > 4 is false.
func TestDivergedComparesThePair(t *testing.T) {
	st := newTestStore(t)
	thread := seedThread(t, st, testThreadID, "claude", "/repo")

	state := store.ThreadImportState{
		ThreadID:        thread.ID,
		Provider:        "claude",
		SourcePath:      "/transcript.jsonl",
		SourceSessionID: "session-1",
		LastTurnIndex:   1,
		LastItemIndex:   4,
		ImportedAt:      baseMillis,
	}
	if err := st.SetThreadImportState(state); err != nil {
		t.Fatalf("set import state: %v", err)
	}

	putItem := func(turnIndex, itemIndex int, id string) {
		t.Helper()
		if _, err := st.UpsertItem(store.Item{
			ID: id, ThreadID: thread.ID, TurnIndex: turnIndex, ItemIndex: itemIndex,
			Kind: "user_text", Role: "user", Status: "completed", Summary: id,
			CreatedAt: baseMillis, UpdatedAt: baseMillis,
		}, nil); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	// Everything the import itself wrote.
	for i := 0; i <= 4; i++ {
		putItem(1, i, "imported-"+string(rune('a'+i)))
	}
	diverged, err := Diverged(st, state)
	if err != nil {
		t.Fatalf("Diverged: %v", err)
	}
	if diverged {
		t.Fatal("an untouched imported thread reads as diverged")
	}

	putItem(2, 0, "live")
	diverged, err = Diverged(st, state)
	if err != nil {
		t.Fatalf("Diverged after a live turn: %v", err)
	}
	if !diverged {
		t.Fatal("a live turn 2 item 0 did not register as growth past turn 1 item 4")
	}
}

func TestDivergedRefusesAnUnusableState(t *testing.T) {
	st := newTestStore(t)
	if _, err := Diverged(nil, store.ThreadImportState{ThreadID: "t"}); err == nil {
		t.Error("Diverged accepted a nil store")
	}
	if _, err := Diverged(st, store.ThreadImportState{}); err == nil {
		t.Error("Diverged accepted a state with no thread id")
	}
}

func TestCursorRoundTripsThroughImportState(t *testing.T) {
	cursor := Cursor{TurnIndex: 3, ItemIndex: 7, SourceUUID: "uuid-9", SourceOffset: 512}
	var state store.ThreadImportState
	cursor.Apply(&state)
	if got := CursorOf(state); got != cursor {
		t.Fatalf("round trip = %+v, want %+v", got, cursor)
	}
	// Apply on nil must not panic — the caller may be building a state it
	// has not allocated yet.
	cursor.Apply(nil)
}

// TestAdvanceNeverMovesACursorBackwards: a refresh folds its cursor onto the
// one already recorded, and each half of the pair can be un-advanced for its
// own reason — a tail that wrote no rows leaves the row position at
// EmptyCursor, a Codex tail carries no transcript uuid, a Claude tail carries
// no byte offset.
func TestAdvanceNeverMovesACursorBackwards(t *testing.T) {
	prev := Cursor{TurnIndex: 4, ItemIndex: 2, SourceUUID: "row-9", SourceOffset: 4096}

	if got := EmptyCursor.Advance(prev); got != prev {
		t.Fatalf("advance of an empty tail = %+v, want the recorded cursor %+v", got, prev)
	}

	claudeTail := Cursor{TurnIndex: 5, ItemIndex: 0, SourceUUID: "row-12"}
	got := claudeTail.Advance(prev)
	if got.TurnIndex != 5 || got.ItemIndex != 0 || got.SourceUUID != "row-12" {
		t.Fatalf("claude advance = %+v, want the tail's own position", got)
	}
	if got.SourceOffset != 4096 {
		t.Fatalf("claude advance dropped the recorded offset: %+v", got)
	}

	codexTail := Cursor{TurnIndex: 5, ItemIndex: 1, SourceOffset: 8192}
	got = codexTail.Advance(prev)
	if got.SourceUUID != "row-9" {
		t.Fatalf("codex advance dropped the recorded uuid: %+v", got)
	}
	if got.SourceOffset != 8192 {
		t.Fatalf("codex advance = %+v, want the tail's own offset", got)
	}
}
