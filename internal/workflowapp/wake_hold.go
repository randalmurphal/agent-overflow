package workflowapp

import (
	"sync"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/engine"
)

// Wake holds: a tool call that waits on a run hands the run's rest to the
// agent itself, so that rest must not also wake the thread
// (docs/specs/agent-workflow-tools.md, "Hearing back"). While a call holds a
// run, a rest of it that would wake the caller's thread is kept back. When the
// call ends it either delivered the rest it read, which is recorded as the
// bound thread's last delivered wake so the coalescing rule suppresses the
// same rest arriving later, or it did not, and the rest it kept back wakes the
// thread as it would have.
//
// Which rest is which is decided by the watch ring, not by what a rest looks
// like: the engine records every transition in the ring before its wake job
// is queued, and a waiting call reads the run's state after the ring head it
// returns. A call delivered the run's current rest only if the ring holds no
// transition of the run after the cursor the call read it at; a kept-back rest
// is still owed only if the ring holds none after the head it was kept at.
// Anything later is somebody acting on the run, whose own wake jobs settle it.
//
// A release takes effect on the wake queue, behind every wake job already
// queued: a running transition's clear queued before it has spent the record
// before the release writes one, and a rest's wake queued before it finds the
// hold standing. A call can also read a rest from the store before the
// engine's event has queued that rest's wake: the release then runs first and
// records the rest, and the wake that follows is suppressed by
// wakeAlreadyDelivered. The engine persists a rest's phase before the
// transition, so the record and the later wake compose the same signature.
//
// Holds are process memory. A crash while one stands loses a kept-back rest,
// as a crash between a rest and its wake job always could; a rest is surfaced
// once, on its transition.

// wakeHolds is every standing hold, by run.
type wakeHolds struct {
	mu    sync.Mutex
	byRun map[string][]*WakeHold
}

// WakeHold is one waiting call's claim on a run's wakes to one thread.
type WakeHold struct {
	service  *Service
	itemID   string
	threadID string
	once     sync.Once
	// held says a rest of the run arrived while the hold stood and was kept
	// back, at ring head keptAt; claim is the usage-attention claim that
	// rest carried, if any.
	held   bool
	keptAt int64
	claim  *store.WorkflowProviderUsageAttentionClaim
}

// ReturnedRest is the rest a call handed its agent: the watch cursor it read
// the run at, and the state and reason the run showed there.
type ReturnedRest struct {
	Cursor int64
	State  string
	Reason string
}

// HoldRestWake keeps the run's rests from waking threadID until Release. A
// run bound to another thread is unaffected: the hold is matched against the
// binding when a rest arrives.
func (s *Service) HoldRestWake(itemID, threadID string) *WakeHold {
	hold := &WakeHold{service: s, itemID: itemID, threadID: threadID}
	s.holds.mu.Lock()
	defer s.holds.mu.Unlock()
	if s.holds.byRun == nil {
		s.holds.byRun = make(map[string][]*WakeHold)
	}
	s.holds.byRun[itemID] = append(s.holds.byRun[itemID], hold)
	return hold
}

// Release ends the hold. returned is the rest the call delivered to its
// agent, nil when it delivered none. Later calls do nothing.
func (h *WakeHold) Release(returned *ReturnedRest) {
	h.once.Do(func() {
		h.service.wake.Go(func() { h.service.releaseWakeHold(h, returned) })
	})
}

// keepBackRest is the wake half's check, made for a root's rest before it is
// composed: a call holding the run for its bound thread will hand the rest
// over itself, or wake the thread with it when it ends without doing so.
func (s *Service) keepBackRest(item store.WorkItem, claim *store.WorkflowProviderUsageAttentionClaim) bool {
	keptAt := s.watch.Head()
	s.holds.mu.Lock()
	defer s.holds.mu.Unlock()
	var replaced []*store.WorkflowProviderUsageAttentionClaim
	kept := false
	for _, hold := range s.holds.byRun[item.ID] {
		if hold.threadID != item.OriginThreadID {
			continue
		}
		// This rest replaces any the hold kept before: that one is no longer
		// what the thread is owed, and the attention it carried goes with it.
		replaced = append(replaced, hold.claim)
		hold.held, hold.keptAt, hold.claim = true, keptAt, nil
		if !kept {
			hold.claim = claim
		}
		kept = true
	}
	if !kept {
		return false
	}
	for _, old := range replaced {
		s.releaseWorkflowUsageAttention(old)
	}
	s.deps.Logf("workflow wake %s: kept a rest back from thread %s while a call waits on the run", item.ID, item.OriginThreadID)
	return true
}

// releaseWakeHold runs on the wake queue.
func (s *Service) releaseWakeHold(hold *WakeHold, returned *ReturnedRest) {
	s.holds.mu.Lock()
	holds := s.holds.byRun[hold.itemID]
	for index, standing := range holds {
		if standing == hold {
			holds = append(holds[:index], holds[index+1:]...)
			break
		}
	}
	if len(holds) == 0 {
		delete(s.holds.byRun, hold.itemID)
	} else {
		s.holds.byRun[hold.itemID] = holds
	}
	held, keptAt, claim := hold.held, hold.keptAt, hold.claim
	hold.claim = nil
	s.holds.mu.Unlock()

	if returned != nil {
		if item, ok := s.currentReturnedRest(hold, *returned); ok {
			s.recordCallDeliveredRest(item, hold, claim)
			return
		}
	}
	if held && !s.runMovedSince(hold.itemID, keptAt) {
		s.wakeKeptBackRest(hold, claim)
		return
	}
	s.releaseWorkflowUsageAttention(claim)
}

// runMovedSince reports whether the ring holds a transition of the run after
// cursor. A cursor the ring can no longer answer for counts as moved.
func (s *Service) runMovedSince(itemID string, cursor int64) bool {
	if cursor <= 0 {
		return true
	}
	moved, _, gap := s.watch.Since(cursor, func(id string) bool { return id == itemID })
	return gap || len(moved) > 0
}

// currentReturnedRest loads the run and reports whether the rest the call
// returned is still the run's rest for the hold's thread. The store check
// covers a transition written but not yet recorded in the ring.
func (s *Service) currentReturnedRest(hold *WakeHold, returned ReturnedRest) (store.WorkItem, bool) {
	if s.runMovedSince(hold.itemID, returned.Cursor) {
		return store.WorkItem{}, false
	}
	item, err := s.deps.Store.GetWorkItem(hold.itemID)
	if err != nil {
		s.deps.Logf("workflow wake %s: load run a call delivered: %v; its rest is treated as not delivered", hold.itemID, err)
		return store.WorkItem{}, false
	}
	current := item.ParentItemID == "" && item.OriginThreadID == hold.threadID &&
		restingWorkflowState(engine.State(item.State)) &&
		item.State == returned.State && item.Reason == returned.Reason
	return item, current
}

// recordCallDeliveredRest records the rest a call returned as the last wake
// its bound thread was given, so the same rest reaching the wake half after
// the call is suppressed, and any later rest (after somebody acts) delivers.
func (s *Service) recordCallDeliveredRest(item store.WorkItem, hold *WakeHold, claim *store.WorkflowProviderUsageAttentionClaim) {
	if claim != nil {
		// The attention the rest carried reached the agent with the rest.
		s.promoteWorkflowUsageAttention(*claim)
	}
	composed, err := s.composeWorkflowWake(item, nil)
	if err != nil {
		s.deps.Logf("workflow wake %s: compose the rest a call delivered: %v; its wake may repeat it", item.ID, err)
		return
	}
	s.recordWakeDelivery(item.ID, composed.signature)
	s.deps.Logf("workflow wake %s: a call waiting on the run delivered its rest to thread %s; no wake for it", item.ID, hold.threadID)
}

// wakeKeptBackRest delivers the rest a hold kept back from a call that ended
// without returning it, as the wake half would have when it arrived. It goes
// through the wake half's own check, so another call from the thread still
// waiting on the run keeps it back in turn.
func (s *Service) wakeKeptBackRest(hold *WakeHold, claim *store.WorkflowProviderUsageAttentionClaim) {
	item, err := s.deps.Store.GetWorkItem(hold.itemID)
	if err != nil {
		s.deps.Logf("workflow wake %s: load run to deliver a kept-back rest: %v", hold.itemID, err)
		s.releaseWorkflowUsageAttention(claim)
		return
	}
	if item.ParentItemID != "" || !restingWorkflowState(engine.State(item.State)) {
		// Somebody acted on the run since: the rest is spent, as the
		// transition's clear spends a delivered one.
		s.releaseWorkflowUsageAttention(claim)
		return
	}
	s.afterWorkflowResting(item, claim)
}
