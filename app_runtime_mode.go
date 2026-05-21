package main

import (
	"fmt"
	"log"

	"agent-overflow/internal/provider"
)

// runtimeModeLockAttemptedForTest lets the lock-order regression test observe
// that a direct runtime-mode update reached the per-thread action lock without
// relying on sleeps. Production leaves it nil.
var runtimeModeLockAttemptedForTest func(threadID string)

// ThreadRuntimeModeChangedEvent is the payload emitted after the runtime
// mode for a thread is updated. Runtime-mode changes now restart active
// sessions synchronously, so needsReconnect is always false on success.
type ThreadRuntimeModeChangedEvent struct {
	ThreadID       string `json:"threadId"`
	RuntimeMode    string `json:"runtimeMode"`
	NeedsReconnect bool   `json:"needsReconnect"`
}

// GetThreadRuntimeMode returns the persisted runtime mode for a thread.
// Exposed as a convenience binding so the frontend picker doesn't have to
// round-trip the full Thread shape.
func (a *App) GetThreadRuntimeMode(threadID string) (string, error) {
	t, err := a.store.GetThread(threadID)
	if err != nil {
		return "", fmt.Errorf("get runtime mode: %w", err)
	}
	return string(provider.NormalizeRuntimeMode(t.RuntimeMode)), nil
}

func (a *App) emitRuntimeModeChanged(threadID string, mode provider.RuntimeMode) {
	a.emitEvent("thread:runtime_mode_changed", ThreadRuntimeModeChangedEvent{
		ThreadID:       threadID,
		RuntimeMode:    string(mode),
		NeedsReconnect: false,
	})
}

func (a *App) applyRuntimeMode(threadID string, mode provider.RuntimeMode) error {
	if runtimeModeLockAttemptedForTest != nil {
		runtimeModeLockAttemptedForTest(threadID)
	}
	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	return a.applyRuntimeModeLocked(threadID, mode)
}

func (a *App) applyRuntimeModeLocked(threadID string, mode provider.RuntimeMode) error {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return err
	}
	previous := provider.NormalizeRuntimeMode(thread.RuntimeMode)
	if previous == mode {
		return nil
	}

	if err := a.store.UpdateRuntimeMode(threadID, string(mode)); err != nil {
		return err
	}

	waitedForStart, waitErr := a.waitForStartingSession(threadID)
	if waitErr != nil {
		log.Printf("thread %s: in-flight session start before runtime-mode change failed: %v", threadID, waitErr)
	}

	if !waitedForStart && !a.hasActiveSession(threadID) {
		a.emitRuntimeModeChanged(threadID, mode)
		return nil
	}
	if err := a.startSession(threadID); err != nil {
		return a.rollbackRuntimeModeAfterRestartFailure(threadID, previous, err)
	}
	a.emitRuntimeModeChanged(threadID, mode)
	return nil
}

func (a *App) rollbackRuntimeModeAfterRestartFailure(
	threadID string,
	previous provider.RuntimeMode,
	restartErr error,
) error {
	if rollbackErr := a.store.UpdateRuntimeMode(threadID, string(previous)); rollbackErr != nil {
		return fmt.Errorf("restart session with updated runtime mode: %w (rollback failed: %v)", restartErr, rollbackErr)
	}
	if recoveryErr := a.startSession(threadID); recoveryErr != nil {
		return fmt.Errorf("restart session with updated runtime mode: %w (rollback session restart failed: %v)", restartErr, recoveryErr)
	}
	return fmt.Errorf("restart session with updated runtime mode: %w", restartErr)
}

func (a *App) waitForStartingSession(threadID string) (bool, error) {
	startState, _ := a.sessionManager().startState(threadID)
	if startState == nil {
		return false, nil
	}
	<-startState.done
	return true, startState.err
}
