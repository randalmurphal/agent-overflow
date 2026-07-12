package wsllauncher

import (
	"testing"

	"agent-overflow/internal/notify"
)

func TestNotificationActivationQueueColdStartAndFIFO(t *testing.T) {
	queue := NewNotificationActivationQueue()
	first := notify.Target{Kind: "thread", ThreadID: "first"}
	second := notify.Target{Kind: "thread", ThreadID: "second"}

	if dropped, start := queue.Push(first, false); dropped != nil || start {
		t.Fatalf("cold push = (dropped=%v, start=%v), want nil, false", dropped, start)
	}
	if _, start := queue.Push(second, false); start {
		t.Fatal("second cold push unexpectedly started drain")
	}
	if !queue.StartIfPending(true) {
		t.Fatal("backend readiness did not start pending drain")
	}
	if queue.StartIfPending(true) {
		t.Fatal("already-active drain started twice")
	}

	if got, ok := queue.Next(); !ok || got != first {
		t.Fatalf("first Next = (%#v, %v), want %#v, true", got, ok, first)
	}
	// Arrival during a drain remains behind the already-queued click and does
	// not start a competing drain.
	third := notify.Target{Kind: "thread", ThreadID: "third"}
	if _, start := queue.Push(third, true); start {
		t.Fatal("arrival during drain started a competing drain")
	}
	for _, want := range []notify.Target{second, third} {
		if got, ok := queue.Next(); !ok || got != want {
			t.Fatalf("Next = (%#v, %v), want %#v, true", got, ok, want)
		}
	}
	if _, ok := queue.Next(); ok {
		t.Fatal("empty queue returned an item")
	}
}

func TestNotificationActivationQueueDropsOldestAndRestartsAfterStop(t *testing.T) {
	queue := NewNotificationActivationQueue()
	for i := 0; i < NotificationActivationQueueCapacity; i++ {
		queue.Push(notify.Target{Kind: "thread", ThreadID: string(rune('a' + i))}, false)
	}
	dropped, start := queue.Push(notify.Target{Kind: "thread", ThreadID: "newest"}, true)
	if dropped == nil || dropped.ThreadID != "a" {
		t.Fatalf("dropped = %#v, want oldest target a", dropped)
	}
	if !start {
		t.Fatal("ready overflow push did not start drain")
	}
	queue.Stop()
	if !queue.StartIfPending(true) {
		t.Fatal("stopped drain with pending clicks did not restart")
	}
	if got, ok := queue.Next(); !ok || got.ThreadID != "b" {
		t.Fatalf("first retained target = (%#v, %v), want b", got, ok)
	}
}
