package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/chatmodel"
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
	trimmedModel := strings.TrimSpace(model)
	if trimmedModel == "" {
		return store.Thread{}, fmt.Errorf("thread model cannot be empty")
	}

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}
	normalizedModel := provider.NormalizeModelSlug(thread.Provider, trimmedModel)
	if thread.Model == normalizedModel {
		a.rememberChatModelProfile(thread)
		return thread, nil
	}

	profile, err := a.chatModelProfileForSelection(thread.Provider, normalizedModel)
	if err != nil {
		return store.Thread{}, err
	}
	return a.updateThreadFromChatModelProfile(thread, profile, "model")
}

// UpdateThreadModelSelection changes provider + model as one atomic model-menu
// selection. The selected provider/model's remembered profile is applied before
// the thread row is persisted, so SQLite never sees an invalid intermediate
// provider/effort pair such as codex + max.
func (a *App) UpdateThreadModelSelection(threadID string, providerName string, model string) (threadResult store.Thread, err error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("update model selection: store unavailable")
	}
	providerName = strings.TrimSpace(providerName)
	if providerName == "" {
		return store.Thread{}, fmt.Errorf("thread provider cannot be empty")
	}
	if !validThreadProvider(providerName) {
		return store.Thread{}, fmt.Errorf("%w: %q", store.ErrInvalidProvider, providerName)
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return store.Thread{}, fmt.Errorf("thread model cannot be empty")
	}

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}
	normalizedModel := provider.NormalizeModelSlug(providerName, model)
	if thread.Provider == providerName && thread.Model == normalizedModel {
		a.rememberChatModelProfile(thread)
		return thread, nil
	}
	profile, err := a.chatModelProfileForSelection(providerName, normalizedModel)
	if err != nil {
		return store.Thread{}, err
	}
	return a.updateThreadFromChatModelProfile(thread, profile, "model")
}

func validThreadProvider(providerName string) bool {
	switch providerName {
	case string(provider.Claude), string(provider.Codex):
		return true
	default:
		return false
	}
}

func (a *App) chatModelProfileForSelection(providerName, model string) (store.ChatModelProfile, error) {
	model = provider.NormalizeModelSlug(providerName, strings.TrimSpace(model))
	if a.store != nil {
		profile, err := a.store.GetChatModelProfile(providerName, model)
		if err == nil {
			return chatmodel.SanitizeProfile(profile), nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return store.ChatModelProfile{}, fmt.Errorf("load chat model profile: %w", err)
		}
	}
	return chatmodel.FallbackProfile(providerName, model), nil
}

func (a *App) latestProviderProfileForSelection(providerName string) (store.ChatModelProfile, error) {
	if a.store != nil {
		profile, err := a.store.LatestChatModelProfileForProvider(providerName)
		if err == nil {
			return chatmodel.SanitizeProfile(profile), nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return store.ChatModelProfile{}, fmt.Errorf("load latest chat model profile for provider %s: %w", providerName, err)
		}
	}
	return chatmodel.FallbackProfile(providerName, ""), nil
}

func threadWithModelSelectionProfile(thread store.Thread, profile store.ChatModelProfile) store.Thread {
	profile = chatmodel.SanitizeProfile(profile)
	providerChanged := thread.Provider != profile.Provider
	thread.Provider = profile.Provider
	thread.Model = profile.Model
	thread.ReasoningEffort = profile.ReasoningEffort
	thread.FastMode = profile.FastMode
	thread.ContextWindow = profile.ContextWindow
	thread.RuntimeMode = string(provider.NormalizeRuntimeMode(profile.RuntimeMode))
	// Switching models clears any per-thread compact override so the new
	// session picks up the live per-provider Settings value. The user can
	// re-override this thread via the chat-meter edit flow.
	thread.AutoCompactStandardPercent = 0
	thread.AutoCompactExtendedPercent = 0
	if providerChanged {
		thread.SessionRef = ""
		thread.PendingForkRef = ""
	}
	return thread
}

func (a *App) updateThreadFromChatModelProfile(previous store.Thread, profile store.ChatModelProfile, changedField string) (threadResult store.Thread, err error) {
	thread := threadWithModelSelectionProfile(previous, profile)
	if err := a.updateThreadForModelSelection(previous, thread); err != nil {
		if errors.Is(err, store.ErrThreadProviderLocked) {
			return store.Thread{}, fmt.Errorf("update provider: thread is locked to %s (start a new thread to use %s)", previous.Provider, thread.Provider)
		}
		return store.Thread{}, err
	}

	active := a.hasActiveSession(thread.ID)
	if !active {
		a.rememberChatModelProfile(thread)
		thread.UpdatedAt = a.mustLoadThreadUpdatedAt(thread.ID, thread.UpdatedAt)
		return thread, nil
	}

	defer func() {
		if err == nil {
			return
		}
		if rollbackErr := a.store.UpdateThread(previous); rollbackErr != nil {
			err = fmt.Errorf("restart session with updated %s: %w (rollback failed: %v)", changedField, err, rollbackErr)
		}
	}()

	if err = a.startSession(thread.ID); err != nil {
		return store.Thread{}, fmt.Errorf("restart session with updated %s: %w", changedField, err)
	}

	updated, err := a.store.GetThread(thread.ID)
	if err != nil {
		return store.Thread{}, err
	}
	a.rememberChatModelProfile(updated)
	return updated, nil
}

func (a *App) updateThreadForModelSelection(previous, updated store.Thread) error {
	if previous.Provider == updated.Provider {
		return a.store.UpdateThread(updated)
	}
	return a.store.UpdateThreadIfProviderSwitchAllowed(updated, previous.Provider)
}

func (a *App) hasActiveSession(threadID string) bool {
	_, ok := a.sessionManager().get(threadID)
	return ok
}

func (a *App) mustLoadThreadUpdatedAt(threadID string, fallback int64) int64 {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return fallback
	}
	return thread.UpdatedAt
}
