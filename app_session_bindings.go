package main

import (
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

	if !hasSession && thread.SessionRef != "" {
		go func() {
			if err := a.startSession(threadID); err != nil {
				log.Printf("app: auto-resume failed for %s: %v", threadID, err)
			}
		}()
	}

	return thread, nil
}

// ReconnectSession tears down the current session and starts a fresh one using
// the thread's stored resume cursor.
func (a *App) ReconnectSession(threadID string) error {
	if err := a.stopSession(threadID); err != nil {
		return err
	}
	return a.startSession(threadID)
}

func (a *App) startSession(threadID string) error {
	if a.startSessionFn != nil {
		return a.startSessionFn(threadID)
	}
	return a.StartSession(threadID)
}

func (a *App) stopSession(threadID string) error {
	if a.stopSessionFn != nil {
		return a.stopSessionFn(threadID)
	}
	return a.StopSession(threadID)
}
