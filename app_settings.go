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

func (a *App) applySettingsPatchWithRollback(patch map[string]any) (func() error, error) {
	if a.settings == nil || len(patch) == 0 {
		return func() error { return nil }, nil
	}

	previous := a.settings.Get()
	rollbackPatch := settingsRollbackPatch(previous, patch)
	if _, err := a.settings.Update(patch); err != nil {
		return nil, err
	}

	return func() error {
		if len(rollbackPatch) == 0 {
			return nil
		}
		_, err := a.settings.Update(rollbackPatch)
		return err
	}, nil
}

func settingsRollbackPatch(previous settings.Settings, patch map[string]any) map[string]any {
	rollback := make(map[string]any, len(patch))
	for key := range patch {
		switch key {
		case "defaultProvider":
			rollback[key] = previous.DefaultProvider
		case "defaultModelClaude":
			rollback[key] = previous.DefaultModelClaude
		case "defaultModelCodex":
			rollback[key] = previous.DefaultModelCodex
		case "defaultRuntimeMode":
			rollback[key] = previous.DefaultRuntimeMode
		case "defaultReasoningEffort":
			rollback[key] = previous.DefaultReasoningEffort
		case "defaultFastMode":
			rollback[key] = previous.DefaultFastMode
		case "defaultContextWindow":
			rollback[key] = previous.DefaultContextWindow
		case "modelContextWindows":
			rollback[key] = cloneModelContextWindows(previous.ModelContextWindows)
		}
	}
	return rollback
}

func cloneModelContextWindows(values map[string]int) map[string]int {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]int, len(values))
	for model, tokens := range values {
		cloned[model] = tokens
	}
	return cloned
}
