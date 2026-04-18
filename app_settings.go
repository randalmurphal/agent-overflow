package main

import (
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
)

// GetProviderStatuses reports provider binary availability using the configured paths.
// Also pushes a `provider:status` event per non-ready provider so thread-level
// banners (ProviderStatusBanner) stay in sync with the settings page without
// polling. Idempotent: re-emitting the same state is harmless — the frontend
// keeps only the latest per-provider entry.
func (a *App) GetProviderStatuses() ([]provider.ProviderStatus, error) {
	cfg := a.currentSettings()
	statuses := []provider.ProviderStatus{
		provider.DetectProvider(string(provider.Claude), cfg.ClaudeBinaryPath),
		provider.DetectProvider(string(provider.Codex), cfg.CodexBinaryPath),
	}
	a.emitProviderStatusesFromDetect(statuses)
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

// UpdateSettings applies a partial settings patch and persists it. Observability
// toggles that can flip at runtime (e.g. replay log) are reconciled here.
// Tracing changes are persisted but require a restart to take effect — the
// UI shows a banner when that path is taken.
func (a *App) UpdateSettings(patch map[string]any) (settings.Settings, error) {
	if a.settings == nil {
		return settings.Settings{}, fmt.Errorf("settings service unavailable")
	}
	prev := a.settings.Get()
	next, err := a.settings.Update(patch)
	if err != nil {
		return settings.Settings{}, err
	}
	a.ReconfigureObservability(prev, next)
	return next, nil
}

// GetModelsForProvider returns the known model registry for the given provider.
func (a *App) GetModelsForProvider(providerName string) ([]provider.ModelInfo, error) {
	return provider.ModelsForProvider(providerName), nil
}

func (a *App) providerBinaryPath(providerName string) string {
	cfg := a.currentSettings()

	switch providerName {
	case string(provider.Claude):
		if path := strings.TrimSpace(cfg.ClaudeBinaryPath); path != "" {
			return path
		}
		return settings.DefaultSettings.ClaudeBinaryPath
	case string(provider.Codex):
		if path := strings.TrimSpace(cfg.CodexBinaryPath); path != "" {
			return path
		}
		return settings.DefaultSettings.CodexBinaryPath
	default:
		return ""
	}
}
