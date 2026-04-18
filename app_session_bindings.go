package main

import (
	"fmt"
	"log"

	"agent-overflow/internal/store"
)

// SwitchThread returns the requested thread and auto-resumes its provider
// session in the background when the thread has a stored session reference.
func (a *App) SwitchThread(threadID string) (store.Thread, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}

	a.mu.Lock()
	_, hasSession := a.sessions[threadID]
	a.mu.Unlock()

	if !hasSession && (thread.SessionRef != "" || thread.PendingForkRef != "") {
		// Auto-resume runs in a single goroutine without a wrapping timeout.
		// The provider's own connect timeout handles the slow-start case.
		// A previous implementation wrapped startSession in an inner goroutine
		// with a separate timeout select — that caused the inner goroutine to
		// keep running after the timeout fired, leaving a session in the map
		// that the UI believed was dead. Retries could then deadlock on the
		// sessionStart.done channel. Keeping it simple avoids both problems.
		go func() {
			if err := a.startSession(threadID); err != nil {
				log.Printf("app: auto-resume failed for %s: %v", threadID, err)
				a.emitErrorToThread(threadID, fmt.Sprintf("auto-resume failed: %v", err))
			}
		}()
	}

	return thread, nil
}

// ReconnectSession tears down the current session and starts a fresh one using
// the thread's stored resume cursor.
func (a *App) ReconnectSession(threadID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	if err := a.stopSession(threadID); err != nil {
		return err
	}
	return a.startSession(threadID)
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
