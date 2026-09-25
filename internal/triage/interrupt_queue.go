package triage

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"agent-overflow/internal/store"
)

// The interrupt queue (invariant 11). A row created while a stream in its
// own scope is open waits here until that scope's streams settle, so it
// cannot take an item_index below the still-streaming tail. Each scope
// drains on its own: a main-scope row does not wait behind an agent's
// stream, and an agent's rows neither wait for nor end with a turn
// (docs/architecture/turn-lifecycle.md, §Agent-owned rows).
//
// Every drain holds the thread's drain lock across its pop and persist.
// Persisting an agent's end settles that agent's streams and drains the
// rows queued behind them inside the same hold (persistAgentEndLocked),
// so it never takes the lock again.

type queuedPersistence struct {
	item    store.Item
	payload *store.Payload
	// end is set on an agent's completion sibling: persisting it ends the
	// agent.
	end *agentEnd
}

// maybeDeferOrPersist enforces invariant 11: a NEW row created mid-stream
// must defer its item_index until the stream it interrupts settles, so it
// can't render above the still-streaming tail (which took its lower index
// at segment start). The defer is SAME-scope only, keyed on the item's
// ParentID: a main-scope completion that deferred behind a concurrent
// subagent-scope stream drained past later main text.
func (r *Router) maybeDeferOrPersist(threadID string, item store.Item, payload *store.Payload) error {
	return r.deferOrPersist(threadID, queuedPersistence{item: item, payload: payload})
}

// deferOrPersist queues queued behind its scope's open stream, or
// persists it now.
func (r *Router) deferOrPersist(threadID string, queued queuedPersistence) error {
	r.mu.Lock()
	if st := r.threadStateIfPresent(threadID); st != nil && st.streamingScopeCounts[queued.item.ParentID] > 0 {
		st.interruptQueue = append(st.interruptQueue, queued)
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	if queued.end == nil {
		return r.persistItem(queued.item, queued.payload)
	}
	lock := r.drainLock(threadID)
	lock.Lock()
	defer lock.Unlock()
	return r.persistAgentEndLocked(queued.item, queued.payload, *queued.end)
}

// hasQueuedInterruptItems reports whether any deferred persists are
// queued for threadID. The promoted-echo boundary path uses it to
// decide whether a drain (and a re-bump of the promoted row) is needed
// before sampling the turn's max item_index (round-6, R6-2).
func (r *Router) hasQueuedInterruptItems(threadID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	return st != nil && len(st.interruptQueue) > 0
}

// drainInterruptQueueIfIdle persists, in order, the queued rows whose
// scope has no open stream. Each failed row is logged by the drain, which
// has no caller to return it to here.
func (r *Router) drainInterruptQueueIfIdle(threadID string) {
	lock := r.drainLock(threadID)
	lock.Lock()
	defer lock.Unlock()
	_ = r.drainQueueLocked(threadID, idleScopeRow)
}

// idleScopeRow drains a row once its scope has no open stream.
func idleScopeRow(st *threadState, queued queuedPersistence) (bool, bool) {
	return st.streamingScopeCounts[queued.item.ParentID] == 0, false
}

// drainLock returns threadID's drain mutex, creating its identity record
// on first use. Identity records are never deleted (see the
// threadIdentity.drainLock doc).
func (r *Router) drainLock(threadID string) *sync.Mutex {
	return &r.identity(threadID).drainLock
}

// drainTurnQueue persists the queued rows of a turn that is ending: every
// row no agent owns, whatever still streams, since the turn's streams end
// with it. A truncated turn's rows are errored with the interrupted
// suffix (interruptedSummary), except a row that reports background work
// (reportsBackgroundOutcome), whose status is the work's own. A row an agent owns is the agent's, not the
// turn's: it waits for its own scope and is never errored here.
// agentScopes is agentOwnedStreamScopes' answer for the turn's end.
func (r *Router) drainTurnQueue(threadID string, agentScopes map[string]bool, truncated bool) error {
	lock := r.drainLock(threadID)
	lock.Lock()
	defer lock.Unlock()
	return r.drainQueueLocked(threadID, func(st *threadState, queued queuedPersistence) (bool, bool) {
		if agentScopes[queued.item.ParentID] {
			return st.streamingScopeCounts[queued.item.ParentID] == 0, false
		}
		return true, truncated && !reportsBackgroundOutcome(queued.item)
	})
}

// drainAllLocked persists every queued row as it was queued; the caller
// holds the drain lock. The promoted-echo boundary uses it to commit every
// row queued before the boundary it samples.
func (r *Router) drainAllLocked(threadID string) error {
	return r.drainQueueLocked(threadID, func(*threadState, queuedPersistence) (bool, bool) {
		return true, false
	})
}

// drainAll persists every queued row as it was queued. A session's end
// runs it before the thread's state goes: no stream will settle any more.
func (r *Router) drainAll(threadID string) error {
	lock := r.drainLock(threadID)
	lock.Lock()
	defer lock.Unlock()
	return r.drainAllLocked(threadID)
}

// reportsBackgroundOutcome reports a row that records background work
// (a completion sibling or its notification) rather than the turn's own
// output, so a turn's interruption does not rewrite it.
func reportsBackgroundOutcome(item store.Item) bool {
	return item.Kind == itemKindBackgroundDone || item.Kind == itemKindNotification
}

// drainQueueLocked persists, in queue order, the rows decide marks ready
// (erroring the ones it marks errored) and keeps the rest queued in
// order. It repeats until a pass finds nothing ready: persisting an
// agent's end settles streams, which can make more rows ready. The caller
// holds the drain lock. Every row is attempted; each failure is logged
// and all of them are returned.
func (r *Router) drainQueueLocked(threadID string, decide func(*threadState, queuedPersistence) (ready, errored bool)) error {
	type drained struct {
		queued  queuedPersistence
		errored bool
	}
	var errs []error
	for {
		var batch []drained
		r.mu.Lock()
		if st := r.threadStateIfPresent(threadID); st != nil && len(st.interruptQueue) > 0 {
			var kept []queuedPersistence
			for _, queued := range st.interruptQueue {
				ready, errored := decide(st, queued)
				if ready {
					batch = append(batch, drained{queued: queued, errored: errored})
				} else {
					kept = append(kept, queued)
				}
			}
			st.interruptQueue = kept
		}
		r.mu.Unlock()
		if len(batch) == 0 {
			return errors.Join(errs...)
		}
		for _, next := range batch {
			item := next.queued.item
			if next.errored {
				item.Status = statusErrored
				item.Summary = interruptedSummary(item.Summary)
				item.UpdatedAt = time.Now().UnixMilli()
			}
			var err error
			if next.queued.end != nil {
				err = r.persistAgentEndLocked(item, next.queued.payload, *next.queued.end)
			} else {
				err = r.persistItem(item, next.queued.payload)
			}
			if err != nil {
				log.Printf("triage: drain persist failed for item %s on thread %s: %v", item.ID, threadID, err)
				errs = append(errs, fmt.Errorf("persist queued item %s: %w", item.ID, err))
			}
		}
	}
}
