package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
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
	return a.updateThreadFromChatModelProfile(thread, profile)
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
	return a.updateThreadFromChatModelProfile(thread, profile)
}

func validThreadProvider(providerName string) bool {
	switch providerName {
	case string(provider.Claude), string(provider.Codex), string(provider.ClaudeTUI):
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
			return a.sanitizeChatModelProfile(profile), nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return store.ChatModelProfile{}, fmt.Errorf("load chat model profile: %w", err)
		}
	}
	return a.sanitizeChatModelProfile(chatmodel.FallbackProfile(providerName, model)), nil
}

func (a *App) latestProviderProfileForSelection(providerName string) (store.ChatModelProfile, error) {
	if a.store != nil {
		profile, err := a.store.LatestChatModelProfileForProvider(providerName)
		if err == nil {
			return a.sanitizeChatModelProfile(profile), nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return store.ChatModelProfile{}, fmt.Errorf("load latest chat model profile for provider %s: %w", providerName, err)
		}
	}
	return a.sanitizeChatModelProfile(chatmodel.FallbackProfile(providerName, "")), nil
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
		// The lazy-fork pin is a PAIR (ref + cut) consumed together, so it
		// clears together: a cut left behind names a position in a
		// transcript the new provider will never read.
		thread.PendingForkRef = ""
		thread.PendingForkResumeAt = ""
	}
	return thread
}

func (a *App) updateThreadFromChatModelProfile(previous store.Thread, profile store.ChatModelProfile) (store.Thread, error) {
	thread := threadWithModelSelectionProfile(previous, profile)
	if err := a.updateThreadForModelSelection(previous, thread); err != nil {
		if errors.Is(err, store.ErrThreadProviderLocked) {
			return store.Thread{}, fmt.Errorf("update provider: thread is locked to %s (start a new thread to use %s)", previous.Provider, thread.Provider)
		}
		return store.Thread{}, err
	}

	// A provider switch dropped SessionRef above: the next session starts
	// from scratch, so the old provider's todo list must not survive into
	// it — a stale threads.live_todo would collide with the new session's
	// per-session task ids (triage.seedTasksFromStoredTodo). Same posture
	// as the reconciler below: the persisted selection is authoritative, a
	// failed cleanup is reported, never a rollback of the switch.
	if previous.Provider != thread.Provider && a.triage != nil {
		if err := a.triage.ResetThreadTodo(thread.ID); err != nil {
			log.Printf("thread %s: reset todo list on provider switch: %v", thread.ID, err)
		}
	}

	// Reconcile any live session with the new profile: model, effort, fast
	// mode, and (absent an autocompact override) context window all apply
	// live on both providers (Claude set_model + /effort + /fast, Codex
	// per-turn override); profile diffs that touch spawn-only config
	// restart the session, deferred while the thread is busy. Failures
	// surface as thread error state from the reconciler rather than
	// rolling back the row — the persisted selection stays authoritative
	// and the next (lazy) start converges on it.
	a.reconcileSessionConfig(thread.ID)

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
	// The guarded switch also clears the lazy-fork pin in the same UPDATE —
	// the pin is absent from the whole-row SET list, and a stale ref would
	// point into the OLD provider's session files.
	return a.store.UpdateThreadIfProviderSwitchAllowed(updated, previous.Provider)
}

func (a *App) hasActiveSession(threadID string) bool {
	_, ok := a.sessionManager().get(threadID)
	return ok
}
