package main

import (
	"sync"
	"time"

	"agent-overflow/internal/workflow/engine"
)

// The run-transition ring behind `agent-overflow run watch` (D53).
//
// The CLI holds a scoped HTTP token and no WebSocket, so a watcher cannot
// subscribe to the event bus the frontend reads. What it gets instead is a
// LONG POLL: one bound method that blocks server-side until the run tree it
// names moves, and returns the transitions it missed. The blocking is the whole
// point — the alternative this replaces is a supervising agent hand-rolling a
// sleep loop and running 553 status polls over 9.5 hours, one of which died
// silently.
//
// This file is the small piece of coordination that makes a blocking read
// possible: an in-memory, bounded, sequence-numbered ring of the item-state
// transitions the engine emitted, plus a broadcast every waiter parks on. It is
// a jitter buffer, not a history store — exactly what `internal/transport`'s
// replay ring is, and for the same reason (root CLAUDE.md principle 3). A
// cursor that falls outside it is answered with a GAP, never with silence, so a
// watcher can tell "nothing happened" from "you missed something"; and every
// answer carries the run's CURRENT state read from SQLite, which is what the
// ring is never asked to reconstruct.

// maxWorkflowWatchRing bounds the retained transitions. A transition costs a
// provider turn, a human decision, or a call boundary, so a campaign produces
// them seconds-to-minutes apart; a watcher re-establishes its poll at least
// every maxWorkflowWatchHold. This depth covers both by orders of magnitude,
// and a cursor that still falls off the end gaps rather than lying.
const maxWorkflowWatchRing = 512

// workflowTransition is one item-state transition, as the engine emitted it,
// with the sequence and wall clock the watcher orders and prints by. The park
// cause is deliberately absent: reading it is a SQLite read, and this record is
// written on the engine's command goroutine, which may not block. It is
// resolved from the coordinate below when a watch call answers.
type workflowTransition struct {
	Seq       int64
	At        int64
	ItemID    string
	ProjectID string
	PhaseID   string
	Attempt   int
	From      string
	To        string
	Reason    string
}

// workflowWatchHub is the ring plus its broadcast. The zero value is usable: an
// App that never starts a workflow engine still answers "nothing has happened",
// which is the truth rather than a nil dereference.
type workflowWatchHub struct {
	mu      sync.Mutex
	seq     int64
	entries []workflowTransition
	// changed is closed (and replaced) on every record. A waiter takes the
	// current channel under the lock and selects on it, so a transition landing
	// between the scan and the wait wakes it immediately rather than after the
	// hold expires.
	changed chan struct{}
}

// seedLocked puts the sequence at 1 before anything is recorded, so the first
// transition is seq 2 and ZERO is never a head this hub returns.
//
// That matters because zero is the caller's sentinel for "I hold no cursor". A
// hub that answered a fresh watcher with head 0 would hand it back the very
// value it just sent, so its next call would take the no-cursor path again — a
// watch that never establishes a position, never blocks, and spins against the
// app. Which is the polling loop this verb exists to delete.
func (h *workflowWatchHub) seedLocked() {
	if h.seq == 0 {
		h.seq = 1
	}
}

// record appends one transition and wakes every waiter. It runs on the engine's
// command-loop goroutine, so it does no I/O and takes the lock for exactly as
// long as an append.
func (h *workflowWatchHub) record(event engine.StateEvent, at int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seedLocked()
	h.seq++
	h.entries = append(h.entries, workflowTransition{
		Seq: h.seq, At: at, ItemID: event.ItemID, ProjectID: event.ProjectID,
		PhaseID: event.PhaseID, Attempt: event.Attempt,
		From: string(event.From), To: string(event.To), Reason: string(event.Reason),
	})
	if len(h.entries) > maxWorkflowWatchRing {
		h.entries = append(h.entries[:0], h.entries[len(h.entries)-maxWorkflowWatchRing:]...)
	}
	if h.changed != nil {
		close(h.changed)
		h.changed = nil
	}
}

// since returns the retained transitions after cursor whose run the filter
// admits, the current head, and whether anything between the two was lost.
//
// A cursor ABOVE the head gaps too, and that case is not hypothetical: the ring
// lives in the process, so a backend that restarted re-seeds it from zero while
// the watcher still holds a sequence from the previous life. Answering "nothing
// missed" there would leave it blocked forever on a number this process will
// take minutes to reach.
func (h *workflowWatchHub) since(cursor int64, watched func(itemID string) bool) ([]workflowTransition, int64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seedLocked()
	// A caller holding no position is asking what happens NEXT. It gets the head
	// and nothing else: no backlog, because replaying a campaign's retained
	// history into an agent's context is the opposite of what was asked, and no
	// gap, because a watcher that was not watching missed nothing.
	if cursor <= 0 {
		return nil, h.seq, false
	}
	if cursor > h.seq {
		return nil, h.seq, true
	}
	gap := false
	if len(h.entries) > 0 && cursor < h.entries[0].Seq-1 {
		gap = true
	} else if len(h.entries) == 0 && cursor < h.seq {
		gap = true
	}
	var matched []workflowTransition
	for _, entry := range h.entries {
		if entry.Seq <= cursor {
			continue
		}
		if watched != nil && !watched(entry.ItemID) {
			continue
		}
		matched = append(matched, entry)
	}
	return matched, h.seq, gap
}

// wait returns the channel that closes on the next recorded transition. It is
// taken before the caller's final scan is acted on, so the caller cannot miss a
// transition that lands between the two.
func (h *workflowWatchHub) wait() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.changed == nil {
		h.changed = make(chan struct{})
	}
	return h.changed
}

// recordWorkflowTransition is the App-side entry point, called from the one
// item-state listener so a watcher and a wake are fed by the same event.
//
// Unlike the wake, it records transitions whose from and to are EQUAL: a
// takeover parks an already-parked run under a new reason, and a monitor that
// dropped it would report a run as still waiting on the thing it is no longer
// waiting on.
func (a *App) recordWorkflowTransition(event engine.StateEvent) {
	a.workflowWatch.record(event, time.Now().UnixMilli())
}
