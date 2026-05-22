package main

import (
	"fmt"
	"log"

	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/store"
)

// SwitchThread returns the requested thread, marks it read durably, and
// auto-resumes its provider session in the background when the thread has a
// stored session reference.
func (a *App) SwitchThread(threadID string) (store.Thread, error) {
	if err := a.markThreadFocused(threadID); err != nil {
		return store.Thread{}, err
	}
	return a.loadThreadForFocus(threadID)
}

// AutoResumeThread starts the stored provider session for a focused thread if
// one is not already live. It is deliberately separate from SwitchThread so
// remote clients can focus/read threads without gaining an implicit local
// process-spawn path.
func (a *App) AutoResumeThread(threadID string) error {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return err
	}
	a.resumeThreadAfterFocus(thread)
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

func (a *App) resumeThreadAfterFocus(thread store.Thread) {
	_, hasSession := a.sessionManager().get(thread.ID)

	if !hasSession && (thread.SessionRef != "" || thread.PendingForkRef != "") {
		// Auto-resume runs in a single goroutine without a wrapping timeout.
		// The provider's own connect timeout handles the slow-start case.
		// A previous implementation wrapped startSession in an inner goroutine
		// with a separate timeout select — that caused the inner goroutine to
		// keep running after the timeout fired, leaving a session in the map
		// that the UI believed was dead. Retries could then deadlock on the
		// sessionStart.done channel. Keeping it simple avoids both problems.
		go func() {
			if err := a.startSession(thread.ID); err != nil {
				log.Printf("app: auto-resume failed for %s: %v", thread.ID, err)
				a.emitErrorToThread(thread.ID, fmt.Sprintf("auto-resume failed: %v", err))
			}
		}()
	}
}

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
