package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// The caller can have missed turn_started on another connection. The
// transport-facing Send RPC must still use the provider's active turn, not
// allocate a phantom next turn merely because that frontend chose Send.
func TestComposerSendActiveUsesQueue(t *testing.T) {
	for _, p := range []string{string(provider.Codex), string(provider.Claude)} {
		t.Run(string(p), func(t *testing.T) {
			a := newTestAppWithStore(t)
			a.triage = triage.NewRouter(a.store, func(eventchan.Channel, any) {})
			thread := testThread("stale-idle-send")
			thread.Provider = string(p)
			if err := a.store.CreateThread(thread); err != nil {
				t.Fatal(err)
			}
			if err := a.store.InsertTurn(store.Turn{TurnID: "active", ThreadID: thread.ID, TurnIndex: 3, StartedAt: time.Now().UnixMilli()}); err != nil {
				t.Fatal(err)
			}
			opts := SendMessageOptions{ReconcileBySendID: true, SendID: "one-send"}
			for range 2 {
				if _, err := a.SendMessageWithOptions(context.Background(), thread.ID, "only once", opts); err != nil {
					t.Fatal(err)
				}
			}
			queued, err := a.GetQueueState(thread.ID)
			if err != nil || len(queued) != 1 {
				t.Fatalf("queue = %+v, %v", queued, err)
			}
			if queued[0].SendID != opts.SendID {
				t.Fatalf("send identity lost: %+v", queued[0])
			}
			items, err := a.store.ListItemsForTurn(thread.ID, 4)
			if err != nil || len(items) != 0 {
				t.Fatalf("phantom next-turn rows = %+v, %v", items, err)
			}
		})
	}
}

func TestComposerSendBeforeTurnStartedUsesQueue(t *testing.T) {
	a := newTestAppWithStore(t)
	a.triage = triage.NewRouter(a.store, func(eventchan.Channel, any) {})
	thread := testThread("send-before-start")
	if err := a.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	a.triage.RegisterPendingSendWithExpectation(thread.ID, "user:0", 0, triage.PendingSendExpectation{})
	if _, err := a.SendMessageWithOptions(context.Background(), thread.ID, "second client", SendMessageOptions{ReconcileBySendID: true, SendID: "second"}); err != nil {
		t.Fatal(err)
	}
	queued, err := a.GetQueueState(thread.ID)
	if err != nil || len(queued) != 1 {
		t.Fatalf("queue = %+v, %v", queued, err)
	}
}

func TestComposerSendActiveCodexEchoKeepsActualTurn(t *testing.T) {
	for _, outcome := range []string{"ok", "no-active-turn"} {
		t.Run(outcome, func(t *testing.T) {
			a := newTestAppWithStore(t)
			a.triage = triage.NewRouter(a.store, func(eventchan.Channel, any) {})
			thread := testThread("active-codex-send")
			thread.Provider = string(provider.Codex)
			thread.WorkspacePath = initGitRepo(t)
			if err := a.store.CreateThread(thread); err != nil {
				t.Fatal(err)
			}
			if err := a.store.InsertTurn(store.Turn{TurnID: "active", ThreadID: thread.ID, TurnIndex: 3, StartedAt: time.Now().UnixMilli()}); err != nil {
				t.Fatal(err)
			}
			sess := installSteerTestSession(t, a, thread, outcome)
			a.sessionManager().put(thread.ID, session{Provider: string(provider.Codex), Token: "test-token", Codex: sess})
			if _, err := a.SendMessageWithOptions(context.Background(), thread.ID, "follow-up", SendMessageOptions{ReconcileBySendID: true, SendID: "one-send"}); err != nil {
				t.Fatal(err)
			}
			queue := a.triage.QueuedFlushItems(thread.ID)
			if len(queue) != 1 {
				t.Fatalf("queue = %+v", queue)
			}
			// A response row arriving before consumption retains its position.
			if err := a.triage.PersistItem(store.Item{ID: "response", ThreadID: thread.ID, TurnIndex: 3, ItemIndex: 9, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "before follow-up"}, nil); err != nil {
				t.Fatal(err)
			}
			a.dispatchFlush(thread.ID, queue)
			expectedTurn := 3
			if outcome == "no-active-turn" {
				expectedTurn = 4
			}
			state, err := a.GetThreadLiveState(thread.ID)
			if err != nil || len(state.FlushedItems) != 1 || state.FlushedItems[0].SendID != "one-send" {
				t.Fatalf("flushed identity = %+v, %v", state.FlushedItems, err)
			}
			meta, _ := json.Marshal(map[string]string{"provider_item_id": "wire-follow-up", "client_id": state.FlushedItems[0].UserItemID})
			if err := a.triage.Handle(provider.ProviderEvent{Kind: provider.EventUserText, ThreadID: thread.ID, TurnIndex: expectedTurn, ItemID: "wire-follow-up", Content: "follow-up", Meta: meta, Timestamp: time.Now()}); err != nil {
				t.Fatal(err)
			}
			item, found, err := a.store.FindUserTextItemBySendID(thread.ID, "one-send")
			if err != nil || !found {
				t.Fatalf("message missing: %v, %v", found, err)
			}
			response, _, err := a.store.GetThreadItem(thread.ID, "response")
			if err != nil {
				t.Fatal(err)
			}
			if item.TurnIndex != expectedTurn || (expectedTurn == 3 && item.ItemIndex <= response.ItemIndex) {
				t.Fatalf("wrong message placement: %+v", item)
			}
			if _, err := a.SendMessageWithOptions(context.Background(), thread.ID, "follow-up", SendMessageOptions{ReconcileBySendID: true, SendID: "one-send"}); err != nil {
				t.Fatal(err)
			}
			items, err := a.store.ListItemsForTurn(thread.ID, expectedTurn+1)
			if err != nil || len(items) != 0 {
				t.Fatalf("retry created future rows: %+v, %v", items, err)
			}
		})
	}
}

// Passthrough provider fixtures do not emit consumption/completion events.
// Tests that send a second independent turn must supply that missing wire
// boundary, rather than asking Send to create a turn while the first is pending.
func acknowledgeMockClaudeSend(t *testing.T, a *App, threadID string) {
	t.Helper()
	rows, err := a.store.ListItems(threadID)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if row.Kind != "user_text" {
			continue
		}
		if err := a.triage.Handle(provider.ProviderEvent{Kind: provider.EventUserText, ThreadID: threadID, TurnIndex: row.TurnIndex, ItemID: row.ID, Content: row.Summary, Meta: json.RawMessage(row.Meta), Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if active, ok := a.triage.ActiveTurnSnapshot(threadID); ok {
			if err := a.triage.Handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: threadID, TurnID: active.TurnID, TurnIndex: active.TurnIndex, Timestamp: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}
		if a.triage.HasPendingSendForThread(threadID) {
			t.Fatal("mock send echo did not consume pending send")
		}
		return
	}
	t.Fatal("mock send has no user row to acknowledge")
}

// An older composer predicts user:<nextTurn> and cannot retire that placeholder
// when authoritative admission queues the message. Refuse before accepting it
// so its existing failure path restores the draft and removes the placeholder.
func TestLegacyComposerBusySendIsNotAccepted(t *testing.T) {
	for _, p := range []provider.ProviderKind{provider.Claude, provider.Codex} {
		for _, state := range []string{"active", "starting"} {
			t.Run(string(p)+"/"+state, func(t *testing.T) {
				a := newTestAppWithStore(t)
				a.triage = triage.NewRouter(a.store, func(eventchan.Channel, any) {})
				thread := testThread("legacy-busy")
				thread.Provider = string(p)
				if err := a.store.CreateThread(thread); err != nil {
					t.Fatal(err)
				}
				if state == "active" {
					if err := a.store.InsertTurn(store.Turn{TurnID: "active", ThreadID: thread.ID, TurnIndex: 3, StartedAt: 1}); err != nil {
						t.Fatal(err)
					}
				} else {
					a.triage.RegisterPendingSendWithExpectation(thread.ID, "user:3", 3, triage.PendingSendExpectation{})
				}
				if err := a.SaveDraft(t.Context(), thread.ID, "keep this draft", nil, nil, nil); err != nil {
					t.Fatal(err)
				}
				_, err := a.SendMessageWithOptions(t.Context(), thread.ID, "not accepted", SendMessageOptions{SendID: "legacy-send"})
				if err == nil || !strings.Contains(err.Error(), "already working") {
					t.Fatalf("legacy send: %v", err)
				}
				if record, found, err := a.findRecordedSend(thread.ID, "legacy-send"); err != nil || found {
					t.Fatalf("rejected send accepted: %+v, %v, %v", record, found, err)
				}
				queued, err := a.GetQueueState(thread.ID)
				if err != nil || len(queued) != 0 {
					t.Fatalf("rejected send queued: %+v, %v", queued, err)
				}
				rows, err := a.store.ListItems(thread.ID)
				if err != nil || len(rows) != 0 {
					t.Fatalf("rejected send persisted: %+v, %v", rows, err)
				}
				draft, err := a.GetDraft(thread.ID)
				if err != nil || draft.Content != "keep this draft" {
					t.Fatalf("rejected send changed draft: %+v, %v", draft, err)
				}
			})
		}
	}
}
