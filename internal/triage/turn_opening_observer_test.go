package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// openingSeen is what the turn-opening observer saw at one call: the
// thread, whether the router's lock was held, and the turn row then.
type openingSeen struct {
	thread    string
	locked    bool
	row       store.Turn
	rowExists bool
}

// observeOpenings records every turn-opening mark, reading turn turnID's
// row as each is made.
func observeOpenings(t *testing.T, r *Router, st *store.Store, turnID string) *[]openingSeen {
	t.Helper()
	var seen []openingSeen
	r.SetTurnOpeningObserver(func(threadID string) {
		locked := !r.mu.TryLock()
		if !locked {
			r.mu.Unlock()
		}
		row, found, err := st.GetTurn(turnID)
		if err != nil {
			t.Errorf("read turn %s: %v", turnID, err)
		}
		seen = append(seen, openingSeen{thread: threadID, locked: locked, row: row, rowExists: found})
	})
	return &seen
}

// Every turn open tells the observer at its mark, under the router's lock,
// before its turn row is open: a provider's start, before the row exists;
// a soft-closed turn's continuation, while the row still reads settled;
// and a queued message's echo, before the row exists.
func TestEveryTurnOpeningIsObservedAtItsMark(t *testing.T) {
	handle := func(t *testing.T, r *Router, evt provider.ProviderEvent) {
		t.Helper()
		evt.ThreadID, evt.Timestamp = "t1", time.Now()
		if err := r.Handle(evt); err != nil {
			t.Fatalf("%s: %v", evt.Kind, err)
		}
	}
	expectOne := func(t *testing.T, seen []openingSeen, open func(openingSeen) bool) {
		t.Helper()
		if len(seen) != 1 || seen[0].thread != "t1" || !seen[0].locked || open(seen[0]) {
			t.Fatalf("observed %+v; want one mark of t1 under the router's lock, its row not yet open", seen)
		}
	}
	t.Run("start", func(t *testing.T) {
		r, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		seen := observeOpenings(t, r, st, "t1:0")
		handle(t, r, provider.ProviderEvent{Kind: provider.EventTurnStart})
		expectOne(t, *seen, func(s openingSeen) bool { return s.rowExists })
	})
	t.Run("soft-close continuation", func(t *testing.T) {
		r, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		handle(t, r, provider.ProviderEvent{Kind: provider.EventTurnStart})
		handle(t, r, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"}})
		seen := observeOpenings(t, r, st, "t1:0")
		handle(t, r, parentContentBlockStart("t1", "text"))
		expectOne(t, *seen, func(s openingSeen) bool { return !s.rowExists || s.row.CompletedAt == nil })
		if turn, _, err := st.GetTurn("t1:0"); err != nil || turn.CompletedAt != nil {
			t.Fatalf("the continuation did not reopen its row: %+v, %v", turn, err)
		}
	})
	t.Run("queued echo", func(t *testing.T) {
		r, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		now := time.Now().UnixMilli()
		if _, err := st.AppendItem(store.Item{ID: "user:1", ThreadID: "t1", TurnIndex: 1, Kind: "user_text", Role: "user",
			Status: "completed", Summary: "queued", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		r.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{ProviderItemID: "echo-1"})
		seen := observeOpenings(t, r, st, "t1:1")
		handle(t, r, provider.ProviderEvent{Kind: provider.EventUserText, Content: "queued", Meta: json.RawMessage(`{"provider_item_id":"echo-1"}`)})
		expectOne(t, *seen, func(s openingSeen) bool { return s.rowExists })
		if got := r.OpenTurnIndex("t1"); got != 1 {
			t.Fatalf("open turn after the echo = %d; want 1", got)
		}
	})
}
