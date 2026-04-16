package main

import (
	"fmt"
	"strings"

	"agent-overflow/internal/store"
)

// UpdateThreadModel changes a thread's model and restarts an active provider
// session so the new model takes effect immediately. Threads without an active
// session are updated in place and will use the new model on the next start.
func (a *App) UpdateThreadModel(threadID string, model string) (threadResult store.Thread, err error) {
	normalizedModel := strings.TrimSpace(model)
	if normalizedModel == "" {
		return store.Thread{}, fmt.Errorf("thread model cannot be empty")
	}

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}
	if thread.Model == normalizedModel {
		return thread, nil
	}

	previousModel := thread.Model
	thread.Model = normalizedModel
	if err := a.store.UpdateModel(threadID, normalizedModel); err != nil {
		return store.Thread{}, err
	}

	active := a.hasActiveSession(threadID)
	if !active {
		thread.UpdatedAt = a.mustLoadThreadUpdatedAt(threadID, thread.UpdatedAt)
		return thread, nil
	}

	defer func() {
		if err == nil {
			return
		}
		if rollbackErr := a.store.UpdateModel(threadID, previousModel); rollbackErr != nil {
			err = fmt.Errorf("restart session with updated model: %w (rollback failed: %v)", err, rollbackErr)
		}
	}()

	if err = a.startSession(threadID); err != nil {
		return store.Thread{}, fmt.Errorf("restart session with updated model: %w", err)
	}

	updated, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}
	return updated, nil
}

func (a *App) hasActiveSession(threadID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.sessions[threadID]
	return ok
}

func (a *App) mustLoadThreadUpdatedAt(threadID string, fallback int64) int64 {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return fallback
	}
	return thread.UpdatedAt
}
