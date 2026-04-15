package main

import (
	"fmt"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
)

// GetProviderStatuses reports provider binary availability using the configured paths.
func (a *App) GetProviderStatuses() ([]provider.ProviderStatus, error) {
	cfg := a.currentSettings()
	statuses := []provider.ProviderStatus{
		provider.DetectProvider(string(provider.Claude), cfg.ClaudeBinaryPath),
		provider.DetectProvider(string(provider.Codex), cfg.CodexBinaryPath),
	}
	return statuses, nil
}

func (a *App) currentSettings() settings.Settings {
	if a.settings == nil {
		return settings.DefaultSettings
	}
	return a.settings.Get()
}

// GetSettings returns the current persisted settings merged over defaults.
func (a *App) GetSettings() (settings.Settings, error) {
	return a.currentSettings(), nil
}

// UpdateSettings applies a partial settings patch and persists it.
func (a *App) UpdateSettings(patch map[string]any) (settings.Settings, error) {
	if a.settings == nil {
		return settings.Settings{}, fmt.Errorf("settings service unavailable")
	}
	return a.settings.Update(patch)
}

// GetModelsForProvider returns the known model registry for the given provider.
func (a *App) GetModelsForProvider(providerName string) ([]provider.ModelInfo, error) {
	return provider.ModelsForProvider(providerName), nil
}
