package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// UpdateThreadModel changes a thread's model and restarts an active provider
// session so the new model takes effect immediately. Threads without an active
// session are updated in place and will use the new model on the next start.
func (a *App) UpdateThreadModel(threadID string, model string) (threadResult store.Thread, err error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("update model: store unavailable")
	}
	normalizedModel := strings.TrimSpace(model)
	if normalizedModel == "" {
		return store.Thread{}, fmt.Errorf("thread model cannot be empty")
	}

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}
	if thread.Model == normalizedModel {
		a.rememberChatModelProfile(thread)
		return thread, nil
	}

	previous := thread
	thread.Model = normalizedModel
	if profile, profileErr := a.store.GetChatModelProfile(thread.Provider, normalizedModel); profileErr == nil {
		thread.ReasoningEffort = profile.ReasoningEffort
		thread.FastMode = profile.FastMode
		thread.ContextWindow = profile.ContextWindow
		thread.AutoCompactStandardPercent = profile.AutoCompactStandardPercent
		thread.AutoCompactExtendedPercent = profile.AutoCompactExtendedPercent
		thread.RuntimeMode = string(provider.NormalizeRuntimeMode(profile.RuntimeMode))
	} else {
		if !errors.Is(profileErr, sql.ErrNoRows) {
			return store.Thread{}, fmt.Errorf("load chat model profile: %w", profileErr)
		}
		thread.ContextWindow = a.defaultContextWindowForModel(thread.Provider, normalizedModel)
		thread.AutoCompactStandardPercent = 0
		thread.AutoCompactExtendedPercent = 0
		if thread.Provider != "claude" && !isValidContextWindow(thread.ContextWindow) {
			thread.ContextWindow = provider.CodexStandardContextWindow
		}
	}

	thread.UpdatedAt = time.Now().UnixMilli()
	if err := a.store.UpdateThread(thread); err != nil {
		return store.Thread{}, err
	}

	active := a.hasActiveSession(threadID)
	if !active {
		a.rememberChatModelProfile(thread)
		thread.UpdatedAt = a.mustLoadThreadUpdatedAt(threadID, thread.UpdatedAt)
		return thread, nil
	}

	defer func() {
		if err == nil {
			return
		}
		if rollbackErr := a.store.UpdateThread(previous); rollbackErr != nil {
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
	a.rememberChatModelProfile(updated)
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
