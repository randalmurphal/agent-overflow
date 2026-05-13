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
		profile.ContextWindow = chatmodel.DefaultContextWindow(providerName, model, options[0].Tokens)
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

func (a *App) UpdateContextSettingsProfile(update ContextSettingsUpdate) (ContextSettingsProfile, error) {
	if a.store == nil {
		return ContextSettingsProfile{}, fmt.Errorf("update context settings profile: store unavailable")
	}
	providerName, model, contextWindow, standardPercent, extendedPercent, err := validateContextSettingsUpdate(update)
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
	profile.ContextWindow = contextWindow
	profile.AutoCompactStandardPercent = standardPercent
	profile.AutoCompactExtendedPercent = extendedPercent
	if err := a.store.UpsertChatModelProfile(profile); err != nil {
		return ContextSettingsProfile{}, err
	}
	return a.GetContextSettings(providerName, model)
}

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
	_, _, contextWindow, standardPercent, extendedPercent, err := validateContextSettingsUpdate(update)
	if err != nil {
		return store.Thread{}, err
	}

	if err := a.store.UpdateContextSettings(threadID, contextWindow, standardPercent, extendedPercent); err != nil {
		return store.Thread{}, err
	}
	refreshed, err := a.restartSessionIfAffected(threadID, "contextSettings")
	if err != nil {
		return store.Thread{}, err
	}
	a.rememberChatModelProfile(refreshed)
	return refreshed, nil
}

func validateContextSettingsUpdate(update ContextSettingsUpdate) (string, string, int, int, int, error) {
	providerName := strings.TrimSpace(update.Provider)
	model := strings.TrimSpace(update.Model)
	if providerName == "" || model == "" {
		return "", "", 0, 0, 0, fmt.Errorf("context settings: provider and model are required")
	}
	options := chatmodel.ContextWindowOptions(providerName, model)
	if len(options) == 0 {
		return "", "", 0, 0, 0, fmt.Errorf("context settings: unknown provider/model %s/%s", providerName, model)
	}
	if !chatmodel.ContextWindowSupported(options, update.ContextWindow) {
		return "", "", 0, 0, 0, fmt.Errorf("context settings: unsupported context window %d for %s/%s", update.ContextWindow, providerName, model)
	}
	if update.AutoCompactStandardPercent < 0 || update.AutoCompactStandardPercent > 90 {
		return "", "", 0, 0, 0, fmt.Errorf("context settings: standard auto-compact percent must be between 0 and 90")
	}
	if update.AutoCompactExtendedPercent < 0 || update.AutoCompactExtendedPercent > 90 {
		return "", "", 0, 0, 0, fmt.Errorf("context settings: extended auto-compact percent must be between 0 and 90")
	}
	return providerName, model, update.ContextWindow, update.AutoCompactStandardPercent, update.AutoCompactExtendedPercent, nil
}
