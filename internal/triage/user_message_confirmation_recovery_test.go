package triage

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/usermessage"
)

func TestOccupiedDeferredConfirmationKeepsFirstEchoPlacementOnRecovery(t *testing.T) {
	for _, mode := range []string{"echo-retry", "retry-without-parent", "retry-other-id-by-client", "session-death"} {
		t.Run(mode, func(t *testing.T) {
			path := storetest.ClonePath(t)
			st, err := store.New(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			router := NewRouter(st, func(eventchan.Channel, any) {})
			createTestThread(t, st, "t1")
			seedOpenTurn(t, router, st, "t1", 1)
			prefix := store.Item{ID: "prefix", ThreadID: "t1", TurnIndex: 1, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "before consumption"}
			if err := router.persistItem(prefix, nil); err != nil {
				t.Fatal(err)
			}
			row := store.Item{ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1, Kind: "user_text", Role: "user", Status: "completed", Summary: "queued", CreatedAt: 10}
			expect := PendingSendExpectation{ProviderItemID: "echo"}
			if mode == "retry-other-id-by-client" {
				expect = PendingSendExpectation{ByClientID: true}
			}
			router.RegisterPendingFlushSendWithExpectation("t1", "queue:q1", row, 10, expect)
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if _, err := raw.Exec(`CREATE TRIGGER fail_prompt BEFORE INSERT ON items WHEN NEW.id = 'user:1:flush:1' BEGIN SELECT RAISE(ABORT, 'injected prompt write failure'); END`); err != nil {
				t.Fatal(err)
			}
			firstEcho := time.UnixMilli(100000)
			echo := provider.ProviderEvent{Kind: provider.EventUserText, ThreadID: "t1", Content: "queued", Meta: json.RawMessage(`{"provider_item_id":"echo","parent_uuid":"parent"}`), Timestamp: firstEcho}
			if mode == "retry-other-id-by-client" {
				echo.Meta = json.RawMessage(`{"provider_item_id":"echo","parent_uuid":"parent","client_id":"user:1:flush:1"}`)
			}
			if err := router.Handle(echo); err == nil {
				t.Fatal("expected injected first write failure")
			}
			if _, found, _ := st.GetThreadItem("t1", row.ID); found {
				t.Fatal("failed write left prompt durable")
			}
			if turn, ok := router.openTurnIndex("t1"); !ok || turn != 1 {
				t.Fatalf("provider attribution changed: %d %v", turn, ok)
			}
			response := prefix
			response.ID = "response"
			response.Summary = "after consumption"
			if err := router.persistItem(response, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(`DROP TRIGGER fail_prompt`); err != nil {
				t.Fatal(err)
			}
			if mode != "session-death" {
				if mode == "retry-without-parent" {
					echo.Meta = json.RawMessage(`{"provider_item_id":"echo"}`)
				}
				if mode == "retry-other-id-by-client" {
					echo.Meta = json.RawMessage(`{"provider_item_id":"other-echo","parent_uuid":"wrong-parent","client_id":"user:1:flush:1"}`)
				}
				echo.Timestamp = firstEcho.Add(time.Hour)
				if err := router.Handle(echo); err != nil {
					t.Fatal(err)
				}
			} else if restored := router.DrainUnconfirmedFlushItems("t1"); len(restored) != 0 {
				t.Fatalf("consumed message restored for resend: %+v", restored)
			}
			prompt := mustGetItem(t, st, "t1", row.ID)
			if usermessage.ReadProviderItemID(prompt.Meta) != "echo" || usermessage.ReadProviderParentUUID(prompt.Meta) != "parent" {
				t.Fatalf("retry lost first echo identity: %s", prompt.Meta)
			}
			before := mustGetItem(t, st, "t1", "prefix")
			after := mustGetItem(t, st, "t1", "response")
			if !(before.ItemIndex < prompt.ItemIndex && prompt.ItemIndex < after.ItemIndex) {
				t.Fatalf("order prefix=%d prompt=%d response=%d", before.ItemIndex, prompt.ItemIndex, after.ItemIndex)
			}
			if prompt.CreatedAt != firstEcho.UnixMilli() || prompt.TurnIndex != 1 {
				t.Fatalf("lost first-echo timestamp/turn: %+v", prompt)
			}
			if _, _, err := st.DeleteConversationFromItem("t1", row.ID); err != nil {
				t.Fatal(err)
			}
			if _, found, _ := st.GetThreadItem("t1", "prefix"); !found {
				t.Fatal("revert removed provider prefix")
			}
			if _, found, _ := st.GetThreadItem("t1", "response"); found {
				t.Fatal("revert kept prompt response")
			}
		})
	}
}
