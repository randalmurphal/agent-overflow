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
