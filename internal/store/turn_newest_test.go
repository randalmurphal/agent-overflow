package store

import "testing"

// An open newest row reads as the boot sweep then records it: ended at its
// last item write, and unread by the rule of a recorded interruption.
func TestAnOpenNewestTurnReadsAsTheSweepRecordsIt(t *testing.T) {
	cases := []struct {
		name       string
		itemAt     int64 // the turn's last item write; 0 for no item
		lastReadAt any   // threads.last_read_at
		wantEnded  int64
		wantUnread bool
	}{
		{name: "no item ends at its start", wantEnded: 200, lastReadAt: nil, wantUnread: true},
		{name: "ends at its last item write", itemAt: 450, lastReadAt: 300, wantEnded: 450, wantUnread: true},
		{name: "read after its end", itemAt: 450, lastReadAt: 500, wantEnded: 450, wantUnread: false},
		{name: "no later than now", itemAt: 5000, lastReadAt: nil, wantEnded: 999, wantUnread: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			mustCreateThreadForTurn(t, s, "t1")
			if err := s.InsertTurn(makeInflightTurn("turn-0", "t1", 0, 1)); err != nil {
				t.Fatalf("insert turn-0: %v", err)
			}
			if err := s.UpdateTurnCompleted("turn-0", 100, "end_turn", "", "", ""); err != nil {
				t.Fatalf("settle turn-0: %v", err)
			}
			if err := s.InsertTurn(makeInflightTurn("turn-1", "t1", 1, 200)); err != nil {
				t.Fatalf("insert turn-1: %v", err)
			}
			if tc.itemAt != 0 {
				seedItemWithStatus(t, s, "t1", "text", 1, 0, "assistant_text", "streaming", "Half", false)
				if _, err := s.db.Exec(`UPDATE items SET created_at = 210, updated_at = ? WHERE id = 'text'`, tc.itemAt); err != nil {
					t.Fatalf("date item: %v", err)
				}
			}
			if _, err := s.db.Exec(`UPDATE threads SET last_read_at = ? WHERE id = 't1'`, tc.lastReadAt); err != nil {
				t.Fatalf("set last read: %v", err)
			}

			got, ok, err := s.NewestOpenTurn("t1", 999)
			if err != nil || !ok {
				t.Fatalf("NewestOpenTurn: ok=%v err=%v", ok, err)
			}
			want := EndedTurn{TurnID: "turn-1", TurnIndex: 1, StartedAt: 200, EndedAt: tc.wantEnded, Unread: tc.wantUnread}
			if got != want {
				t.Fatalf("NewestOpenTurn = %+v, want %+v", got, want)
			}

			if _, err := s.RecoverCrashedTurns(testInterruptedSummary, 999); err != nil {
				t.Fatalf("RecoverCrashedTurns: %v", err)
			}
			swept, ok, err := s.GetTurn("turn-1")
			if err != nil || !ok {
				t.Fatalf("GetTurn: ok=%v err=%v", ok, err)
			}
			if swept.CompletedAt == nil || *swept.CompletedAt != got.EndedAt {
				t.Fatalf("sweep recorded the end at %v, the open row read %d", swept.CompletedAt, got.EndedAt)
			}
			thread, err := s.GetThread("t1")
			if err != nil {
				t.Fatalf("GetThread: %v", err)
			}
			if thread.HasIncompleteTurn != got.Unread {
				t.Fatalf("swept thread unread interruption = %v, the open row read %v", thread.HasIncompleteTurn, got.Unread)
			}
			if _, ok, err := s.NewestOpenTurn("t1", 999); err != nil || ok {
				t.Fatalf("NewestOpenTurn after the sweep: ok=%v err=%v, want none", ok, err)
			}
		})
	}
}

// Only a thread's newest row counts: an open row under a later turn is not
// the thread's turn in flight.
func TestListOpenNewestTurnsListsOnlyNewestOpenRows(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"open", "older-open", "settled", "none"} {
		mustCreateThreadForTurn(t, s, id)
	}
	if err := s.InsertTurn(makeInflightTurn("open-0", "open", 0, 1)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.InsertTurn(makeInflightTurn("older-open-0", "older-open", 0, 1)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.InsertTurn(makeInflightTurn("older-open-1", "older-open", 1, 2)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.UpdateTurnCompleted("older-open-1", 3, "end_turn", "", "", ""); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := s.InsertTurn(makeInflightTurn("settled-0", "settled", 0, 1)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.UpdateTurnCompleted("settled-0", 3, "end_turn", "", "", ""); err != nil {
		t.Fatalf("settle: %v", err)
	}

	got, err := s.ListOpenNewestTurns()
	if err != nil {
		t.Fatalf("ListOpenNewestTurns: %v", err)
	}
	if len(got) != 1 || got[0] != (OpenNewestTurn{ThreadID: "open", TurnID: "open-0"}) {
		t.Fatalf("ListOpenNewestTurns = %+v, want only open/open-0", got)
	}
	for _, id := range []string{"older-open", "settled", "none"} {
		if _, ok, err := s.NewestOpenTurn(id, 10); err != nil || ok {
			t.Fatalf("NewestOpenTurn(%s): ok=%v err=%v, want none", id, ok, err)
		}
	}
}
