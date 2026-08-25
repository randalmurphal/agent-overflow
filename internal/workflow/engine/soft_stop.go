package engine

import (
	"fmt"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
)

// Soft stop — "finish what you are doing, then stop" (spec §12, decision D36).
//
// A campaign is one ROOT run whose final phase calls the next wave (§3a), so
// the point at which stopping costs nothing is the moment before that call is
// made: the wave that just ran is complete, the next one has not started, and
// no turn is interrupted. `SetSoftStop` arms a standing request on the tree's
// root; `startCall` consults it and, when it is armed, parks the run that was
// about to call under `needs-human(checkpoint)` instead of invoking.
//
// Three properties make this a stop rather than a mode:
//
//   - It is checked ONLY at a call boundary. Nothing in flight is interrupted,
//     no phase is cut short, and a run with no call edge simply never reaches
//     the check — the flag stays armed and inert rather than firing somewhere
//     it was not asked to.
//   - The boundary that fires CONSUMES it. Arming is a one-shot, so the resume
//     that follows takes the call edge the park skipped instead of re-parking
//     on the same boundary forever.
//   - It lives on the ROOT and on the root alone. A call tree is stopped as a
//     tree, exactly like pause (§12), and every descendant's boundary reads the
//     root's row — which is why a wave forty deep still honours a request made
//     against the run a human is watching.

// SetSoftStop arms or disarms the request to stop a run tree at its next call
// boundary. It is idempotent in both directions: setting the state the run is
// already in succeeds and writes the same row, so a human pressing the button
// twice and an agent re-issuing the verb after a dropped response cannot
// disagree about what was asked for.
//
// It runs on the command loop rather than writing the row from the caller's
// goroutine, because the boundary check and the flag's consumption are also on
// that loop: making the engine the only writer is what removes the window where
// a request lands between a boundary's read and its clear and is silently lost.
func (e *Engine) SetSoftStop(itemID string, armed bool) error {
	return e.request(softStopCommand{itemID: itemID, armed: armed})
}

func (e *Engine) setSoftStop(itemID string, armed bool) error {
	stored, err := e.store.GetWorkItem(itemID)
	if err != nil {
		return fmt.Errorf("soft stop item %q: %w", itemID, err)
	}
	if stored.ParentItemID != "" {
		return fmt.Errorf(
			"soft stop item %q: this run was called by %s; a soft stop applies to the whole tree, so set it on the run that called it",
			itemID, stored.ParentItemID,
		)
	}
	// Arming a run that is not going has nothing to stop: there is no next call
	// boundary to reach, and leaving the request on the row would fire it at
	// whatever a later rerun did first. Disarming stays legal in every state so
	// clearing a request is never itself refused.
	if armed && State(stored.State) != StateRunning {
		return fmt.Errorf(
			"soft stop item %q: the run is %s; a soft stop stops a run that is still going",
			itemID, stored.State,
		)
	}
	if stored.SoftStop == armed {
		return nil
	}
	if err := e.store.SetWorkItemSoftStop(itemID, armed); err != nil {
		return fmt.Errorf("soft stop item %q: %w", itemID, err)
	}
	// The resident copy is refreshed so a reader inside the loop does not see a
	// row it just changed as unchanged. The boundary check re-reads regardless;
	// this keeps the two from disagreeing in a log line or an emitted payload.
	if resident, tracked := e.items[itemID]; tracked {
		resident.item.SoftStop = armed
	}
	e.emitter.Emit(eventchan.WorkflowSoftStop, SoftStopEvent{ItemID: itemID, Armed: armed})
	return nil
}

// SoftStopEvent announces that a run tree's stop request changed. It is its own
// channel rather than an item-state transition because nothing about the run's
// state changed — the run is still running, it simply now has an appointment.
type SoftStopEvent struct {
	ItemID string `json:"itemId"`
	Armed  bool   `json:"armed"`
}

// softStopArmed reports whether the tree this run belongs to has been asked to
// stop, returning the root so the caller can consume the request against the
// row that holds it.
func (e *Engine) softStopArmed(item *runtimeItem) (store.WorkItem, bool, error) {
	root, err := e.treeRoot(item)
	if err != nil {
		return store.WorkItem{}, false, err
	}
	return root, root.SoftStop, nil
}

// parkSoftStop is the boundary firing: it consumes the request and parks the
// run that was about to call.
//
// The clear happens FIRST. If the park then fails, the caller sees the error
// and the run is in whatever state the failed teardown left it — but the row no
// longer claims a stop is pending, which is the ordering that cannot strand a
// tree: a cleared flag with a failed park is a loud error a human resolves,
// while a set flag with a successful park would re-park the run on every resume.
//
// The item parked is the one whose boundary fired, not the root — the same rule
// the budget check follows (§12), and for the same reason: it is the run that
// was about to do the thing, and it is where the record of not doing it belongs.
func (e *Engine) parkSoftStop(item *runtimeItem, root store.WorkItem) error {
	if err := e.store.SetWorkItemSoftStop(root.ID, false); err != nil {
		return fmt.Errorf("soft stop item %q: clear request on root %q: %w", item.item.ID, root.ID, err)
	}
	if resident, tracked := e.items[root.ID]; tracked {
		resident.item.SoftStop = false
	}
	e.emitter.Emit(eventchan.WorkflowSoftStop, SoftStopEvent{ItemID: root.ID, Armed: false})
	return e.teardown(item, teardownRequest{
		cause:       softStopCause(item, root),
		phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonCheckpoint,
	})
}

// softStopCause is what the parked attempt's `park_cause` says. It is written as
// the phase's own record because no turn ran to author an envelope, and it is
// the only place the record states which call was skipped and whose request
// skipped it.
func softStopCause(item *runtimeItem, root store.WorkItem) error {
	if root.ID == item.item.ID {
		return fmt.Errorf(
			"stopped at the requested checkpoint: phase %q would have called the next run, and this run was asked to stop at its next call boundary; resume to take that call",
			item.phaseID,
		)
	}
	return fmt.Errorf(
		"stopped at the requested checkpoint: phase %q would have called the next run, and run %s (the root of this tree) was asked to stop at its next call boundary; resume to take that call",
		item.phaseID, root.ID,
	)
}
