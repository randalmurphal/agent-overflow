package app

import (
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

func TestHistoryPreparationLifetimeAndLiveWrites(t *testing.T) {
	app := retentionTestApp(t)
	seedThread(t, app, "history", 1)
	seed := func(offset int) {
		t.Helper()
		for i := offset; i < offset+150; i++ {
			item := store.Item{ThreadID: "history", ID: fmt.Sprint(i), TurnIndex: i, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "settled", Meta: "{}", CreatedAt: 1, UpdatedAt: 1}
			if err := app.store.InsertItem(item); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed(0)
	app.startHistoryPreparation()
	app.startHistoryPreparation()
	t.Cleanup(app.historyPreparation.halt)
	// Writes continue while the loop changes their physical representation.
	seed(150)
	waitPrepared := func() {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			items, err := app.store.ListItems("history")
			if err != nil {
				t.Fatal(err)
			}
			ready := len(items) > 0
			for _, item := range items {
				ready = ready && item.Rev < 0
			}
			if ready {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("background preparation failed to finish")
	}
	waitPrepared()
	app.historyPreparation.halt()
	app.historyPreparation.halt()
	seed(300)
	items, err := app.store.ListItems("history")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 450 || items[449].Rev < 0 {
		t.Fatal("stopped loop still preparing or lost history")
	}
	app.startHistoryPreparation()
	waitPrepared()
	// Joining also covers cancellation during a store call, before Close.
	app.historyPreparation.halt()
}
