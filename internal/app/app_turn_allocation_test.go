package app

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/triage"
)

// A turn the CLI starts on its own between a send's placement and its
// row's persist takes the index placement chose. The send is placed again
// under the turn allocation lock, so its row and its turn follow the
// provider's turn instead of sharing its index.
func TestDirectSendAfterProviderTurnTakesTheNextIndex(t *testing.T) {
	t.Parallel()
	app, bus, thread := startTwoTurnClaudeThread(t)
	if err := app.SendMessage(thread.ID, "first", nil); err != nil {
		t.Fatalf("SendMessage 1: %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)

	// The wake lands after placement chose turn 1: its turns row exists
	// before the send's row does.
	app.sendStageFn = func(threadID, stage string) {
		if stage != "placed" {
			return
		}
		app.sendStageFn = nil
		now := time.Now().UnixMilli()
		if err := app.store.InsertTurn(store.Turn{TurnID: threadID + ":1", ThreadID: threadID, TurnIndex: 1, StartedAt: now}); err != nil {
			t.Errorf("insert wake turn: %v", err)
		}
	}
	if err := app.SendMessage(thread.ID, "second", nil); err != nil {
		t.Fatalf("SendMessage 2: %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	for _, item := range items {
		if item.Summary == "second" || item.Summary == "turn two" {
			if item.TurnIndex != 2 {
				t.Fatalf("%q on turn %d, want 2, after the provider's turn 1", item.Summary, item.TurnIndex)
			}
		}
	}
	_ = app.StopSession(thread.ID)
}

// A wake that lands after a direct send's row is persisted, before its
// provider write, finds the registered send and takes its turn, rather
// than opening a provider-started turn past the row.
func TestWakeAfterDirectSendPersistTakesItsTurn(t *testing.T) {
	t.Parallel()
	app, bus, thread := startTwoTurnClaudeThread(t)
	if err := app.SendMessage(thread.ID, "first", nil); err != nil {
		t.Fatalf("SendMessage 1: %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)

	app.sendStageFn = func(threadID, stage string) {
		if stage != "persisted" {
			return
		}
		app.sendStageFn = nil
		if err := app.triage.Handle(provider.ProviderEvent{Kind: provider.EventInit, ThreadID: threadID, Timestamp: time.Now()}); err != nil {
			t.Errorf("wake init: %v", err)
		}
		if got := app.triage.OpenTurnIndex(threadID); got != 1 {
			t.Errorf("wake opened turn %d, want the send's turn 1", got)
		}
	}
	if err := app.SendMessage(thread.ID, "second", nil); err != nil {
		t.Fatalf("SendMessage 2: %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)
	if _, found, err := app.store.GetTurnByThreadIndex(thread.ID, 2); err != nil || found {
		t.Fatalf("turn 2 row: found=%v err=%v, want none", found, err)
	}
	_ = app.StopSession(thread.ID)
}

// A session torn down and replaced after a send registered with its row
// (an account switch reconnects Codex before dispatch) cleared the
// registry. The send registers again before its provider write, so the
// replacement session's turn start still finds it.
func TestSendRegistersAgainAfterSessionReplacedBeforeDispatch(t *testing.T) {
	t.Parallel()
	app, bus, thread := startTwoTurnClaudeThread(t)
	if err := app.SendMessage(thread.ID, "first", nil); err != nil {
		t.Fatalf("SendMessage 1: %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)

	app.sendStageFn = func(threadID, stage string) {
		if stage != "persisted" {
			return
		}
		app.sendStageFn = nil
		app.triage.CleanupThread(threadID)
		app.triage.MarkThreadActive(threadID)
	}
	if err := app.SendMessage(thread.ID, "second", nil); err != nil {
		t.Fatalf("SendMessage 2: %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)
	turn, found, err := app.store.GetTurnByThreadIndex(thread.ID, 1)
	if err != nil || !found {
		t.Fatalf("turn 1 row: found=%v err=%v, want the send's turn", found, err)
	}
	if turn.CompletedAt == nil {
		t.Fatal("the send's turn 1 never completed")
	}
	_ = app.StopSession(thread.ID)
}

// startTwoTurnClaudeThread starts a Claude session whose mock answers two
// sends, one turn each.
func startTwoTurnClaudeThread(t *testing.T) (*App, *capturedEventBus, store.Thread) {
	t.Helper()
	app, bus := setupE2EApp(t)
	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	responses := [][]string{
		{
			`{"type":"system","subtype":"init","session_id":"sess-alloc","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
			`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"turn one"}]}}`,
			`{"type":"result","subtype":"success","is_error":false}`,
		},
		{
			`{"type":"system","subtype":"init","session_id":"sess-alloc","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
			`{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"turn two"}]}}`,
			`{"type":"result","subtype":"success","is_error":false}`,
		},
	}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), responses)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	return app, bus, thread
}

// A queued message's dispatch holds the turn allocation lock from its
// placement through its registration, so a provider turn decision waits
// for it and then finds the registered send.
func TestFlushDispatchHoldsTurnAllocationThroughRegistration(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	thread := testThread("flush-turn-allocation")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	sess := installSteerTestSession(t, app, thread, "ok")
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Codex), Token: "alloc-token", Codex: sess})

	unlock := app.triage.LockTurnAllocation(thread.ID)
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{{ID: "queue:alloc", Message: "queued", Payload: json.RawMessage(`{}`)}})
	}()
	time.Sleep(100 * time.Millisecond)
	if app.triage.HasPendingSendForThread(thread.ID) {
		unlock()
		t.Fatal("dispatch registered its send while the turn allocation lock was held")
	}
	unlock()
	<-done
	if !app.triage.HasPendingSendForThread(thread.ID) {
		t.Fatal("dispatch did not register its send")
	}
}
