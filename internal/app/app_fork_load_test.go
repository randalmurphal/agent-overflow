package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// Exercise triage's real flush lock and SQLite writer from both the source
// and another thread. Mock sessions supply the provider checkpoint separately.
func startForkStreamLoad(t *testing.T, app *App, ids ...string) (func(), *atomic.Int64) {
	t.Helper()
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	ticks := &atomic.Int64{}
	emit := func(id string) error {
		err := app.triage.Handle(provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: id, ItemID: "load-text", Content: strings.Repeat("stream ", 400), Timestamp: time.Now()})
		if err == nil {
			ticks.Add(1)
		}
		return err
	}
	for _, id := range ids {
		if err := emit(id); err != nil {
			t.Fatal(err)
		}
	}
	stopped := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopped:
				done <- nil
				return
			case <-ticker.C:
				for _, id := range ids {
					if err := emit(id); err != nil {
						done <- err
						return
					}
				}
			}
		}
	}()
	stop := sync.OnceFunc(func() {
		close(stopped)
		if err := <-done; err != nil {
			t.Error(err)
		}
		for _, id := range ids {
			if err := app.triage.FlushThread(id); err != nil {
				t.Error(err)
			}
		}
	})
	t.Cleanup(stop)
	return stop, ticks
}

func TestLargeLiveForkWithStreamLoad(t *testing.T) {
	app := newTestApp(t)
	fixture := newMidTurnForkFixture(t, "mid-turn-session", midTurnSourceJSONL)
	source := createAppTestThread(t, app, "loaded-source", "claude", fixture.workspace)
	source.SessionRef = fixture.sessionID
	if err := app.store.UpdateThread(source); err != nil {
		t.Fatal(err)
	}
	seedMidTurnSourceRows(t, app.store, source.ID)
	root := store.Item{ID: "history-agent", ThreadID: source.ID, TurnIndex: 0, ItemIndex: 5, Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "Agent", Summary: "completed agent", Meta: "{}", CreatedAt: 1, UpdatedAt: 1}
	if err := app.store.InsertItem(root); err != nil {
		t.Fatal(err)
	}
	const historyRows = 1536
	for i := range historyRows {
		id := fmt.Sprintf("history-%d", i)
		item := store.Item{ID: id, ThreadID: source.ID, TurnIndex: 0, ItemIndex: 6 + i, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "historical answer", ParentID: root.ID, PayloadID: id, Meta: `{"provider_item_id":"historical"}`, CreatedAt: 1, UpdatedAt: 1}
		if err := app.store.InsertItemWithPayload(item, store.Payload{ID: id, Kind: "text", Data: []byte(strings.Repeat("history ", 1024)), Meta: "{}", CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	other := store.BuildForkedThread(source)
	other.ID = "other-stream"
	if err := app.store.CreateThread(other); err != nil {
		t.Fatal(err)
	}
	openTurn(t, app.store, other.ID, "other:0", 0)
	attachLiveClaudeSession(t, app, source.ID, fixture.workspace, fixture.sessionID, "a1")
	stop, ticks := startForkStreamLoad(t, app, source.ID, other.ID)
	started := time.Now()
	fork, err := app.ForkThread(context.Background(), source.ID, nil)
	stop()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("live fork with %d nested history rows: %s; stream events: %d", historyRows, time.Since(started), ticks.Load())
	if fork.ForkPreparing || fork.PendingForkResumeAt != "a1" || fork.PendingForkRef != fixture.sessionID {
		t.Fatalf("fork identity/readiness: %+v", fork)
	}
	items, err := app.store.ListItems(fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	var historical int
	var streaming store.Item
	for _, item := range items {
		if item.Summary == "historical answer" {
			historical++
			if item.ParentID != root.ID || item.ThreadID != fork.ID {
				t.Fatalf("incorrect fork parent: %s", item.ID)
			}
		}
		if strings.HasPrefix(item.Summary, "stream ") {
			streaming = item
		}
	}
	if historical != historyRows {
		t.Fatalf("history rows=%d, want %d", historical, historyRows)
	}
	if streaming.ID == "" || streaming.Status != "errored" {
		t.Fatalf("streaming snapshot not interrupted: %+v", streaming)
	}
	before, err := app.store.GetPayloadData(fork.ID, streaming.PayloadID)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.AppendPayloadData(source.ID, streaming.PayloadID, []byte("later source output"), "{}", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	after, err := app.store.GetPayloadData(fork.ID, streaming.PayloadID)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("source continuation changed fork context")
	}
	if _, active, err := app.store.GetActiveTurn(source.ID); err != nil || !active {
		t.Fatalf("source stopped: active=%v err=%v", active, err)
	}
	if _, active, err := app.store.GetActiveTurn(fork.ID); err != nil || active {
		t.Fatalf("fork retained active turn: active=%v err=%v", active, err)
	}
}

func TestForkCancelledWhileWaitingForSourceAction(t *testing.T) {
	for _, message := range []bool{false, true} {
		t.Run(fmt.Sprintf("message=%v", message), func(t *testing.T) {
			app := newTestApp(t)
			source := createAppTestThread(t, app, "busy-source", "claude", t.TempDir())
			unlock := app.threadLocks().Lock(source.ID)
			defer unlock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				if message {
					_, err = app.ForkThreadFromMessage(ctx, source.ID, "unused")
				} else {
					_, err = app.ForkThread(ctx, source.ID, nil)
				}
				done <- err
			}()
			deadline := time.After(2 * time.Second)
			for app.threadLocks().Refs(source.ID) != 2 {
				select {
				case <-deadline:
					t.Fatal("fork never waited on source action lock")
				default:
					time.Sleep(time.Millisecond)
				}
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled fork: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancelled fork remained blocked behind source action")
			}
		})
	}
}
