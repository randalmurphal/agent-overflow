package harnessclient

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The wait-consumption rules, the parked-wait handle, and the two
// timeout-path properties a lost event hides behind. They live apart from
// client_test.go because none of them needs the connection to do anything
// beyond deliver a frame.

func TestATimedOutWaitConsumesNothing(t *testing.T) {
	backend := newFakeBackend(t)
	client := backend.dial(t)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := client.WaitForEvent(ctx, "provider:turn_completed", nil); err == nil {
		t.Fatal("a wait for an unemitted channel succeeded")
	}

	// The abandoned waiter must be GONE. Left registered, it wins the next
	// dispatch, marks the log entry consumed, and parks the event in a
	// channel nobody holds — so the event arrived, is not in the log as
	// available, and no caller can ever see it.
	backend.pushEvent(t, "provider:turn_completed", map[string]any{"turn": 1})
	event, err := client.WaitForEvent(testContext(t), "provider:turn_completed", nil)
	if err != nil {
		t.Fatalf("the timed-out wait swallowed the next event: %v", err)
	}
	if !strings.Contains(string(event.Data), "\"turn\":1") {
		t.Fatalf("event data = %s", event.Data)
	}
}

func TestAnEventArrivingInTheTimeoutRaceWindowIsStillReturned(t *testing.T) {
	backend := newFakeBackend(t)
	client := backend.dial(t)

	// Both select arms ready at once is exactly the window: dispatch has
	// handed the event over (under c.mu, with the log entry already marked
	// consumed) and the deadline expires before Wait runs. Go picks a ready
	// case at random, so this is the shape that fails intermittently rather
	// than never — repeat it enough that both arms are taken.
	for i := 0; i < 200; i++ {
		a := client.Await("harness:mock", nil)
		a.w.out <- Event{Channel: "harness:mock", Seq: uint64(i + 1)}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		event, err := a.Wait(ctx)
		if err != nil {
			t.Fatalf("iteration %d: a delivered event was reported as a timeout: %v", i, err)
		}
		if event.Seq != uint64(i+1) {
			t.Fatalf("iteration %d: got seq %d", i, event.Seq)
		}
	}
}

func TestAwaitParksBeforeTheCallerCausesTheEvent(t *testing.T) {
	backend := newFakeBackend(t)
	client := backend.dial(t)

	// The `send --wait` shape: park, THEN do the thing that produces the
	// event. A backend that answers inside the round trip must not be able
	// to slip the completion past the caller.
	awaiting := client.Await("provider:turn_completed", func(ev Event) bool {
		return strings.Contains(string(ev.Data), "t-1")
	})
	backend.pushEvent(t, "provider:turn_completed", map[string]any{"threadId": "t-1"})

	event, err := awaiting.Wait(testContext(t))
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !strings.Contains(string(event.Data), "t-1") {
		t.Fatalf("event data = %s", event.Data)
	}
}

func TestClosingAnAwaitReleasesItsWaiterSlot(t *testing.T) {
	backend := newFakeBackend(t)
	client := backend.dial(t)

	client.Await("harness:mock", nil).Close()

	client.mu.Lock()
	parked := len(client.waiters)
	client.mu.Unlock()
	if parked != 0 {
		t.Fatalf("%d waiter(s) still parked after Close; an abandoned wait leaks for the life of the connection", parked)
	}
}

func TestWaitNewestPicksTheLatestRetainedMatch(t *testing.T) {
	backend := newFakeBackend(t)
	client := backend.dial(t)
	backend.pushEvent(t, "provider:turn_completed", map[string]any{"turn": 1})
	backend.pushEvent(t, "provider:turn_completed", map[string]any{"turn": 2})
	backend.pushEvent(t, "provider:turn_completed", map[string]any{"turn": 3})
	waitForCount(t, client, "provider:turn_completed", 3)

	event, err := client.WaitForEventOpts(testContext(t), "provider:turn_completed", WaitOptions{Newest: true}, nil)
	if err != nil {
		t.Fatalf("WaitForEventOpts: %v", err)
	}
	if !strings.Contains(string(event.Data), "\"turn\":3") {
		t.Fatalf("newest-first settled on %s", event.Data)
	}
}

func TestWaitMinSeqIgnoresEverythingUpToTheFloor(t *testing.T) {
	backend := newFakeBackend(t)
	client := backend.dial(t)
	backend.pushEvent(t, "harness:mock", map[string]any{"n": 1})
	backend.pushEvent(t, "harness:mock", map[string]any{"n": 2})
	waitForCount(t, client, "harness:mock", 2)

	events := client.Events()
	floor := events[0].Seq

	event, err := client.WaitForEventOpts(testContext(t), "harness:mock", WaitOptions{MinSeq: floor}, nil)
	if err != nil {
		t.Fatalf("WaitForEventOpts: %v", err)
	}
	if event.Seq <= floor {
		t.Fatalf("seq %d is at or below the floor %d", event.Seq, floor)
	}
}

func TestWaitSkipHistoryOnlySeesLaterEvents(t *testing.T) {
	backend := newFakeBackend(t)
	client := backend.dial(t)
	backend.pushEvent(t, "harness:mock", map[string]any{"n": "old"})
	waitForCount(t, client, "harness:mock", 1)

	go func() {
		time.Sleep(30 * time.Millisecond)
		backend.pushEvent(t, "harness:mock", map[string]any{"n": "new"})
	}()

	event, err := client.WaitForEventOpts(testContext(t), "harness:mock", WaitOptions{SkipHistory: true}, nil)
	if err != nil {
		t.Fatalf("WaitForEventOpts: %v", err)
	}
	if !strings.Contains(string(event.Data), "new") {
		t.Fatalf("SkipHistory settled on a retained event: %s", event.Data)
	}
}

func TestAwaitOnAClosedClientAnswersInsteadOfBlocking(t *testing.T) {
	backend := newFakeBackend(t)
	client := backend.dial(t)
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := client.Await("harness:mock", nil).Wait(testContext(t))
	if err == nil {
		t.Fatal("a wait parked on a closed client succeeded")
	}
}
