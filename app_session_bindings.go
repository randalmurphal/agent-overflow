package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/provider"
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
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			errCh := make(chan error, 1)
			// The inner goroutine is intentionally not cancelled by the timeout.
			// Session startup should run to completion to avoid orphaned provider
			// state. The timeout only controls how long we wait before reporting
			// failure to the user.
			go func() {
				errCh <- a.startSession(threadID)
			}()

			select {
			case err := <-errCh:
				if err != nil {
					log.Printf("app: auto-resume failed for %s: %v", threadID, err)
					a.emitAutoResumeError(threadID, err)
				}
			case <-ctx.Done():
				err := fmt.Errorf("auto-resume timed out after 60s")
				log.Printf("app: %v for %s", err, threadID)
				a.emitAutoResumeError(threadID, err)
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

func (a *App) emitAutoResumeError(threadID string, err error) {
	evt := provider.ProviderEvent{
		Kind:      provider.EventError,
		ThreadID:  threadID,
		Content:   fmt.Sprintf("auto-resume failed: %v", err),
		Timestamp: time.Now(),
	}

	if a.triage != nil {
		if handleErr := a.triage.Handle(evt); handleErr != nil {
			log.Printf("app: emit auto-resume error for %s: %v", threadID, handleErr)
		}
		return
	}
	if a.app != nil {
		a.app.Event.Emit("provider:event", evt)
	}
}
