package app

import (
	"context"
	"fmt"
	"time"
)

// updateWorkReason only reads existing owners. It runs under workAdmission.mu,
// so it must never acquire a thread action lock or call the workflow actor.
func (a *App) updateWorkReason() (string, error) {
	// Read triage before dispatch: a queue handoff retains its triage claim
	// until the dispatcher has published its in-flight count.
	if a.triage.AnyUnfinishedWork(wakeupReapGrace) {
		return "Waiting for turns and queued work to finish…", nil
	}
	a.flushDispatch.mu.Lock()
	flushing := len(a.flushDispatch.inflightItems) != 0
	a.flushDispatch.mu.Unlock()
	if flushing {
		return "Waiting for queued messages to finish…", nil
	}
	if a.remoteJobs != nil && a.remoteJobs.HasActive() {
		return "Waiting for remote commands to finish…", nil
	}
	if a.terminals != nil && len(a.terminals.ThreadProcesses()) != 0 {
		return "Close this computer’s terminals to finish the update.", nil
	}
	if a.store == nil {
		return "", nil // Bare update fixtures have no session-capable store.
	}
	count, err := a.store.CountWorkItemsInStates("running")
	if err != nil {
		return "", fmt.Errorf("checking active workflows: %w", err)
	}
	if count != 0 {
		return "Waiting for workflows to finish…", nil
	}
	for threadID, sess := range a.sessionManager().snapshot() {
		if sess.Liveness != nil && sess.Liveness.ActiveTurns.Load() > 0 {
			return "Waiting for provider turns to finish…", nil
		}
		rows, err := a.runningBackgroundWorkForThread(threadID, sess.Provider)
		if err != nil {
			return "", fmt.Errorf("checking background work: %w", err)
		}
		if len(rows) != 0 {
			return "Waiting for background agents and commands to finish…", nil
		}
	}
	return "", nil
}

func (a *App) waitForUpdateIdle(ctx context.Context) error {
	var previous string
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		reason, err := a.workAdmission.quiesce(a.updateWorkReason)
		if err != nil || reason == "" {
			return err
		}
		if reason != previous {
			a.publishServiceUpdate(func(status *ServiceUpdateStatus) {
				status.Phase = serviceUpdatePhaseWaiting
				status.WaitingFor = reason
			})
			previous = reason
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
