package app

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

type ContextSettingsProfile struct {
	Provider                   string                         `json:"provider"`
	Model                      string                         `json:"model"`
	ContextWindows             []provider.ContextWindowOption `json:"contextWindows"`
	ContextWindow              int                            `json:"contextWindow"`
	AutoCompactStandardPercent int                            `json:"autoCompactStandardPercent"`
	AutoCompactExtendedPercent int                            `json:"autoCompactExtendedPercent"`
}

type ContextSettingsUpdate struct {
	Provider                   string `json:"provider"`
	Model                      string `json:"model"`
	ContextWindow              int    `json:"contextWindow"`
	AutoCompactStandardPercent int    `json:"autoCompactStandardPercent"`
	AutoCompactExtendedPercent int    `json:"autoCompactExtendedPercent"`
}

//ao:scope settings:read
func (a *App) GetContextSettings(providerName, model string) (ContextSettingsProfile, error) {
	if a.store == nil {
		return ContextSettingsProfile{}, fmt.Errorf("context settings: store unavailable")
	}
	providerName = strings.TrimSpace(providerName)
	model = strings.TrimSpace(model)
	if providerName == "" || model == "" {
		return ContextSettingsProfile{}, fmt.Errorf("context settings: provider and model are required")
	}

	options := chatmodel.ContextWindowOptions(providerName, model)
	if len(options) == 0 {
		return ContextSettingsProfile{}, fmt.Errorf("context settings: unknown provider/model %s/%s", providerName, model)
	}

	profile, err := a.store.GetChatModelProfile(providerName, model)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return ContextSettingsProfile{}, fmt.Errorf("context settings: load profile: %w", err)
		}
		profile = chatmodel.FallbackProfile(providerName, model)
	}
	if !chatmodel.ContextWindowSupported(options, profile.ContextWindow) {
		profile.ContextWindow = chatmodel.DefaultContextWindow(providerName, model, 0)
	}

	return ContextSettingsProfile{
		Provider:                   providerName,
		Model:                      model,
		ContextWindows:             options,
		ContextWindow:              profile.ContextWindow,
		AutoCompactStandardPercent: profile.AutoCompactStandardPercent,
		AutoCompactExtendedPercent: profile.AutoCompactExtendedPercent,
	}, nil
}

//ao:scope settings:write
func (a *App) UpdateContextSettingsProfile(update ContextSettingsUpdate) (ContextSettingsProfile, error) {
	if a.store == nil {
		return ContextSettingsProfile{}, fmt.Errorf("update context settings profile: store unavailable")
	}
	providerName, model, err := chatmodel.ValidateContextUpdate(update.Provider, update.Model, update.ContextWindow, update.AutoCompactStandardPercent, update.AutoCompactExtendedPercent)
	if err != nil {
		return ContextSettingsProfile{}, err
	}

	profile, err := a.store.GetChatModelProfile(providerName, model)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return ContextSettingsProfile{}, fmt.Errorf("update context settings profile: load profile: %w", err)
		}
		profile = chatmodel.FallbackProfile(providerName, model)
	}
	profile.ContextWindow = update.ContextWindow
	profile.AutoCompactStandardPercent = update.AutoCompactStandardPercent
	profile.AutoCompactExtendedPercent = update.AutoCompactExtendedPercent
	if err := a.store.UpsertChatModelProfile(profile); err != nil {
		return ContextSettingsProfile{}, err
	}
	return a.GetContextSettings(providerName, model)
}

//ao:scope threads:operate
func (a *App) UpdateThreadContextSettings(threadID string, update ContextSettingsUpdate) (store.Thread, error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("update thread context settings: store unavailable")
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}

	update.Provider = thread.Provider
	update.Model = thread.Model
	if _, _, err := chatmodel.ValidateContextUpdate(update.Provider, update.Model, update.ContextWindow, update.AutoCompactStandardPercent, update.AutoCompactExtendedPercent); err != nil {
		return store.Thread{}, err
	}

	_, changed, err := a.store.UpdateContextSettings(threadID, update.ContextWindow, update.AutoCompactStandardPercent, update.AutoCompactExtendedPercent)
	if err != nil {
		return store.Thread{}, err
	}
	// Clear the persisted token-usage snapshot and notify the frontend
	// that the meter is no longer authoritative. The new context-window
	// max would otherwise be paired with stale `usedTokens` from the
	// previous setting until the next session emits a fresh reading
	// (and shrinking the window from 1M to 200k would briefly render
	// usage as a wildly wrong percentage). The session restart below
	// brings the new state online; once a turn runs the meter is
	// authoritative again.
	if err := a.store.ClearLastTokenUsage(threadID); err != nil {
		log.Printf("UpdateThreadContextSettings: clear last token usage: %v", err)
	}
	a.emit(eventchan.ProviderUsage, provider.UsageEvent{
		Action:   "reset",
		ThreadID: threadID,
	})
	refreshed, err := a.restartSessionIfAffected(threadID, "contextSettings")
	if err != nil {
		return store.Thread{}, err
	}
	a.rememberChatModelProfile(refreshed)
	a.broadcastThreadRowIfChanged(triage.ThreadActionFull, refreshed, changed)
	return refreshed, nil
}
