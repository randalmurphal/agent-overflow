package engine

import (
	"errors"
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// cancelCallChildren brings a parent's whole live child subtree down before the
// parent itself moves, which is the tree-aware half of the teardown contract
// (§12, D23). A descendant whose parent has left the phase that called it can
// never be consumed by anything, so leaving one running would strand a real
// provider session with no reader.
//
// Two phase shapes can hold live children: a call phase, which does not finish
// until its one child is terminal, and a fan-out phase, whose call-bound units
// each hold one. The fan-out case reads the children from the record rather than
// from `item.fan`, because the crash-rebuild path tears an attempt down with no
// in-memory unit state at all — and that is exactly the case where a stranded
// grandchild would otherwise survive a restart.
func (e *Engine) cancelCallChildren(item *runtimeItem) error {
	if item.phaseID == "" {
		return nil
	}
	phase, ok := findPhase(item.workflow, item.phaseID)
	if !ok {
		return nil
	}
	switch phase.EffectiveShape() {
	case def.ShapeCall:
		return e.cancelDescendants(item.item.ID, 0)
	case def.ShapeFanOut:
		return e.cancelAttemptUnitChildren(item)
	default:
		return nil
	}
}

// cancelAttemptUnitChildren cancels the child runs the current fan-out attempt's
// call units created. Children of *other* attempts are left alone: they were
// settled when their own attempt ended, and a repaired attempt keeps the results
// its finished units already produced.
func (e *Engine) cancelAttemptUnitChildren(item *runtimeItem) error {
	children, err := e.store.ListWorkItemChildren(item.item.ID)
	if err != nil {
		return fmt.Errorf("list children of %q: %w", item.item.ID, err)
	}
	var errs []error
	for _, child := range children {
		if child.ParentUnitID == "" || child.ParentPhaseID != item.phaseID || child.ParentAttempt != item.attempt {
			continue
		}
		errs = append(errs, e.cancelSubtree(child, 0))
	}
	return errors.Join(errs...)
}

// cancelDescendants cancels every non-terminal descendant of an item, deepest
// first.
func (e *Engine) cancelDescendants(itemID string, depth int) error {
	if depth > MaxCallDepth {
		return fmt.Errorf("cancel descendants of %q: tree is deeper than %d", itemID, MaxCallDepth)
	}
	children, err := e.store.ListWorkItemChildren(itemID)
	if err != nil {
		return fmt.Errorf("list children of %q: %w", itemID, err)
	}
	var errs []error
	for _, child := range children {
		errs = append(errs, e.cancelSubtree(child, depth))
	}
	return errors.Join(errs...)
}

// cancelSubtree cancels one child and everything below it, deepest first.
// Resident runs go through the same teardown every cancel uses; a parked
// descendant holds nothing and is transitioned in place, because the engine
// evicts parked items from memory and it must still come down with its tree.
func (e *Engine) cancelSubtree(child store.WorkItem, depth int) error {
	state := State(child.State)
	if state != StateRunning && state != StateNeedsHuman {
		return nil
	}
	var errs []error
	if err := e.cancelDescendants(child.ID, depth+1); err != nil {
		errs = append(errs, err)
	}
	resident, tracked := e.items[child.ID]
	if tracked {
		errs = append(errs, e.teardown(resident, teardownRequest{
			phaseStatus: "cancelled", nextState: StateCancelled, reason: ReasonInterrupted,
		}))
		return errors.Join(errs...)
	}
	return errors.Join(append(errs, e.cancelEvictedChild(child))...)
}

// cancelEvictedChild cancels a parked descendant the scheduler no longer holds
// in memory. It owns no resources and no runner — parking released both — so
// the transition is the whole teardown for it.
func (e *Engine) cancelEvictedChild(child store.WorkItem) error {
	endedAt := e.timestamp()
	if err := e.store.UpdateWorkItemState(child.ID, string(StateCancelled), string(ReasonInterrupted), endedAt); err != nil {
		return fmt.Errorf("cancel parked child %q: %w", child.ID, err)
	}
	e.emitItemState(child.ID, child.ProjectID, State(child.State), StateCancelled, ReasonInterrupted)
	return nil
}
