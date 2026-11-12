package app

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
)

// queuedThreadFrames returns the thread:updated frames already queued on sub.
func queuedThreadFrames(t *testing.T, sub *transport.Subscriber) []triage.ThreadUpdateEvent {
	t.Helper()
	var out []triage.ThreadUpdateEvent
	for {
		select {
		case e := <-sub.Events():
			if e.Channel != string(eventchan.ThreadUpdated) {
				continue
			}
			var frame triage.ThreadUpdateEvent
			if err := json.Unmarshal(e.Data, &frame); err != nil {
				t.Fatal(err)
			}
			out = append(out, frame)
		default:
			return out
		}
	}
}

func busApp(t *testing.T) (*App, *transport.Subscriber) {
	t.Helper()
	app := newTestAppWithStore(t)
	bus := transport.NewEventBus(64)
	t.Cleanup(bus.Close)
	app.SetEventBus(bus)
	sub := bus.Subscribe()
	t.Cleanup(sub.Close)
	return app, sub
}

// A resume cursor the app writes itself (rollback, imported branch) is
// announced: the session init that follows reports the same cursor and
// finds nothing to announce.
func TestWriteSessionRefAnnouncesOnlyAChange(t *testing.T) {
	t.Parallel()
	app, sub := busApp(t)
	thread := testThread("thread-session-ref-announce")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}

	if err := app.writeSessionRef(thread.ID, "session-after-rollback"); err != nil {
		t.Fatal(err)
	}
	frames := queuedThreadFrames(t, sub)
	if len(frames) != 1 || frames[0].Action != triage.ThreadActionPatch || frames[0].SessionRef == nil || *frames[0].SessionRef != "session-after-rollback" {
		t.Fatalf("frames = %+v, want one sessionRef patch", frames)
	}

	if err := app.writeSessionRef(thread.ID, "session-after-rollback"); err != nil {
		t.Fatal(err)
	}
	if frames := queuedThreadFrames(t, sub); len(frames) != 0 {
		t.Fatalf("an unchanged cursor announced %+v", frames)
	}
}

// Focus repairs model settings the provider cannot run; the repaired row is
// what every client must show.
func TestFocusModelRepairAnnouncesTheRow(t *testing.T) {
	t.Parallel()
	app, sub := busApp(t)
	thread := testThread("thread-focus-repair")
	thread.Provider, thread.Model, thread.ContextWindow = "claude", "claude-sonnet-4-6", 7
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}

	repaired, err := app.loadThreadForFocus(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.ContextWindow == 7 {
		t.Fatalf("fixture did not need a repair: %+v", repaired)
	}
	frames := queuedThreadFrames(t, sub)
	if len(frames) != 1 || frames[0].Thread == nil || frames[0].Thread.ContextWindow != repaired.ContextWindow {
		t.Fatalf("frames = %+v, want one row frame with context window %d", frames, repaired.ContextWindow)
	}
}
