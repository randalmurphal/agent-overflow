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
// call ends it either delivered the rest, which is recorded as the bound
// thread's last delivered wake so the coalescing rule suppresses the same rest
// arriving later, or it did not, and the rest it kept back wakes the thread as
// it would have.
//
// A release takes effect on the wake queue, in order with the rests it
// settles. A wake job queued before the release still finds the hold standing
// and keeps its rest back. A call can also read a rest from the store before
// the engine's event has queued that rest's wake: the release then runs first
// and records the rest, and the wake that follows is suppressed by
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
	// back; claim is the usage-attention claim that rest carried, if any.
	held  bool
	claim *store.WorkflowProviderUsageAttentionClaim
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

// Release ends the hold. delivered says the call returned the run at rest.
// Later calls do nothing.
func (h *WakeHold) Release(delivered bool) {
	h.once.Do(func() {
		// On the wake queue, behind every wake job already queued for this
		// run: those still see the hold, so a release never lets past a rest
		// it is about to settle.
		h.service.wake.Go(func() { h.service.releaseWakeHold(h, delivered) })
	})
}

// keepBackRest is the wake half's check, made for a root's rest before it is
// composed: a call holding the run for its bound thread will hand the rest
// over itself, or wake the thread with it when it ends without doing so.
func (s *Service) keepBackRest(item store.WorkItem, claim *store.WorkflowProviderUsageAttentionClaim) bool {
	s.holds.mu.Lock()
	defer s.holds.mu.Unlock()
	kept := false
	for _, hold := range s.holds.byRun[item.ID] {
		if hold.threadID != item.OriginThreadID {
			continue
		}
		hold.held = true
		if !kept && hold.claim == nil {
			hold.claim, claim = claim, nil
		}
		kept = true
	}
	if !kept {
		return false
	}
	// A rest that comes while an earlier one is still kept back replaces
	// it; the attention it carried is already on a standing hold.
	s.releaseWorkflowUsageAttention(claim)
	s.deps.Logf("workflow wake %s: kept a rest back from thread %s while a call waits on the run", item.ID, item.OriginThreadID)
	return true
}

// releaseWakeHold runs on the wake queue.
func (s *Service) releaseWakeHold(hold *WakeHold, delivered bool) {
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
	held, claim := hold.held, hold.claim
	hold.claim = nil
	s.holds.mu.Unlock()

	switch {
	case delivered:
		s.recordCallDeliveredRest(hold, claim)
	case held:
		s.wakeKeptBackRest(hold, claim)
	default:
		s.releaseWorkflowUsageAttention(claim)
	}
}

// recordCallDeliveredRest records the rest a call returned as the last wake
// its bound thread was given, so the same rest reaching the wake half after
// the call is suppressed, and any later rest (after somebody acts) delivers.
func (s *Service) recordCallDeliveredRest(hold *WakeHold, claim *store.WorkflowProviderUsageAttentionClaim) {
	if claim != nil {
		// The attention the rest carried reached the agent with the rest.
		s.promoteWorkflowUsageAttention(*claim)
	}
	item, err := s.deps.Store.GetWorkItem(hold.itemID)
	if err != nil {
		s.deps.Logf("workflow wake %s: load run a call delivered: %v; its wake may repeat the rest", hold.itemID, err)
		return
	}
	if item.ParentItemID != "" || item.OriginThreadID != hold.threadID || !restingWorkflowState(engine.State(item.State)) {
		// Not the caller's bound root, or no longer resting: whatever it was
		// told is already spent, and there is nothing a wake could repeat.
		return
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
