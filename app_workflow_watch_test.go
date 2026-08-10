package main

import (
	"strings"
	"testing"

	"agent-overflow/internal/workflow/engine"
)

// The transition ring behind `run watch`, on its own. It is the one piece of
// this feature with no database and no scope: a bounded sequence, a gap rule at
// both ends, and a broadcast. Everything a watcher can be told wrong is decided
// here.

func recordTransitions(hub *workflowWatchHub, itemIDs ...string) {
	for index, itemID := range itemIDs {
		hub.record(engine.StateEvent{
			ItemID: itemID, ProjectID: "project", PhaseID: "survey", Attempt: 1,
			From: engine.StateRunning, To: engine.StateNeedsHuman, Reason: engine.ReasonStuck,
		}, int64(index+1))
	}
}

func everything(string) bool { return true }

// Zero is the caller's sentinel for "I hold no cursor", so it must never also be
// a head this hub returns — a watcher handed back the value it just sent would
// take the no-cursor path forever, blocking on nothing and polling in a loop.
func TestWorkflowWatchHubNeverAnswersWithTheNoCursorSentinel(t *testing.T) {
	hub := &workflowWatchHub{}
	transitions, head, gap := hub.since(0, everything)
	if head == 0 {
		t.Fatal("an untouched hub answered with the sentinel as its head")
	}
	if len(transitions) != 0 || gap {
		t.Fatalf("an untouched hub reported %d transitions (gap=%t)", len(transitions), gap)
	}
	// A cursor taken from that empty answer still sees the next transition.
	recordTransitions(hub, "run-1")
	transitions, next, gap := hub.since(head, everything)
	if gap {
		t.Fatal("the first transition after an empty read reported a gap")
	}
	if len(transitions) != 1 || next == head {
		t.Fatalf("transitions = %#v, head %d -> %d", transitions, head, next)
	}
}

func TestWorkflowWatchHubReturnsOnlyWhatIsAfterTheCursorAndPassesTheFilter(t *testing.T) {
	hub := &workflowWatchHub{}
	// The cursor a watcher that attached BEFORE anything happened would hold.
	_, start, _ := hub.since(0, everything)
	recordTransitions(hub, "run-1", "run-2", "run-1")
	all, head, _ := hub.since(start, everything)
	if len(all) != 3 {
		t.Fatalf("transitions = %d, want every retained one", len(all))
	}
	mine, _, _ := hub.since(start, func(itemID string) bool { return itemID == "run-1" })
	if len(mine) != 2 {
		t.Fatalf("filtered transitions = %#v, want only run-1's", mine)
	}
	if tail, _, _ := hub.since(head, everything); len(tail) != 0 {
		t.Fatalf("a cursor at the head returned %#v", tail)
	}
	recordTransitions(hub, "run-2")
	tail, _, _ := hub.since(head, everything)
	if len(tail) != 1 || tail[0].ItemID != "run-2" {
		t.Fatalf("tail = %#v, want just the new transition", tail)
	}
}

// A cursor that fell off the OLD end of the ring gaps: the transitions between
// it and what is retained are gone, and a watcher told "nothing happened" would
// report a run as idle through the very window it moved in.
func TestWorkflowWatchHubGapsAnEvictedCursor(t *testing.T) {
	hub := &workflowWatchHub{}
	ids := make([]string, maxWorkflowWatchRing+10)
	for index := range ids {
		ids[index] = "run-1"
	}
	_, start, _ := hub.since(0, everything)
	recordTransitions(hub, ids...)
	transitions, head, gap := hub.since(start, everything)
	if !gap {
		t.Fatal("a cursor older than the ring did not gap")
	}
	if len(transitions) != maxWorkflowWatchRing {
		t.Fatalf("retained %d transitions, want the ring's bound", len(transitions))
	}
	if head != int64(len(ids))+1 {
		t.Fatalf("head = %d, want the sequence to keep counting past the ring's bound", head)
	}
}

// A cursor ABOVE the head gaps too. It is not hypothetical: the ring lives in
// the process, so a restarted backend re-seeds it while the watcher still holds
// a sequence from the previous life — and answering "nothing missed" would leave
// it blocked on a number this process will take a campaign to reach.
func TestWorkflowWatchHubGapsACursorFromAPreviousProcess(t *testing.T) {
	hub := &workflowWatchHub{}
	recordTransitions(hub, "run-1")
	transitions, head, gap := hub.since(9_000, everything)
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

// The broadcast is what makes the long poll a poll of ONE call: a waiter takes
// the channel before its scan, so a transition landing in between wakes it
// rather than being missed until the hold expires.
func TestWorkflowWatchHubWakesEveryWaiter(t *testing.T) {
	hub := &workflowWatchHub{}
	first, second := hub.wait(), hub.wait()
	if first != second {
		t.Fatal("two waiters took different channels, so one of them cannot be woken")
	}
	recordTransitions(hub, "run-1")
	select {
	case <-first:
	default:
		t.Fatal("a recorded transition did not wake the waiters")
	}
	// The next waiter gets a fresh channel rather than the closed one.
	if next := hub.wait(); next == first {
		t.Fatal("a waiter after the wake took the already-closed channel")
	}
}

// The two sentences the verb ends on. They differ because a bare resume of a
// repaired-in-place park does NOT start an attempt that reads the row, and an
// operator told otherwise would resume, watch the old value run, and conclude
// the amendment did nothing.
func TestWorkflowAmendmentNoteNamesWhatMakesTheValueTake(t *testing.T) {
	fresh := workflowAmendmentNote(engine.SeedAmendment{
		ItemID: "run-1", PhaseID: "wave", Effect: engine.SeedEffectFreshEntry,
	})
	if !strings.Contains(fresh, "run resume run-1 --phase wave") {
		t.Fatalf("fresh-entry note = %q, want the verb that enters the phase now", fresh)
	}
	next := workflowAmendmentNote(engine.SeedAmendment{
		ItemID: "run-1", PhaseID: "wave", Effect: engine.SeedEffectNextAttempt,
	})
	if strings.Contains(next, "--phase") {
		t.Fatalf("next-attempt note = %q, want it not to demand a phase be re-entered", next)
	}
	if !strings.Contains(next, "next attempt") {
		t.Fatalf("next-attempt note = %q", next)
	}
}
