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
	previousContextWindow := thread.ContextWindow
	nextContextWindow := a.defaultContextWindowForModel(thread.Provider, normalizedModel)
	if thread.Provider != "claude" {
		nextContextWindow = thread.ContextWindow
		if !isValidContextWindow(nextContextWindow) {
			nextContextWindow = a.defaultContextWindow()
		}
	}
	settingsPatch := map[string]any{}
	switch thread.Provider {
	case "claude":
		settingsPatch["defaultModelClaude"] = normalizedModel
	case "codex":
		settingsPatch["defaultModelCodex"] = normalizedModel
	}
	if thread.Provider == "claude" {
		modelContexts := cloneModelContextWindows(a.currentSettings().ModelContextWindows)
		if modelContexts == nil {
			modelContexts = map[string]int{}
		}
		modelContexts[normalizedModel] = nextContextWindow
		settingsPatch["modelContextWindows"] = modelContexts
	}
	rollbackSettings, err := a.applySettingsPatchWithRollback(settingsPatch)
	if err != nil {
		return store.Thread{}, fmt.Errorf("remember model default: %w", err)
	}

	thread.Model = normalizedModel
	thread.ContextWindow = nextContextWindow
	if err := a.store.UpdateModelAndContextWindow(threadID, normalizedModel, nextContextWindow); err != nil {
		if rollbackErr := rollbackSettings(); rollbackErr != nil {
			return store.Thread{}, fmt.Errorf("update thread model: %w (settings rollback failed: %v)", err, rollbackErr)
		}
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
		if settingsRollbackErr := rollbackSettings(); settingsRollbackErr != nil {
			err = fmt.Errorf("restart session with updated model: %w (settings rollback failed: %v)", err, settingsRollbackErr)
		}
		if rollbackErr := a.store.UpdateModelAndContextWindow(threadID, previousModel, previousContextWindow); rollbackErr != nil {
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
