package app

import (
	"context"
	"log"
	"sync"
)

// deleteDecisions are the running deletes' decisions, by delete operation:
// the claim on a thread's row names the delete that decides it. A delete
// registers its decision before it writes a claim (deleteThreads) and makes
// it as it commits (commit) or, failing that, as it ends, so on a running app a claim names a decision still to come or one
// already made; the boot releases every claim no delete committed before
// anything starts (releaseUndecidedDeletes). A start the app makes on its
// own, a dispatch or a notice's session start, that meets a claim waits for
// the decision holding no lock (waitDeleteDecision); a person's doors refuse
// at once. A delete never waits for a decision. Zero value ready.
type deleteDecisions struct {
	mu      sync.Mutex
	ops     map[string]*deleteDecision
	waiting int // starts waiting for a decision
}

// deleteDecision is one delete's decision: decided closes once it is made,
// and committed says which it was.
type deleteDecision struct {
	decided   chan struct{}
	committed bool
}

// open registers op's decision, still to come.
func (d *deleteDecisions) open(op string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ops == nil {
		d.ops = make(map[string]*deleteDecision)
	}
	if d.ops[op] == nil {
		d.ops[op] = &deleteDecision{decided: make(chan struct{})}
	}
}

// decide makes op's decision, and the starts waiting for it go on. A
// decision is made once; a later call does nothing.
func (d *deleteDecisions) decide(op string, committed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if decision, ok := d.ops[op]; ok {
		decision.committed = committed
		close(decision.decided)
		delete(d.ops, op)
	}
}

// waitDeleteDecision waits, holding no lock, for the decision of the delete
// that claims threadID, and reports whether the start is to try again from
// the top: false when the delete committed the thread, when ctx ended first,
// or when the claim stands with no delete under way to decide it (a boot or
// a delete whose release failed), which leaves the thread's queue to the
// next boot.
func (a *App) waitDeleteDecision(ctx context.Context, threadID string) bool {
	claim, committed, err := a.store.ThreadDeleteClaim(threadID)
	if err != nil || committed || claim == "" {
		// Decided, or unread: the start's own check reads it again.
		return err != nil || !committed
	}
	if hook := a.deleteDecisionReadForTest; hook != nil {
		hook(threadID)
	}
	d := &a.deleteDecisions
	d.mu.Lock()
	decision, open := d.ops[claim]
	if open {
		d.waiting++
	}
	d.mu.Unlock()
	if !open {
		// A decision is made after its claims: a claim standing now has no delete.
		if again, committed, err := a.store.ThreadDeleteClaim(threadID); err == nil && again == claim && !committed {
			log.Printf("app: thread %s is claimed by delete %s, which is not under way; what it queued waits for the next boot", threadID, claim)
			return false
		}
		return true
	}
	defer func() {
		d.mu.Lock()
		d.waiting--
		d.mu.Unlock()
	}()
	select {
	case <-decision.decided:
		if hook := a.deleteDecisionMadeForTest; hook != nil {
			hook(threadID)
		}
		// A committed thread starts nothing, and its row may be gone already.
		return !decision.committed
	case <-ctx.Done():
		return false
	}
}
