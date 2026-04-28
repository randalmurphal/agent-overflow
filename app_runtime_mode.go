package main

import (
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/provider"
)

// runtimeModeLockAttemptedForTest lets the lock-order regression test observe
// that a direct runtime-mode update reached the per-thread send lock without
// relying on sleeps. Production leaves it nil.
var runtimeModeLockAttemptedForTest func(threadID string)

// ThreadRuntimeModeChangedEvent is the payload emitted after the runtime
// mode for a thread is updated. Mirrors the interaction-mode event shape for
// compatibility with older frontend code. Runtime-mode changes now restart
// active sessions synchronously, so needsReconnect is always false on success.
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

// SetThreadRuntimeMode is the legacy binding name preserved only while
// the frontend migration lands (Wave 2c+). The implementation routes to
// UpdateThreadRuntimeMode and returns the older event struct.
//
// The new per-field binding UpdateThreadRuntimeMode in app_threads.go
// is the going-forward surface and returns store.Thread directly.
func (a *App) SetThreadRuntimeMode(threadID, mode string) (ThreadRuntimeModeChangedEvent, error) {
	t, err := a.store.GetThread(threadID)
	if err != nil {
		return ThreadRuntimeModeChangedEvent{}, fmt.Errorf("set runtime mode: %w", err)
	}
	normalized, err := parseRuntimeMode(mode)
	if err != nil {
		return ThreadRuntimeModeChangedEvent{}, fmt.Errorf("set runtime mode: %w", err)
	}
	if provider.NormalizeRuntimeMode(t.RuntimeMode) == normalized {
		return ThreadRuntimeModeChangedEvent{
			ThreadID:       threadID,
			RuntimeMode:    string(normalized),
			NeedsReconnect: false,
		}, nil
	}

	if _, err := a.UpdateThreadRuntimeMode(threadID, string(normalized)); err != nil {
		return ThreadRuntimeModeChangedEvent{}, err
	}

	return ThreadRuntimeModeChangedEvent{
		ThreadID:       threadID,
		RuntimeMode:    string(normalized),
		NeedsReconnect: false,
	}, nil
}

func (a *App) emitRuntimeModeChanged(threadID string, mode provider.RuntimeMode) {
	a.emitEvent("thread:runtime_mode_changed", ThreadRuntimeModeChangedEvent{
		ThreadID:       threadID,
		RuntimeMode:    string(mode),
		NeedsReconnect: false,
	})
}

func parseOptionalRuntimeMode(mode string) (provider.RuntimeMode, bool, error) {
	if strings.TrimSpace(mode) == "" {
		return "", false, nil
	}
	normalized, err := parseRuntimeMode(mode)
	if err != nil {
		return "", false, err
	}
	return normalized, true, nil
}

func parseRuntimeMode(mode string) (provider.RuntimeMode, error) {
	normalized := provider.RuntimeMode(strings.TrimSpace(mode))
	switch normalized {
	case provider.RuntimeApprovalRequired, provider.RuntimeAutoAcceptEdits, provider.RuntimeFullAccess:
		return normalized, nil
	default:
		return "", fmt.Errorf("invalid runtime mode %q", mode)
	}
}

func (a *App) applyRuntimeMode(threadID string, mode provider.RuntimeMode) error {
	if runtimeModeLockAttemptedForTest != nil {
		runtimeModeLockAttemptedForTest(threadID)
	}
	unlock := sendThreadMuRegistry.lockFor(threadID)
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
	a.mu.Lock()
	startState := a.startingSessions[threadID]
	a.mu.Unlock()
	if startState == nil {
		return false, nil
	}
	<-startState.done
	return true, startState.err
}
