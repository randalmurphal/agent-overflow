package triage

import (
	"context"
	"errors"
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
	}, nil)
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

// A handoff frame is the old thread's row read under its announce lock,
// unlisted and naming the successor: a write that lands while another frame
// holds the lock is in it.
func TestAnnounceHandOffReadsTheRowUnderTheLock(t *testing.T) {
	stored := store.Thread{ID: "old", Title: "before"}
	announcer := NewThreadAnnouncer(func(string) (store.Thread, error) { return stored, nil }, nil)
	release := announcer.Hold("old")
	frames := make(chan ThreadUpdateEvent, 1)
	go announcer.AnnounceHandOff(func(_ eventchan.Channel, payload any) { frames <- payload.(ThreadUpdateEvent) }, "old", "next")
	for announcer.Refs("old") < 2 {
		time.Sleep(time.Millisecond)
	}
	stored.Title = "renamed meanwhile"
	stored.Archived = true
	release()
	var got ThreadUpdateEvent
	select {
	case got = <-frames:
	case <-time.After(10 * time.Second):
		t.Fatal("no handoff frame")
	}
	if got.Action != ThreadActionUnlisted || got.HandOffTo != "next" || got.Thread == nil ||
		got.Thread.ID != "old" || got.Thread.Title != "renamed meanwhile" || !got.Thread.Archived {
		t.Fatalf("handoff frame = %+v (row %+v), want the row read after the lock, unlisted, handing off to next", got, got.Thread)
	}

	// Unarchived since the handoff: the row stays listed, and the frame
	// still names the successor.
	stored.Archived = false
	announcer.AnnounceHandOff(func(_ eventchan.Channel, payload any) { got = payload.(ThreadUpdateEvent) }, "old", "next")
	if got.Action != ThreadActionFull || got.HandOffTo != "next" || got.Thread == nil || got.Thread.Archived {
		t.Fatalf("handoff frame of an unarchived row = %+v, want full, handing off to next", got)
	}
}

// A placement frame follows the sidebar listing it reads: `listed` for a
// row the sidebar lists, `full` for an owned, archived or never-sent one,
// and never the archive's `unlisted`, which closes the panes showing the
// thread. A failed listing read answers from the row's placement.
func TestAnnouncePlacedFollowsTheListing(t *testing.T) {
	unread := errors.New("listing unreadable")
	for _, tc := range []struct {
		name      string
		placement string
		archived  bool
		listed    bool
		listErr   error
		want      string
	}{
		{"standalone", store.PlacementStandalone, false, true, nil, ThreadActionListed},
		{"grouped", store.PlacementGrouped, false, true, nil, ThreadActionListed},
		{"never-sent standalone", store.PlacementStandalone, false, false, nil, ThreadActionFull},
		{"owned", store.PlacementOwned, false, false, nil, ThreadActionFull},
		{"archived standalone", store.PlacementStandalone, true, false, nil, ThreadActionFull},
		{"archived owned", store.PlacementOwned, true, false, nil, ThreadActionFull},
		{"unread standalone", store.PlacementStandalone, false, false, unread, ThreadActionListed},
		{"unread owned", store.PlacementOwned, false, false, unread, ThreadActionFull},
		{"unread archived", store.PlacementStandalone, true, false, unread, ThreadActionFull},
	} {
		stored := store.Thread{ID: "worker", Placement: tc.placement, Archived: tc.archived}
		announcer := NewThreadAnnouncer(func(string) (store.Thread, error) { return stored, nil },
			func(string) (bool, error) { return tc.listed, tc.listErr })
		var got []ThreadUpdateEvent
		announcer.AnnouncePlaced(func(_ eventchan.Channel, payload any) { got = append(got, payload.(ThreadUpdateEvent)) }, "worker")
		if len(got) != 1 || got[0].Action != tc.want || got[0].Thread == nil || got[0].Thread.Placement != tc.placement {
			t.Errorf("%s: frames = %+v, want one %s frame carrying the row", tc.name, got, tc.want)
		}
	}
}

// A patch carries the stored value of each field it names, whatever its
// caller wrote, and leaves the fields it does not name unset.
func TestAnnouncePatchCarriesTheStoredFields(t *testing.T) {
	stored := store.Thread{ID: "t1", Title: "stored title", Model: "stored-model", SessionRef: "stored-ref"}
	announcer := NewThreadAnnouncer(func(string) (store.Thread, error) { return stored, nil }, nil)
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
	}, nil)
	announcer.AnnouncePatch(emit, "t1", ThreadUpdateEvent{UpdatedAt: &at})
	if got.UpdatedAt == nil || *got.UpdatedAt != at {
		t.Fatalf("activity patch = %+v", got)
	}
}
