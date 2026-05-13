package main

import (
	"context"
	"fmt"
	"strings"

	"agent-overflow/internal/codexmodels"
	"agent-overflow/internal/editor"
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
//
// SECURITY: RemoteEndpoints[*].Token is redacted to the empty string
// before returning. A LAN-attached token-holder calling GetSettings
// must not be able to harvest credentials for other backends — without
// this redaction, a single GetSettings call enumerates every saved
// token, defeating the on-demand token fetch model that
// ListRemoteEndpoints + GetRemoteEndpointToken were designed to
// enforce. Callers that need an actual token (the "Copy launch
// command" affordance) fetch it through GetRemoteEndpointToken, which
// is a logged single-record lookup.
func (a *App) GetSettings() (settings.Settings, error) {
	current := a.currentSettings()
	if len(current.RemoteEndpoints) > 0 {
		// Defensive copy of the slice + each record so we don't mutate
		// the cached settings struct. The settings.Service.Get returns
		// a value copy of Settings, but the RemoteEndpoints slice
		// shares backing memory with the cache; clearing tokens in
		// place would corrupt the cache for subsequent callers.
		redacted := make([]settings.RemoteEndpoint, len(current.RemoteEndpoints))
		for i, ep := range current.RemoteEndpoints {
			redacted[i] = ep
			redacted[i].Token = ""
		}
		current.RemoteEndpoints = redacted
	}
	return current, nil
}

// UpdateSettings applies a partial settings patch and persists it. Observability
// toggles that can flip at runtime (e.g. replay log) are reconciled here.
// Tracing changes are persisted but require a restart to take effect — the
// UI shows a banner when that path is taken.
//
// When the patch touches "editor", the catalog detection cache is
// invalidated so the next ListAvailableEditors call surfaces fresh
// state. The dedicated SetEditorSettings binding does the same; this
// generic path needs the parallel hook because the frontend Settings
// panel writes through UpdateSettings for any field — a stale cache
// after a generic update would leave the picker showing the wrong
// availability flags until the next app launch.
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
	if _, ok := patch["editor"]; ok {
		// Editor preference touched — drop the cached catalog so the
		// picker doesn't surface stale availability. Same invalidation
		// the dedicated SetEditorSettings path runs.
		editor.RefreshEditors()
	}
	if _, ok := patch["codexBinaryPath"]; ok {
		a.refreshCodexModelCatalog()
	}
	return next, nil
}

// GetModelsForProvider returns the known model registry for the given provider.
func (a *App) GetModelsForProvider(providerName string) ([]provider.ModelInfo, error) {
	if providerName == string(provider.Codex) {
		return a.codexModelsForBinary(context.Background(), a.providerBinaryPath(providerName))
	}
	return provider.ModelsForProvider(providerName), nil
}

func (a *App) refreshCodexModelCatalog() {
	a.codexModels().Reset()
}

func (a *App) codexModelsForBinary(ctx context.Context, binary string) ([]provider.ModelInfo, error) {
	binary = strings.TrimSpace(binary)
	if binary == "" {
		binary = settings.DefaultSettings.CodexBinaryPath
	}
	return a.codexModels().Get(ctx, binary)
}

func (a *App) codexModels() *codexmodels.Cache {
	a.codexModelCatalogOnce.Do(func() {
		a.codexModelCatalog = codexmodels.New()
	})
	return a.codexModelCatalog
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
