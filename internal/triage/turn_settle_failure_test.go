package triage

import (
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
)

// A turn whose end cannot be recorded stays open, which every reader takes
// for a running turn, so the thread says why: one error row at that turn,
// however many completions report it, and another for a later turn that
// fails the same way. Triage settles both providers' turns alike.
func TestAFailedTurnSettleReachesTheThread(t *testing.T) {
	for _, threadProvider := range []string{"claude", "codex"} {
		t.Run(threadProvider, func(t *testing.T) {
			path := storetest.ClonePath(t)
			st, err := store.New(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := st.Close(); err != nil {
					t.Errorf("close store: %v", err)
				}
			})
			// The sidebar's Failed pill on every client rides thread:error_notice.
			var notices atomic.Int32
			router := NewRouter(st, func(channel eventchan.Channel, _ any) {
				if channel == eventchan.ThreadErrorNotice {
					notices.Add(1)
				}
			})
			t.Cleanup(router.DrainWireItemRefresh)
			if threadProvider == "codex" {
				createCodexTestThread(t, st, "t1")
			} else {
				createTestThread(t, st, "t1")
			}

			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := raw.Close(); err != nil {
					t.Errorf("close the trigger connection: %v", err)
				}
			}()
			if _, err := raw.Exec(`CREATE TRIGGER refuse_turn_settle BEFORE UPDATE OF completed_at ON turns
				WHEN NEW.completed_at IS NOT NULL BEGIN SELECT RAISE(ABORT, 'injected settle failure'); END`); err != nil {
				t.Fatal(err)
			}

			settlePrefix := TurnEndFailureSummary(errors.New(""))
			reports := func(turnIndex int) int {
				t.Helper()
				n := 0
				for _, row := range findItemsByKind(t, st, "t1", ItemKindError) {
					if row.TurnIndex == turnIndex && strings.HasPrefix(row.Summary, settlePrefix) && strings.Contains(row.Summary, "injected settle failure") {
						n++
					}
				}
				return n
			}

			parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0})
			parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta()})
			// The synthetic abort a provider sends after its real result.
			parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: &provider.WireTurnCompleteMeta{Aborted: true}})
			if got := reports(0); got != 1 {
				t.Fatalf("reports of turn 0 = %d, want 1: %+v", got, findItemsByKind(t, st, "t1", ItemKindError))
			}
			if got := notices.Load(); got != 1 {
				t.Fatalf("error notices after turn 0 = %d, want 1", got)
			}
			if open, found, err := st.GetActiveTurn("t1"); err != nil || !found || open.TurnIndex != 0 {
				t.Fatalf("GetActiveTurn = %+v, %v, %v; want turn 0 still open", open, found, err)
			}

			parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1})
			parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta()})
			if got := reports(1); got != 1 {
				t.Fatalf("reports of turn 1 = %d, want 1: %+v", got, findItemsByKind(t, st, "t1", ItemKindError))
			}
			if got := reports(0); got != 1 {
				t.Fatalf("reports of turn 0 after turn 1 = %d, want 1", got)
			}
			if got := notices.Load(); got != 2 {
				t.Fatalf("error notices after turn 1 = %d, want 2", got)
			}
			thread, err := st.GetThread("t1")
			if err != nil {
				t.Fatalf("GetThread: %v", err)
			}
			if !thread.HasFailedTurn || !thread.HasOpenTurn {
				t.Fatalf("thread row: HasFailedTurn %v, HasOpenTurn %v; want both", thread.HasFailedTurn, thread.HasOpenTurn)
			}
		})
	}
}
