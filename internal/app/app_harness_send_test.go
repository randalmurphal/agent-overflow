package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil/reporoot"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
)

// The soak sends the page's default send: the reference is
// frontend/src/test/fixtures/pageDefaultSend.json, which a frontend test
// holds to buildSendOptions, read here as the bound method reads a page's
// send. Only the send id differs.
func TestSoakSendsThePageDefaultSend(t *testing.T) {
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
		t.Fatalf("soak options = %+v (draft %+v)\nthe page's default send = %s", got, got.ConsumeDraft, raw)
	}
}

// An awaited harness send is admitted only at rest. A thread that is working
// refuses it and nothing is recorded: no row, no queued message, no message
// waiting for its turn. A thread at rest answers the turn its row opened and
// the completion sequence read before the send.
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

	idle := newThread("harness-send-idle")
	if err := app.store.InsertItem(store.Item{ID: "earlier", ThreadID: idle.ID, TurnIndex: 0, Kind: "assistant_text", Role: "assistant", Summary: "prior turn", CreatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	sess, err := claude.NewSession(context.Background(), idle.ID, claude.Config{
		Reaper:    procutiltest.Reaper(),
		Retention: claude.UserHomeRetention(0),
		Binary:    writeClaudePassthroughBinary(t),
		WorkDir:   idle.WorkspacePath,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(idle.ID, session{Provider: string(provider.Claude), Token: "test-token", Claude: sess})

	// A completion sequenced while the send runs is above the floor the
	// answer carries.
	app.sendStageFn = func(threadID, stage string) {
		if threadID == idle.ID && stage == "placed" {
			if _, err := bus.Emit(eventchan.ProviderTurnCompleted, map[string]any{"threadId": "elsewhere", "turnIndex": 0}); err != nil {
				t.Error(err)
			}
		}
	}
	got, err := host.Send(context.Background(), idle.ID, "at rest", true)
	if err != nil {
		t.Fatalf("Send at rest only to an idle thread: %v", err)
	}
	rows := userRows(idle.ID)
	if len(rows) != 1 || got.Queued || got.TurnIndex != rows[0].TurnIndex || got.TurnIndex != 1 {
		t.Fatalf("answer = %+v, rows = %+v, want turn 1, the turn of its row", got, rows)
	}
	if got.CompletionsAfter != 1 {
		t.Fatalf("CompletionsAfter = %d, want 1, the completion sequenced before the send", got.CompletionsAfter)
	}
}
