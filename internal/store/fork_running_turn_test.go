package store

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
)

// What a pointer fork owns of the turns its source has not finished.

// TestPointerForkOwnsTheSourcesUnsettledTurns: a turn row the source has
// not settled below the cut turn is the fork's own copy, settled as
// interrupted, so the source's later settle of its row (the crash sweep
// here, an import's completion) changes no turn row the fork shows.
func TestPointerForkOwnsTheSourcesUnsettledTurns(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "src")
	for i := range 3 {
		if err := insertCarded(s, Item{ID: fmt.Sprintf("u%d", i), ThreadID: "src", TurnIndex: i, Kind: "user_text", Role: "user", Status: "completed", CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertTurn(Turn{TurnID: fmt.Sprintf("src:%d", i), ThreadID: "src", TurnIndex: i, StartedAt: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpdateTurnCompleted("src:1", 3, "end_turn", "", "", ""); err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "src", "fork", ForkCut{})
	type shown struct {
		id, stop  string
		completed int64
	}
	forkTurns := func() []shown {
		t.Helper()
		var out []shown
		for i := range 3 {
			turn, found, err := s.GetTurnByThreadIndex("fork", i)
			if err != nil || !found || turn.CompletedAt == nil {
				t.Fatalf("fork turn %d = %+v, found=%v, %v; want a settled row", i, turn, found, err)
			}
			out = append(out, shown{turn.TurnID, turn.StopReason, *turn.CompletedAt})
		}
		return out
	}
	want := []shown{{"fork:0", "interrupted", 999}, {"src:1", "end_turn", 3}, {"fork:2", "interrupted", 999}}
	if got := forkTurns(); !slices.Equal(got, want) {
		t.Fatalf("fork turns = %+v, want %+v", got, want)
	}
	crashed, err := s.RecoverCrashedTurns(testInterruptedSummary, 50)
	if err != nil {
		t.Fatalf("the source's crash sweep: %v", err)
	}
	if !slices.Equal(crashed, []CrashedTurn{{"src", 0}, {"src", 2}}) {
		t.Fatalf("crashed turns = %+v, want the source's two", crashed)
	}
	if got := forkTurns(); !slices.Equal(got, want) {
		t.Fatalf("the source's settle changed the fork's turns to %+v", got)
	}
	if turn, _, err := s.GetTurnByThreadIndex("src", 0); err != nil || turn.CompletedAt == nil || *turn.CompletedAt != 50 {
		t.Fatalf("source turn 0 = %+v, %v; want settled by the sweep", turn, err)
	}
}

// TestPointerForkOfARunningTurnOwnsItsRows: a fork of a source that is
// still running the cut turn owns a copy of that turn's rows, so the
// writes the source's provider still makes to them (a queued message's
// fold, its move to the turn's end, a Codex spawn's identity, a settle)
// change nothing the fork shows. A settled turn's rows stay the source's,
// and a rewrite of one gives the fork a copy first.
func TestPointerForkOfARunningTurnOwnsItsRows(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "src")
	rows := []Item{
		{ID: "u0", TurnIndex: 0, ItemIndex: 0, Kind: "user_text", Role: "user", Status: "completed"},
		{ID: "a0", TurnIndex: 0, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", Status: "completed"},
		{ID: "u1", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user", Status: "completed"},
		{ID: "spawn", TurnIndex: 1, ItemIndex: 1, Kind: "tool_call", ToolName: "collab_agent", Role: "assistant", Status: "completed", Meta: `{"input":{}}`},
		{ID: "q1", TurnIndex: 1, ItemIndex: 2, Kind: "user_text", Role: "user", Status: "completed", Summary: "first"},
		{ID: "q2", TurnIndex: 1, ItemIndex: 3, Kind: "user_text", Role: "user", Status: "completed", Summary: "second"},
		{ID: "a1", TurnIndex: 1, ItemIndex: 4, Kind: "assistant_text", Role: "assistant", Status: "streaming", Summary: "partial"},
	}
	for _, it := range rows {
		it.ThreadID, it.CreatedAt, it.UpdatedAt = "src", 1, 1
		if it.Meta == "" {
			it.Meta = "{}"
		}
		if err := insertCarded(s, it); err != nil {
			t.Fatalf("insert %s: %v", it.ID, err)
		}
	}
	for turn := range 2 {
		if err := s.InsertTurn(Turn{TurnID: fmt.Sprintf("src:%d", turn), ThreadID: "src", TurnIndex: turn, StartedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpdateTurnCompleted("src:0", 2, "end_turn", "", "", ""); err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "src", "fork", ForkCut{})
	owned, err := queryIDs(s.db, `SELECT id FROM items WHERE thread_id = 'fork' ORDER BY turn_index, item_index`)
	if err != nil || !slices.Equal(owned, []string{"u1", "spawn", "q1", "q2", "a1"}) {
		t.Fatalf("the fork owns %v, %v; want the running turn's rows", owned, err)
	}
	before, err := s.ListItems("fork")
	if err != nil {
		t.Fatal(err)
	}
	stamp, _, err := s.ThreadHistoryStamp("fork")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.FoldUserTextRows("src", "q2", []string{"q1"}, "first second", "{}", 5); err != nil {
		t.Fatalf("fold: %v", err)
	}
	if _, err := s.BumpItemToTurnEnd("src", "q2", nil, 6); err != nil {
		t.Fatalf("bump: %v", err)
	}
	if err := s.UpdateItemMeta("src", "spawn", `{"input":{"newAgentNickname":"Ada"}}`); err != nil {
		t.Fatalf("spawn identity: %v", err)
	}
	completed, summary := "completed", "whole"
	if _, err := s.UpdateItemFields("src", "a1", ItemPartialUpdate{Status: &completed, Summary: &summary}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	after, err := s.ListItems("fork")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("the source's writes changed the fork:\n%+v\nwas\n%+v", after, before)
	}
	if got, _, err := s.ThreadHistoryStamp("fork"); err != nil || got != stamp {
		t.Fatalf("the source's writes moved the fork's stamp %+v -> %+v, %v", stamp, got, err)
	}
	// A settled turn's row the fork shows goes to it first.
	if _, err := s.UpdateItemFields("src", "a0", ItemPartialUpdate{Summary: &summary}); err != nil {
		t.Fatalf("a rewrite of the settled turn's row: %v", err)
	}
	if after, err := s.ListItems("fork"); err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("the rewrite changed the fork, %v:\n%+v\nwas\n%+v", err, after, before)
	}
	if got, _, err := s.ThreadHistoryStamp("fork"); err != nil || got != stamp {
		t.Fatalf("the rewrite moved the fork's stamp %+v -> %+v, %v", stamp, got, err)
	}
}
