package triage

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func TestCodexTerminalPollOnlyUpdatesProcessState(t *testing.T) {
	for _, phase := range []string{"active", "idle", "next-turn"} {
		t.Run(phase, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createCodexBackgroundTestThread(t, st, "t1")
			seedOpenTurn(t, router, st, "t1", 0)
			if err := router.Handle(provider.ProviderEvent{Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-1", ItemType: "commandExecution", Meta: buildUnifiedExecStartMeta(t, "pid-1", "sleep 10"), Timestamp: time.Now()}); err != nil {
				t.Fatal(err)
			}
			if phase != "active" {
				if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnID: "turn-0", TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "next-turn" {
				seedOpenTurn(t, router, st, "t1", 1)
			}
			for _, pid := range []string{"pid-1", "pid-1", "unknown", "pid-1"} {
				if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTerminalInteraction, ThreadID: "t1", TurnID: "turn-0", ItemID: "cmd-1", Meta: terminalInteractionMetaBlob(t, pid, ""), Timestamp: time.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			if rows := findItemsByKind(t, st, "t1", string(provider.ItemTerminalInteraction)); len(rows) != 0 {
				t.Fatalf("poll created history: %+v", rows)
			}
			live := router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
			if len(live) != 1 || live[0].ID != "cmd-1" || !live[0].IsBackground || live[0].Status != statusRunning {
				t.Fatalf("poll lost background process: %+v", live)
			}
			if meta := decodeItemMetaMap(t, live[0].Meta); meta["process_id"] != "pid-1" {
				t.Fatalf("poll changed process control identity: %+v", meta)
			}
		})
	}
}

func TestCodexTerminalPollPreservesHistoricalWaitRows(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	old := store.Item{ID: "waited:pid-1:0:0", ThreadID: "t1", TurnIndex: 0, Kind: string(provider.ItemTerminalInteraction), Role: "assistant", Status: statusCompleted, Summary: "Waited for background terminal", CreatedAt: 1, UpdatedAt: 1}
	if err := router.persistItem(old, nil); err != nil {
		t.Fatal(err)
	}
	before, _, err := st.GetThreadItem("t1", old.ID)
	if err != nil {
		t.Fatal(err)
	}
	seedTerminalInteractionBackgroundExec(t, router, "t1", "pid-1", "cmd-1", "sleep 10")
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTerminalInteraction, ThreadID: "t1", Meta: terminalInteractionMetaBlob(t, "pid-1", ""), Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	after, found, err := st.GetThreadItem("t1", old.ID)
	if err != nil || !found || after.Rev != before.Rev || after.Summary != before.Summary || after.ItemIndex != before.ItemIndex {
		t.Fatalf("historical wait changed: before=%+v after=%+v found=%v err=%v", before, after, found, err)
	}
}

func TestCodexTerminalPollRejectsMalformedMetadata(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	err := router.Handle(provider.ProviderEvent{Kind: provider.EventTerminalInteraction, ThreadID: "t1", Meta: []byte(`{"process_id":`), Timestamp: time.Now()})
	if err == nil {
		t.Fatal("invalid terminal metadata was silently ignored")
	}
}
