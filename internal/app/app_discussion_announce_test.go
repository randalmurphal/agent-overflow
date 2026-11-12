package app

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
)

// Starting a discussion changes the parent row's mode and discussion id. A
// client that did not start it learns both from the row frame.
func TestStartDiscussionAnnouncesTheParentRow(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	bus := transport.NewEventBus(64)
	t.Cleanup(bus.Close)
	app.SetEventBus(bus)
	app.startSessionFn = func(string) error { return nil }

	thread := testThread("thread-discussion-announce")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := app.store.CreateDiscussionDef(store.DiscussionDefinition{
		ID: "def-announce", Name: "Pair", Scope: "global",
		Participants: []store.DiscussionParticipant{
			{Role: "architect", System: "Design the change"},
			{Role: "reviewer", System: "Review the change"},
		},
		Settings:  store.DiscussionSettings{MaxTurns: 4},
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	other := bus.Subscribe()
	t.Cleanup(other.Close)

	if err := app.StartDiscussion(thread.ID, "Pair"); err != nil {
		t.Fatalf("StartDiscussion: %v", err)
	}
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatal(err)
	}

	for {
		select {
		case e := <-other.Events():
			if e.Channel != string(eventchan.ThreadUpdated) {
				continue
			}
			var frame triage.ThreadUpdateEvent
			if err := json.Unmarshal(e.Data, &frame); err != nil {
				t.Fatal(err)
			}
			if frame.Thread == nil || frame.Thread.ID != thread.ID {
				continue
			}
			if frame.Action != triage.ThreadActionFull || frame.Thread.Mode != "discussion" || frame.Thread.DiscussionID != stored.DiscussionID {
				t.Fatalf("parent frame = %s %+v, want full with mode discussion and id %s", frame.Action, frame.Thread, stored.DiscussionID)
			}
			return
		default:
			t.Fatal("no thread:updated frame for the parent was queued before StartDiscussion returned")
		}
	}
}
