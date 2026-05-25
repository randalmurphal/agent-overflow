package main

import (
	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/store"
)

// SwitchThread returns the requested thread and marks it read durably.
// Provider sessions are started lazily on first send, not on focus.
func (a *App) SwitchThread(threadID string) (store.Thread, error) {
	if err := a.markThreadFocused(threadID); err != nil {
		return store.Thread{}, err
	}
	return a.loadThreadForFocus(threadID)
}

// AutoResumeThread is a no-op retained for wire compatibility. Provider
// sessions are now started lazily on first send (app_send.go) rather than
// eagerly on thread focus. Eager resume was spawning ~240 MB Claude CLI
// processes (plus MCP servers) for every thread the user clicked on,
// accumulating gigabytes of resident memory across a handful of navigations
// before the 30-minute idle reaper could reclaim them.
func (a *App) AutoResumeThread(threadID string) error {
	return nil
}

func (a *App) markThreadFocused(threadID string) error {
	return a.store.MarkThreadReadNow(threadID)
}

func (a *App) loadThreadForFocus(threadID string) (store.Thread, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}
	sanitized := chatmodel.SanitizeThread(thread)
	if chatmodel.SameModelFields(thread, sanitized) {
		return thread, nil
	}
	if err := a.store.UpdateThread(sanitized); err != nil {
		return store.Thread{}, err
	}
	return sanitized, nil
}

// resumeThreadAfterFocus is a no-op. Retained so test fixtures that
// reference it still compile; the session spawn it used to perform now
// happens lazily in sendToProvider.
func (a *App) resumeThreadAfterFocus(_ store.Thread) {}

// ReconnectSession tears down the current session and starts a fresh one using
// the thread's stored resume cursor.
//
// Single-flight across the stop-then-start pair: a second concurrent caller
// returns nil without doing any work. Without the gate, a second call's
// stopSession can yank the new session out from under the first call's
// in-flight startSession (runSessionStart serialises starts but not stops),
// leaving the thread with no live session despite both calls completing
// "successfully". This matters for the auto-reconnect path racing a manual
// click on the banner Reconnect button.
func (a *App) ReconnectSession(threadID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	if !a.acquireReconnect(threadID) {
		return nil
	}
	defer a.releaseReconnect(threadID)
	if err := a.stopSession(threadID); err != nil {
		return err
	}
	return a.startSession(threadID)
}

func (a *App) acquireReconnect(threadID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.reconnectingThreads == nil {
		a.reconnectingThreads = make(map[string]bool)
	}
	if a.reconnectingThreads[threadID] {
		return false
	}
	a.reconnectingThreads[threadID] = true
	return true
}

func (a *App) releaseReconnect(threadID string) {
	a.mu.Lock()
	delete(a.reconnectingThreads, threadID)
	a.mu.Unlock()
}

func (a *App) startSession(threadID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	return a.runSessionStart(threadID, func() error {
		if a.startSessionFn != nil {
			return a.startSessionFn(threadID)
		}
		return a.startSessionNow(threadID)
	})
}

func (a *App) stopSession(threadID string) error {
	if a.stopSessionFn != nil {
		return a.stopSessionFn(threadID)
	}
	return a.StopSession(threadID)
}
