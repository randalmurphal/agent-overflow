package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harnessrpc"
	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil/reporoot"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
)

// HarnessSend sends the page's default send: the reference is
// frontend/src/test/fixtures/pageDefaultSend.json, which a frontend test
// holds to buildSendOptions, read here as the bound method reads a page's
// send. Only the send id differs.
func TestHarnessSendsThePageDefaultSend(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(reporoot.Path(t, "frontend/src/test/fixtures/pageDefaultSend.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want SendMessageOptions
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("decode the page's default send: %v", err)
	}
	got := pageSendOptions("send-1")
	if got.SendID != "send-1" {
		t.Fatalf("SendID = %q, want the one given", got.SendID)
	}
	got.SendID = ""
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("harness send options = %+v (draft %+v)\nthe page's default send = %s", got, got.ConsumeDraft, raw)
	}
}

// An awaited harness send is admitted only at rest. A thread that is working
// refuses it and nothing is recorded: no row, no queued message, no message
// waiting for its turn. A thread at rest answers the turn its row was placed
// in and the completion floor read once the send was admitted.
func TestHarnessSendAtRestOnly(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	bus := transport.NewEventBus(16)
	app.SetEventBus(bus)
	t.Cleanup(bus.Close)
	if _, err := bus.Emit(eventchan.ProviderTurnCompleted, map[string]any{"threadId": "earlier", "turnIndex": 0}); err != nil {
		t.Fatal(err)
	}
	host := &harnessHost{app: app}
	newThread := func(id string) store.Thread {
		thread := testThread(id)
		thread.Provider = string(provider.Claude)
		thread.WorkspacePath = t.TempDir()
		if err := app.store.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
		return thread
	}
	userRows := func(threadID string) []store.Item {
		items, err := app.store.ListItems(threadID)
		if err != nil {
			t.Fatal(err)
		}
		var rows []store.Item
		for _, item := range items {
			if item.Kind == "user_text" {
				rows = append(rows, item)
			}
		}
		return rows
	}

	working := map[string]func(threadID string){
		"turn open": func(threadID string) {
			if err := app.store.InsertTurn(store.Turn{TurnID: "open", ThreadID: threadID, TurnIndex: 0, StartedAt: time.Now().UnixMilli()}); err != nil {
				t.Fatal(err)
			}
		},
		"message waiting for its turn": func(threadID string) {
			app.triage.RegisterPendingSendWithExpectation(threadID, "user:0", 0, triage.PendingSendExpectation{})
		},
	}
	for name, makeWorking := range working {
		thread := newThread("harness-send-working-" + strings.ReplaceAll(name, " ", "-"))
		makeWorking(thread.ID)
		indexBefore, pendingBefore := app.triage.MaxPendingSendTurnIndex(thread.ID)
		if _, err := host.Send(context.Background(), thread.ID, "refused", true); !errors.Is(err, errThreadWorking) {
			t.Fatalf("%s: Send at rest only = %v, want the working thread refused", name, err)
		}
		if rows := userRows(thread.ID); len(rows) != 0 {
			t.Fatalf("%s: rows after a refusal = %+v", name, rows)
		}
		if queued, err := app.GetQueueState(thread.ID); err != nil || len(queued) != 0 {
			t.Fatalf("%s: queue after a refusal = %+v, %v", name, queued, err)
		}
		if index, pending := app.triage.MaxPendingSendTurnIndex(thread.ID); pending != pendingBefore || index != indexBefore {
			t.Fatalf("%s: messages waiting for a turn = %d (%v) after a refusal, %d (%v) before", name, index, pending, indexBefore, pendingBefore)
		}
	}

	withSession := func(thread store.Thread) {
		sess, err := claude.NewSession(context.Background(), thread.ID, claude.Config{
			Reaper:    procutiltest.Reaper(),
			Retention: claude.UserHomeRetention(0),
			Binary:    writeClaudePassthroughBinary(t),
			WorkDir:   thread.WorkspacePath,
		}, func(provider.ProviderEvent) {})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sess.Close() })
		app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "test-token", Claude: sess})
	}

	// An ordinary send reads no floor.
	ordinary := newThread("harness-send-ordinary")
	withSession(ordinary)
	if got, err := host.Send(context.Background(), ordinary.ID, "ordinary", false); err != nil || got.Queued || got.CompletionsAfter != 0 {
		t.Fatalf("ordinary send = %+v, %v, want a turn of its own and no completion floor", got, err)
	}

	idle := newThread("harness-send-idle")
	if err := app.store.InsertItem(store.Item{ID: "earlier", ThreadID: idle.ID, TurnIndex: 0, Kind: "assistant_text", Role: "assistant", Summary: "prior turn", CreatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	withSession(idle)

	// The floor is read once the send is admitted and before its turn is
	// asked for. A completion published while an action holds the thread's
	// lock and the send waits is at or below it; one published after
	// placement, at the send's "placed" stage, is above it.
	unlock := sync.OnceFunc(app.threadLocks().Lock(idle.ID))
	var afterPlacement transport.Event
	app.sendStageFn = func(threadID, stage string) {
		if threadID != idle.ID || stage != "placed" {
			return
		}
		var err error
		if afterPlacement, err = bus.Emit(eventchan.ProviderTurnCompleted, map[string]any{"threadId": "elsewhere", "turnIndex": 0}); err != nil {
			t.Error(err)
		}
	}
	type answer struct {
		got harnessrpc.HarnessSendResult
		err error
	}
	answered := make(chan answer, 1)
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		got, err := host.Send(context.Background(), idle.ID, "at rest", true)
		answered <- answer{got, err}
	}()
	// Registered after the fixture's cleanups, so it runs before them: the
	// send is let through and finishes before its sessions, bus and store
	// are torn down, whichever assertion ends the test.
	t.Cleanup(func() {
		unlock()
		<-sent
	})
	waitForThreadLockRefs(t, app.threadLocks(), idle.ID, 2)
	whileWaiting, err := bus.Emit(eventchan.ProviderTurnCompleted, map[string]any{"threadId": idle.ID, "turnIndex": 0})
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	result := <-answered
	got, err := result.got, result.err
	if err != nil {
		t.Fatalf("Send at rest only to an idle thread: %v", err)
	}
	if afterPlacement.Seq == 0 {
		t.Fatal("the at-rest send never reached its placed stage")
	}
	if got.CompletionsAfter < whileWaiting.Seq || got.CompletionsAfter >= afterPlacement.Seq {
		t.Fatalf("CompletionsAfter = %d, want at or above %d (published while the send waited for the lock) and below %d (published after placement)",
			got.CompletionsAfter, whileWaiting.Seq, afterPlacement.Seq)
	}
	rows := userRows(idle.ID)
	if len(rows) != 1 || got.Queued || got.TurnIndex != rows[0].TurnIndex || got.TurnIndex != 1 {
		t.Fatalf("answer = %+v, rows = %+v, want turn 1, the turn of its row", got, rows)
	}
}
