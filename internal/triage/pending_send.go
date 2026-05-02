package triage

import "time"

// pending_send.go owns the pending-send correlation state used by the
// triage router. The send path registers an entry when an AO-initiated
// user message is dispatched to a provider; the matching wire
// EventUserText pops the FIFO head once Phase E lands the consumer
// path. wireOnlyUserTextSeen dedupes replay-or-self-prompt envelopes
// that don't correspond to any pending AO send.
//
// All state lives on Router (router.go). The methods here use r.mu —
// the existing Router mutex — and never introduce a separate lock; the
// pending-send maps share the same critical section as every other
// per-thread correlation map so a CleanupThread sweep is consistent
// across all of them.

// pendingSend records an AO-initiated user message awaiting wire
// confirmation. FIFO per thread; consumed when the matching wire
// EventUserText arrives. Bounded by user attention (typically 0-1
// entries per thread); swept at CleanupThread as a safety net.
type pendingSend struct {
	AOItemID   string // "user:<turnIndex>"
	TurnIndex  int
	EnqueuedAt int64
}

// RegisterPendingSend appends a new entry to the per-thread FIFO. The
// send path calls this synchronously so the queue mirrors user-typed
// order. EnqueuedAt is stamped from time.Now here — Phase E uses it
// for diagnostics on stranded entries and the wall clock at the
// register call is the natural reference.
func (r *Router) RegisterPendingSend(threadID, aoItemID string, turnIndex int) {
	if threadID == "" || aoItemID == "" {
		return
	}
	r.mu.Lock()
	r.pendingByThread[threadID] = append(r.pendingByThread[threadID], pendingSend{
		AOItemID:   aoItemID,
		TurnIndex:  turnIndex,
		EnqueuedAt: time.Now().UnixMilli(),
	})
	r.mu.Unlock()
}

// HasPendingSendForThread reports whether the FIFO holds at least one
// entry for threadID. Used by handleInit's Phase F gate to decide
// whether an incoming wire init corresponds to an AO send still
// awaiting confirmation. Read-only — does not mutate state. Exported
// so the send-failure cleanup path in app_send.go (and tests in
// package main) can verify the marker is in the expected state.
func (r *Router) HasPendingSendForThread(threadID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pendingByThread[threadID]) > 0
}

// consumePendingSendHead pops and returns the FIFO head for threadID.
// Returns (zero, false) when no entry exists. Pure FIFO — Phase E
// pairs this with the wire EventUserText that triggers it.
func (r *Router) consumePendingSendHead(threadID string) (pendingSend, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	queue := r.pendingByThread[threadID]
	if len(queue) == 0 {
		return pendingSend{}, false
	}
	head := queue[0]
	rest := queue[1:]
	if len(rest) == 0 {
		delete(r.pendingByThread, threadID)
	} else {
		// Copy into a fresh slice so we don't pin the previous
		// backing array (which still references the popped head)
		// in the map. Pending-send queues are tiny, so the
		// allocation is cheap and the GC behavior is obvious.
		next := make([]pendingSend, len(rest))
		copy(next, rest)
		r.pendingByThread[threadID] = next
	}
	return head, true
}

// ClearPendingSendForFailure removes a single matching entry from the
// FIFO. Distinct from consumePendingSendHead because failure recovery
// is not necessarily head-of-queue: a later send may be the one that
// failed (e.g. a provider write returned an error after queueing a
// follow-up). Preserves relative order of surviving entries. Exported
// because the send path in `app_send.go` (package main) calls this
// when sendToProvider fails — without it, the marker stays live and a
// later wire init would mis-route the next AO send onto a dead turn.
func (r *Router) ClearPendingSendForFailure(threadID, aoItemID string) {
	if threadID == "" || aoItemID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	queue := r.pendingByThread[threadID]
	if len(queue) == 0 {
		return
	}
	for i, entry := range queue {
		if entry.AOItemID != aoItemID {
			continue
		}
		queue = append(queue[:i], queue[i+1:]...)
		if len(queue) == 0 {
			delete(r.pendingByThread, threadID)
		} else {
			r.pendingByThread[threadID] = queue
		}
		return
	}
}

// clearPendingSendsForThread sweeps every entry for threadID. Called
// by CleanupThread on session teardown. Acquires r.mu — for the
// CleanupThread codepath that already holds r.mu, see
// clearPendingSendsLocked.
func (r *Router) clearPendingSendsForThread(threadID string) {
	r.mu.Lock()
	r.clearPendingSendsLocked(threadID)
	r.mu.Unlock()
}

// clearPendingSendsLocked is the no-lock variant of
// clearPendingSendsForThread. Caller MUST hold r.mu. Used by
// CleanupThread to keep the entire teardown inside one critical
// section so a concurrent Handle observes a consistent snapshot.
func (r *Router) clearPendingSendsLocked(threadID string) {
	delete(r.pendingByThread, threadID)
}

// markWireOnlyUserTextSeen records a wire EventUserText whose
// providerItemID has no pending AO send to match. Returns true on the
// first sighting (caller may persist a "wire-only" timeline row),
// false on duplicates so a session-resume replay doesn't double-write.
func (r *Router) markWireOnlyUserTextSeen(threadID, providerItemID string) bool {
	if threadID == "" || providerItemID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	seen, ok := r.wireOnlyUserTextSeen[threadID]
	if !ok {
		seen = make(map[string]struct{})
		r.wireOnlyUserTextSeen[threadID] = seen
	}
	if _, dup := seen[providerItemID]; dup {
		return false
	}
	seen[providerItemID] = struct{}{}
	return true
}

// clearWireOnlyUserTextForThread sweeps the dedup set for threadID.
// Called by CleanupThread on session teardown. Acquires r.mu — for
// the CleanupThread codepath that already holds r.mu, see
// clearWireOnlyUserTextLocked.
func (r *Router) clearWireOnlyUserTextForThread(threadID string) {
	r.mu.Lock()
	r.clearWireOnlyUserTextLocked(threadID)
	r.mu.Unlock()
}

// clearWireOnlyUserTextLocked is the no-lock variant of
// clearWireOnlyUserTextForThread. Caller MUST hold r.mu.
func (r *Router) clearWireOnlyUserTextLocked(threadID string) {
	delete(r.wireOnlyUserTextSeen, threadID)
}
