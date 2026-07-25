package main

import (
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
func (a *App) WorkflowPauseItem(itemID string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	return workflowEngine.PauseItem(itemID)
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
