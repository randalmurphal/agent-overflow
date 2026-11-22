package app

import (
	"context"
	"fmt"
	"time"
)

// updateWorkReason says what running work the restart to update waits for,
// or "". It runs under workAdmission.mu, so it must never acquire a thread
// action lock or call the workflow actor; the computer scope of
// runningWorkIn only reads existing owners.
func (a *App) updateWorkReason() (string, error) {
	work, err := a.runningWorkIn(workScope{computer: true})
	if err != nil || !work.running() {
		return "", err
	}
	switch work.kind {
	case workTurn, workAwaitingAnswer:
		return "Waiting for turns and queued work to finish…", nil
	case workQueuedMessages:
		return "Waiting for queued messages to finish…", nil
	case workBackgroundTasks:
		return "Waiting for background agents and commands to finish…", nil
	case workShellCommand:
		return "Waiting for shell commands to finish…", nil
	case workTerminal:
		return "Close this computer’s terminals to finish the update.", nil
	case workRemoteJob:
		return "Waiting for remote commands to finish…", nil
	case workWorkflow:
		return "Waiting for workflows to finish…", nil
	case workProviderUpdate:
		return "Waiting for a provider CLI update to finish…", nil
	case workWorktreeSetup, workRemoteWatch, workAgentWatch:
		fallthrough
	default:
		return fmt.Sprintf("Waiting to finish: %s…", work.kind), nil
	}
}

// waitForUpdateIdle returns once work admission is closed with nothing
// running, or with ctx's error. onWaiting receives each new reason the host
// is not idle yet; published is a reason the caller has already announced.
func (a *App) waitForUpdateIdle(ctx context.Context, published string, onWaiting func(reason string)) error {
	previous := published
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		reason, err := a.workAdmission.quiesce(a.updateWorkReason)
		if err != nil || reason == "" {
			return err
		}
		if reason != previous {
			onWaiting(reason)
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
