package main

import (
	"fmt"

	"agent-overflow/internal/provider"
)

// runtimeModeLockAttemptedForTest lets the lock-order regression test observe
// that a direct runtime-mode update reached the per-thread action lock without
// relying on sleeps. Production leaves it nil.
var runtimeModeLockAttemptedForTest func(threadID string)

// ThreadRuntimeModeChangedEvent is the payload emitted after the runtime
// mode for a thread is updated. Runtime-mode changes apply live (Claude
// set_permission_mode, Codex per-turn approval/sandbox overrides) or via a
// reconciler-owned restart, so needsReconnect is always false on success.
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

	// Live on both providers except escalating a Claude session to full
	// access (the CLI refuses bypassPermissions on a process spawned
	// without --allow-dangerously-skip-permissions); that case restarts,
	// deferred while the thread is busy. The reconciler never takes the
	// per-thread action lock this caller holds — its deferred restarts run
	// on a watcher goroutine that acquires the lock itself.
	a.reconcileSessionConfig(threadID)
	a.emitRuntimeModeChanged(threadID, mode)
	return nil
}

func (a *App) waitForStartingSession(threadID string) (bool, error) {
	startState, _ := a.sessionManager().startState(threadID)
	if startState == nil {
		return false, nil
	}
	<-startState.done
	return true, startState.err
}
