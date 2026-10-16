package triage

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
)

// A bounded announcement gives up when its ctx ends while another holds the
// thread's announce lock, and emits nothing.
func TestAnnounceWithinGivesUpWhenItsContextEnds(t *testing.T) {
	announcer := NewThreadAnnouncer(func(threadID string) (store.Thread, error) {
		return store.Thread{ID: threadID}, nil
	})
	release := announcer.Hold("t1")
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	emitted := 0
	done := make(chan struct{})
	go func() {
		announcer.AnnounceWithin(ctx, func(eventchan.Channel, any) { emitted++ }, ThreadActionFull, "t1")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the announcement outlived its context")
	}
	if emitted != 0 {
		t.Fatalf("emitted %d frames after the context ended, want 0", emitted)
	}
}

// A patch carries the stored value of each field it names, whatever its
// caller wrote, and leaves the fields it does not name unset.
func TestAnnouncePatchCarriesTheStoredFields(t *testing.T) {
	stored := store.Thread{ID: "t1", Title: "stored title", Model: "stored-model", SessionRef: "stored-ref"}
	announcer := NewThreadAnnouncer(func(string) (store.Thread, error) { return stored, nil })
	var got ThreadUpdateEvent
	emit := func(_ eventchan.Channel, payload any) { got = payload.(ThreadUpdateEvent) }
	title, model, ref := "caller title", "caller-model", "caller-ref"
	announcer.AnnouncePatch(emit, "t1", ThreadUpdateEvent{Title: &title, Model: &model, SessionRef: &ref})
	if got.Action != ThreadActionPatch || got.ID != "t1" || *got.Title != stored.Title || *got.Model != stored.Model || *got.SessionRef != stored.SessionRef {
		t.Fatalf("patch = %+v, want the stored fields", got)
	}
	announcer.AnnouncePatch(emit, "t1", ThreadUpdateEvent{Model: &model})
	if got.Title != nil || got.SessionRef != nil || *got.Model != stored.Model {
		t.Fatalf("model-only patch = %+v, want only the stored model", got)
	}
	at := int64(7)
	announcer = NewThreadAnnouncer(func(string) (store.Thread, error) {
		t.Fatal("an activity-only patch read the row")
		return store.Thread{}, nil
	})
	announcer.AnnouncePatch(emit, "t1", ThreadUpdateEvent{UpdatedAt: &at})
	if got.UpdatedAt == nil || *got.UpdatedAt != at {
		t.Fatalf("activity patch = %+v", got)
	}
}
