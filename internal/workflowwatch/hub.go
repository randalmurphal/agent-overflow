package workflowwatch

import (
	"sync"

	"agent-overflow/internal/workflow/engine"
)

// maxRing bounds the retained transitions. A cursor that falls outside this
// window receives a gap instead of a false "nothing happened" answer.
const maxRing = 512

// Transition is one item-state transition as the engine emitted it, annotated
// with the process-local sequence and wall clock used by a long poll.
type Transition struct {
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

// Hub is a bounded transition ring plus a broadcast channel. Its zero value is
// ready for use, including in an App that never starts a workflow engine.
type Hub struct {
	mu      sync.Mutex
	seq     int64
	entries []Transition
	changed chan struct{}
}

// seedLocked keeps zero reserved as the caller's "no cursor" sentinel.
func (h *Hub) seedLocked() {
	if h.seq == 0 {
		h.seq = 1
	}
}

// Record appends one transition and wakes every waiter. It performs no I/O so
// callers may use it directly from the engine's command-loop event path.
func (h *Hub) Record(event engine.StateEvent, at int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seedLocked()
	h.seq++
	h.entries = append(h.entries, Transition{
		Seq: h.seq, At: at, ItemID: event.ItemID, ProjectID: event.ProjectID,
		PhaseID: event.PhaseID, Attempt: event.Attempt,
		From: string(event.From), To: string(event.To), Reason: string(event.Reason),
	})
	if len(h.entries) > maxRing {
		h.entries = append(h.entries[:0], h.entries[len(h.entries)-maxRing:]...)
	}
	if h.changed != nil {
		close(h.changed)
		h.changed = nil
	}
}

// Since returns retained transitions after cursor admitted by watched, the
// current head, and whether the caller lost anything before that head.
func (h *Hub) Since(cursor int64, watched func(itemID string) bool) ([]Transition, int64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seedLocked()
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
	var matched []Transition
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

// Wait returns a channel that closes when the next transition is recorded.
func (h *Hub) Wait() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.changed == nil {
		h.changed = make(chan struct{})
	}
	return h.changed
}
