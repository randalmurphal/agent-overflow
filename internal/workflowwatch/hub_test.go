package workflowwatch

import (
	"testing"

	"agent-overflow/internal/workflow/engine"
)

func recordTransitions(hub *Hub, itemIDs ...string) {
	for index, itemID := range itemIDs {
		hub.Record(engine.StateEvent{
			ItemID: itemID, ProjectID: "project", PhaseID: "survey", Attempt: 1,
			From: engine.StateRunning, To: engine.StateNeedsHuman, Reason: engine.ReasonStuck,
		}, int64(index+1))
	}
}

func everything(string) bool { return true }

func TestHubNeverAnswersWithTheNoCursorSentinel(t *testing.T) {
	hub := &Hub{}
	transitions, head, gap := hub.Since(0, everything)
	if head == 0 {
		t.Fatal("an untouched hub answered with the sentinel as its head")
	}
	if len(transitions) != 0 || gap {
		t.Fatalf("an untouched hub reported %d transitions (gap=%t)", len(transitions), gap)
	}
	recordTransitions(hub, "run-1")
	transitions, next, gap := hub.Since(head, everything)
	if gap {
		t.Fatal("the first transition after an empty read reported a gap")
	}
	if len(transitions) != 1 || next == head {
		t.Fatalf("transitions = %#v, head %d -> %d", transitions, head, next)
	}
}

func TestHubReturnsOnlyWhatIsAfterTheCursorAndPassesTheFilter(t *testing.T) {
	hub := &Hub{}
	_, start, _ := hub.Since(0, everything)
	recordTransitions(hub, "run-1", "run-2", "run-1")
	all, head, _ := hub.Since(start, everything)
	if len(all) != 3 {
		t.Fatalf("transitions = %d, want every retained one", len(all))
	}
	mine, _, _ := hub.Since(start, func(itemID string) bool { return itemID == "run-1" })
	if len(mine) != 2 {
		t.Fatalf("filtered transitions = %#v, want only run-1's", mine)
	}
	if tail, _, _ := hub.Since(head, everything); len(tail) != 0 {
		t.Fatalf("a cursor at the head returned %#v", tail)
	}
	recordTransitions(hub, "run-2")
	tail, _, _ := hub.Since(head, everything)
	if len(tail) != 1 || tail[0].ItemID != "run-2" {
		t.Fatalf("tail = %#v, want just the new transition", tail)
	}
}

func TestHubGapsAnEvictedCursor(t *testing.T) {
	hub := &Hub{}
	ids := make([]string, maxRing+10)
	for index := range ids {
		ids[index] = "run-1"
	}
	_, start, _ := hub.Since(0, everything)
	recordTransitions(hub, ids...)
	transitions, head, gap := hub.Since(start, everything)
	if !gap {
		t.Fatal("a cursor older than the ring did not gap")
	}
	if len(transitions) != maxRing {
		t.Fatalf("retained %d transitions, want the ring's bound", len(transitions))
	}
	if head != int64(len(ids))+1 {
		t.Fatalf("head = %d, want the sequence to keep counting past the ring's bound", head)
	}
}

func TestHubGapsACursorFromAPreviousProcess(t *testing.T) {
	hub := &Hub{}
	recordTransitions(hub, "run-1")
	transitions, head, gap := hub.Since(9_000, everything)
	if !gap {
		t.Fatal("a cursor above the head did not gap")
	}
	if len(transitions) != 0 {
		t.Fatalf("an above-head cursor returned %#v", transitions)
	}
	if head >= 9_000 {
		t.Fatalf("head = %d, want this process's own sequence", head)
	}
}

func TestHubWakesEveryWaiter(t *testing.T) {
	hub := &Hub{}
	first, second := hub.Wait(), hub.Wait()
	if first != second {
		t.Fatal("two waiters took different channels, so one of them cannot be woken")
	}
	recordTransitions(hub, "run-1")
	select {
	case <-first:
	default:
		t.Fatal("a recorded transition did not wake the waiters")
	}
	if next := hub.Wait(); next == first {
		t.Fatal("a waiter after the wake took the already-closed channel")
	}
}
