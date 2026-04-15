package main

import (
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
