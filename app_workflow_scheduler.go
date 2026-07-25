package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/scheduler"
)

// workflowSchedulerStopTimeout bounds the graceful stop of the scheduler loop.
// The loop is only ever busy inside one fire (a start, which is itself bounded
// by the engine's command loop), so this covers the worst case with room to
// spare; a scheduler that misses the window is reported rather than waited on,
// because quit latency is the thing worth protecting.
const workflowSchedulerStopTimeout = 3 * time.Second

// initWorkflowScheduler starts the §11 scheduler over the engine that was just
// initialised. It is deliberately the last piece of workflow boot: the timer
// must not be able to fire into an engine that does not exist yet.
//
// Next fires are computed from now, so a schedule missed while the app was
// closed is neither replayed nor recorded as a skip.
func (a *App) initWorkflowScheduler() error {
	workflowScheduler, err := scheduler.New(scheduler.Config{
		Store: a.store,
		Start: a.startAutomationRun,
		Clock: scheduler.SystemClock(),
		Report: func(err error) {
			log.Printf("workflow scheduler: %v", err)
		},
	})
	if err != nil {
		return err
	}
	a.workflowScheduler = workflowScheduler
	workflowScheduler.Start()
	return nil
}

// stopWorkflowScheduler ends the timer loop. Shutdown runs it before anything
// else workflow-related: pausing every active run while a trigger can still
// start a new one would be a race the human loses.
func (a *App) stopWorkflowScheduler(ctx context.Context) error {
	if a.workflowScheduler == nil {
		return nil
	}
	stopCtx, cancel := contextWithTimeout(ctx, workflowSchedulerStopTimeout)
	defer cancel()
	err := a.workflowScheduler.Stop(stopCtx)
	// The feed queue can hold jobs that were submitted before the stop; draining
	// them here keeps a stopped-scheduler log line off the shutdown path of every
	// later step. Each such job returns ErrStopped immediately.
	a.workflowSchedulerQueue.Wait()
	return err
}

func (a *App) requireWorkflowScheduler() (*scheduler.Scheduler, error) {
	if a.shuttingDown.Load() {
		return nil, ErrShuttingDown
	}
	if a.workflowScheduler == nil {
		return nil, fmt.Errorf("workflow scheduler unavailable")
	}
	return a.workflowScheduler, nil
}

// startAutomationRun is the scheduler's only way to start work: the same app
// start path every other producer calls, stamped with the automation as the
// run's source. An automation carries no budget or base-branch override of its
// own — those come from the project profile — and never runs in step mode,
// which would park an unattended run at its first phase boundary.
func (a *App) startAutomationRun(automation store.Automation, goal string, seeds json.RawMessage) (string, error) {
	item, err := a.startWorkflowRun(
		automation.ProjectID, automation.WorkflowID, automation.WorkflowScope, goal, seeds,
		nil, "", false, scheduler.Source, automation.ID,
	)
	if err != nil {
		return "", err
	}
	return item.ID, nil
}

// notifyWorkflowScheduler feeds one run's resting transition to the event
// triggers. It runs on its own ordered queue for the same reason the wake does:
// the engine emits from its command loop, so nothing here may block it, and two
// transitions must not race each other's follow-up. A separate queue from the
// wake's keeps a slow wake composition from delaying a chained automation.
func (a *App) notifyWorkflowScheduler(event engine.StateEvent) {
	if a.workflowScheduler == nil {
		return
	}
	if _, ok := scheduler.EventKindForState(string(event.To)); !ok {
		return
	}
	a.workflowSchedulerQueue.Go(func() { a.feedWorkflowSchedulerEvent(event) })
}

func (a *App) feedWorkflowSchedulerEvent(event engine.StateEvent) {
	workflowScheduler := a.workflowScheduler
	if workflowScheduler == nil {
		return
	}
	// The transition is the event; the row supplies only facts that cannot
	// change after creation (workflow, provenance, parent linkage), so a run that
	// has already moved on cannot rewrite what fired.
	item, err := a.store.GetWorkItem(event.ItemID)
	if err != nil {
		log.Printf("workflow scheduler feed %s: load run: %v", event.ItemID, err)
		return
	}
	err = workflowScheduler.NotifyItemEvent(scheduler.ItemEvent{
		ProjectID: event.ProjectID, ItemID: event.ItemID, WorkflowID: item.WorkflowID,
		State: string(event.To), Reason: string(event.Reason),
		ParentItemID: item.ParentItemID, Source: item.Source, SourceRef: item.SourceRef,
	})
	if err != nil && !errors.Is(err, scheduler.ErrStopped) {
		log.Printf("workflow scheduler feed %s: %v", event.ItemID, err)
	}
}
