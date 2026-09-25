package store

import (
	"fmt"
	"slices"
	"testing"
)

// A thread's write to its own row, payload or turn row a fork shows
// (fork_reown.go).

// TestShownHistoryRewritesGoToTheForks: a thread's write to its own row or
// payload a fork shows lands, and every thread that read it through the
// writer keeps what it read: a holder of the writer's takes a copy first,
// which those readers read right before the writer, and no reader's stamp
// moves. The writer reads the write.
func TestShownHistoryRewritesGoToTheForks(t *testing.T) {
	edited := "edited"
	for _, tc := range []struct {
		name, writer string
		readers      []string
		rewrite      func(*Store) error
		copies       []string
	}{
		{"rewrite meta", "S", []string{"F", "G"}, func(s *Store) error { return s.UpdateItemMeta("S", "u0", `{"changed":true}`) }, []string{"u0"}},
		{"rewrite fields", "S", []string{"F", "G"}, func(s *Store) error {
			_, err := s.UpdateItemFields("S", "a0", ItemPartialUpdate{Summary: &edited})
			return err
		}, []string{"a0"}},
		{"move row", "S", []string{"F", "G"}, func(s *Store) error { _, err := s.BumpItemToTurnEnd("S", "u1", nil, 5); return err }, []string{"u1"}},
		{"upsert row", "S", []string{"F", "G"}, func(s *Store) error {
			_, err := s.UpsertItem(Item{ID: "a1", ThreadID: "S", TurnIndex: 1, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "late", Meta: "{}", UpdatedAt: 5}, nil)
			return err
		}, []string{"a1"}},
		{"upsert payload", "S", []string{"F", "G"}, func(s *Store) error {
			_, err := s.UpsertItem(Item{ID: "tool", ThreadID: "S", TurnIndex: 1, Kind: "tool_call", Role: "assistant", Status: "completed", Meta: "{}", UpdatedAt: 5},
				&Payload{ID: "p", Kind: "text", Meta: "{}", Data: []byte("late output")})
			return err
		}, []string{"tool"}},
		{"append payload", "S", []string{"F", "G"}, func(s *Store) error { return s.AppendPayloadData("S", "p", []byte(" more"), "{}", 5) }, []string{"tool"}},
		{"replace payload", "S", []string{"F", "G"}, func(s *Store) error { return s.ReplacePayloadData("S", "p", []byte("new"), "{}", 5) }, []string{"tool"}},
		{"payload meta", "S", []string{"F", "G"}, func(s *Store) error { return s.UpdatePayloadMeta("S", "p", `{"late":true}`) }, []string{"tool"}},
		{"fork copy-on-write under its fork", "F", []string{"G"}, func(s *Store) error {
			return s.UpdateItemMeta("F", "u0", `{"changed":true}`)
		}, []string{"u0"}},
		{"fork payload copy under its fork", "F", []string{"G"}, func(s *Store) error {
			return s.AppendPayloadData("F", "p", []byte(" fork"), "{}", 5)
		}, []string{"tool"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := forkChainFixture(t)
			writer := timelineShape(t, s, tc.writer)
			shapes, stamps := map[string][]string{}, map[string]HistoryStamp{}
			for _, id := range tc.readers {
				shapes[id], stamps[id] = timelineShape(t, s, id), historyStampOf(t, s, id)
			}
			if err := tc.rewrite(s); err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if slices.Equal(timelineShape(t, s, tc.writer), writer) {
				t.Fatalf("%s does not read its write", tc.writer)
			}
			for id, shape := range shapes {
				requireShape(t, s, id, shape)
				if now := historyStampOf(t, s, id); now != stamps[id] {
					t.Fatalf("%s's stamp moved %+v -> %+v", id, stamps[id], now)
				}
			}
			holders := holderIDs(t, s)
			if len(holders) != 1 {
				t.Fatalf("holders = %v, want one", holders)
			}
			requireIDs(t, "holder rows", ownIDs(t, s, holders[0]), tc.copies)
			var origin string
			if err := s.db.QueryRow(`SELECT fork_source_thread_id FROM threads WHERE id = ?`, holders[0]).Scan(&origin); err != nil || origin != tc.writer {
				t.Fatalf("the holder holds rows of %q, %v; want %s", origin, err, tc.writer)
			}
			for _, reader := range tc.readers {
				if got := holderOf(t, s, reader); got != holders[0] {
					t.Fatalf("%s reads holder %s, want %s", reader, got, holders[0])
				}
			}
		})
	}
}

// TestShownTurnRewritesGoToTheForks: a late write to a settled turn row a
// fork shows lands on the source's row; the fork reads a holder's copy of
// the row as it was. A turn row no fork shows takes the write in place.
func TestShownTurnRewritesGoToTheForks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(*Store) error
	}{
		{"complete", func(s *Store) error {
			return s.UpdateTurnCompleted("S:0", 9, "late_stop", "late-msg", `{"input":2}`, "")
		}},
		{"late payload", func(s *Store) error {
			return s.UpdateTurnLatePayload("S:0", LateTurnPayload{AssistantMessageIDOverwrite: "late-msg", ErrorMessageOverwrite: "late error"})
		}},
		{"provider id", func(s *Store) error { return s.BackfillTurnProviderID("S:0", "wire-0") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			seedLinearSource(t, s, "S", 2)
			for turn := range 2 {
				mustExec(t, s.db, `INSERT INTO turns (turn_id, thread_id, turn_index, started_at, completed_at, stop_reason)
					VALUES (?, 'S', ?, 1, 2, 'end_turn')`, fmt.Sprintf("S:%d", turn), turn)
			}
			mustPointerFork(t, s, "S", "F", ForkCut{})
			before, stamp := forkTurns(t, s, "F"), historyStampOf(t, s, "F")
			if err := tc.write(s); err != nil {
				t.Fatal(err)
			}
			if slices.Equal(forkTurns(t, s, "S"), before) {
				t.Fatal("the source does not read its write")
			}
			requireIDs(t, "F turns", forkTurns(t, s, "F"), before)
			if now := historyStampOf(t, s, "F"); now != stamp {
				t.Fatalf("F's stamp moved %+v -> %+v", stamp, now)
			}
			h := holderOf(t, s, "F")
			requireIDs(t, "F lineage", forkLineage(t, s, "F"), []string{"1:" + h + ":1:2", "2:S:1:2"})
			// The row of the fork's last turn is its own; a write there
			// needs no holder.
			if err := s.UpdateTurnCompleted("S:1", 9, "late_stop", "", "", ""); err != nil {
				t.Fatal(err)
			}
			if got := holderIDs(t, s); len(got) != 1 {
				t.Fatalf("holders = %v, want the one", got)
			}
		})
	}
}

// TestImportTurnCompletionGivesForksTheTurn: a refresh import that settles
// again a turn a fork shows lands on the source's row; the fork reads a
// holder's copy of the row as it was.
func TestImportTurnCompletionGivesForksTheTurn(t *testing.T) {
	s := newTestStore(t)
	newImportTargetThread(t, s, "S")
	if err := s.ApplyImportBatch("S", importBatchFixture("S")); err != nil {
		t.Fatal(err)
	}
	appendSourceTurn(t, s, "S", 2, "next")
	mustPointerFork(t, s, "S", "F", ForkCut{})
	before, stamp := forkTurns(t, s, "F"), historyStampOf(t, s, "F")
	refresh := ImportBatch{TurnCompletions: []TurnCompletion{{
		TurnID: "S:1", CompletedAt: importTurnComplete + 5, StopReason: "refreshed", AssistantMessageID: "msg-2",
	}}}
	if err := s.ApplyImportBatch("S", refresh); err != nil {
		t.Fatalf("refresh of a turn the fork shows: %v", err)
	}
	requireIDs(t, "F turns", forkTurns(t, s, "F"), before)
	if now := historyStampOf(t, s, "F"); now != stamp {
		t.Fatalf("F's stamp moved %+v -> %+v", stamp, now)
	}
	if turn, found, err := s.GetTurn("S:1"); err != nil || !found || turn.StopReason != "refreshed" {
		t.Fatalf("the source's turn = %+v found=%v, %v", turn, found, err)
	}
}

// TestLateSettleOfATurnReadPastANearerCut: a fork's revert lowers its own
// fork's cut on it, and that fork still reads the source's turn row past
// the lowered cut, where the reverted fork now holds a row of its own. A
// late settle of the source's row gives the fork its copy first.
func TestLateSettleOfATurnReadPastANearerCut(t *testing.T) {
	s := newTestStore(t)
	seedTurnedSource(t, s, "S", 4)
	mustPointerFork(t, s, "S", "F", ForkCut{})
	mustPointerFork(t, s, "F", "G", ForkCut{})
	views := readerViews(t, s, "G")
	if _, _, err := s.DeleteConversationFromItem("F", "a1"); err != nil {
		t.Fatal(err)
	}
	requireViews(t, s, views, "after F's revert")
	if held, found, err := s.GetTurnByThreadIndex("F", 1); err != nil || !found || held.TurnID != "F:1" {
		t.Fatalf("F's turn 1 = %+v found=%v, %v; want its own", held, found, err)
	}
	if err := s.UpdateTurnCompleted("S:1", 50, "late", "", "", ""); err != nil {
		t.Fatalf("late settle of a turn G reads: %v", err)
	}
	requireViews(t, s, views, "after the late settle")
}

// forkTurns renders the turn rows a thread reads.
func forkTurns(t *testing.T, s *Store, threadID string) []string {
	t.Helper()
	turns, err := s.ListRecentTurns(threadID, 1000)
	if err != nil {
		t.Fatalf("turns of %s: %v", threadID, err)
	}
	out := make([]string, len(turns))
	for i, turn := range turns {
		completed := int64(-1)
		if turn.CompletedAt != nil {
			completed = *turn.CompletedAt
		}
		out[i] = fmt.Sprintf("%d %d %s %s %s %s %s", turn.TurnIndex, completed, turn.StopReason, turn.AssistantMessageID,
			turn.TokenUsageJSON, turn.ErrorMessage, turn.ProviderTurnID)
	}
	return out
}
