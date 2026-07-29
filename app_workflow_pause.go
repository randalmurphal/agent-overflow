package main

import (
	"context"
	"fmt"
	"log"
	"time"
)

// Per-run pause and its graceful-quit counterpart (decision D23).

// WorkflowPauseItem parks a run tree `needs-human(paused)`: in-flight turns are
// interrupted now, resources are released, and every live member of the tree
// comes down through the engine's one teardown path. Resuming continues on the
// provider sessions the runs parked on.
//
// LocalOnly: pausing interrupts local provider processes and releases the
// worktrees they hold.
func (a *App) WorkflowPauseItem(ctx context.Context, itemID string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	if err := a.authorizeScopedRunAction(ctx, itemID, "pause workflow run"); err != nil {
		return err
	}
	return workflowEngine.PauseItem(itemID)
}

// WorkflowRequestSoftStop arms or clears a run tree's request to stop at its
// next call boundary (D36). Nothing is interrupted and nothing starts: the run
// keeps going and, the next time it would invoke a call, parks
// `needs-human(checkpoint)` instead. Resuming takes the call it skipped.
//
// It is one method with a flag rather than a pair, because the two directions
// are one piece of state: a clear has to be able to undo an arm through exactly
// the path that set it, and a caller that can only ever arm would have no way to
// change its mind.
//
// LocalOnly: the request decides whether the next wave of autonomous provider
// sessions runs, which is the same control plane as pause.
func (a *App) WorkflowRequestSoftStop(ctx context.Context, itemID string, armed bool) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	if err := a.authorizeScopedRunAction(ctx, itemID, "request a workflow soft stop"); err != nil {
		return err
	}
	return workflowEngine.SetSoftStop(itemID, armed)
}

// pauseWorkflowRunsForShutdown is the graceful-quit half: every active root run
// is paused before the process tears its provider sessions down, so a restart
// finds resumable `needs-human(paused)` runs rather than crash-parked ones.
//
// It is bounded rather than awaited. PauseAllActive itself never waits for a
// turn to finish — it interrupts — but it does serialize behind whatever the
// engine's command loop is already doing, and that could be a phase entry that
// is provisioning a worktree. Quit must not hang on it. Timing out is a
// degradation, not a failure: the runs that missed the window are exactly the
// ones the startup sweep parks `interrupted`, which resume handles identically.
func (a *App) pauseWorkflowRunsForShutdown() error {
	done := make(chan error, 1)
	go func() { done <- a.workflowEngine.PauseAllActive() }()
	timer := time.NewTimer(workflowPauseAllTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("pause active workflow runs: %w", err)
		}
		return nil
	case <-timer.C:
		// Deliberately not an error: the runs still running are recoverable by
		// the startup sweep, and reporting a failed shutdown step for an
		// expected slow-quit would train the log's readers to ignore it.
		log.Printf(
			"workflow shutdown: pausing active runs did not finish within %s; "+
				"runs still active will be parked interrupted on the next launch",
			workflowPauseAllTimeout,
		)
		return nil
	}
}
