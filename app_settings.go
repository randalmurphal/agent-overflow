package main

import (
	"context"
	"encoding/json"
	"errors"
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

// GetSettings returns the current persisted settings merged over defaults,
// with every secret redacted (see redactedSettings).
func (a *App) GetSettings() (settings.Settings, error) {
	return redactedSettings(a.currentSettings()), nil
}

// redactedSettings is the projection every bound method returning a full
// Settings value goes through.
//
// SECURITY: RemoteEndpoints[*].Token and the values of custom environment
// variables flagged sensitive are cleared. A LAN-attached token-holder calling
// GetSettings must not be able to harvest credentials — without this, a single
// call enumerates every saved token, defeating the on-demand fetch model that
// ListRemoteEndpoints + GetRemoteEndpointToken were designed to enforce.
// Callers that need an actual token (the "Copy launch command" affordance)
// fetch it through GetRemoteEndpointToken, which is a logged single-record
// lookup; a sensitive environment value has no read path at all — the UI
// overwrites it by re-entry.
//
// Both fields are copied before clearing: settings.Service.Get returns a value
// copy of Settings, but its slices share backing memory with the service's
// cache, so clearing in place would corrupt every later reader.
func redactedSettings(current settings.Settings) settings.Settings {
	if len(current.RemoteEndpoints) > 0 {
		redacted := make([]settings.RemoteEndpoint, len(current.RemoteEndpoints))
		for i, ep := range current.RemoteEndpoints {
			redacted[i] = ep
			redacted[i].Token = ""
		}
		current.RemoteEndpoints = redacted
	}
	current.ClaudeCustomEnv = settings.RedactProviderEnvVars(current.ClaudeCustomEnv)
	current.CodexCustomEnv = settings.RedactProviderEnvVars(current.CodexCustomEnv)
	return current
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
//
// The returned snapshot is redacted like GetSettings': the frontend store
// re-seeds from it, and the two read paths must not disagree about whether the
// store holds a plaintext secret. (Nothing consumes the secrets from here —
// tokens are fetched through GetRemoteEndpointToken and sensitive environment
// values have no read path at all.)
func (a *App) UpdateSettings(patch map[string]any) (settings.Settings, error) {
	if a.settings == nil {
		return settings.Settings{}, fmt.Errorf("settings service unavailable")
	}
	prev := a.settings.Get()
	next, err := a.settings.Update(patch)
	if err != nil {
		return settings.Settings{}, err
	}
	if _, workflowPausedChanged := patch["workflowPaused"]; a.workflowEngine != nil && workflowPausedChanged {
		if err := a.workflowEngine.PauseDetachedStarts(next.WorkflowPaused); err != nil {
			rollback, rollbackBuildErr := settingsRollbackPatch(prev, patch)
			var rollbackErr error
			if rollbackBuildErr == nil {
				_, rollbackErr = a.settings.Update(rollback)
			}
			return settings.Settings{}, errors.Join(err, rollbackBuildErr, rollbackErr)
		}
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
	if _, ok := patch["gitlabSelfHostedHosts"]; ok {
		// The forge classifier reads from a Core-held snapshot; push
		// the new list and drop every cached cwd classification so
		// the next git-status refresh reclassifies. Without
		// invalidation, repos whose origin matches the new allowlist
		// would stay classified as "" until forgeDetectionTTL
		// expired (up to 5 minutes).
		if a.git != nil {
			a.git.SetGitLabHosts(next.GitLabSelfHostedHosts)
			a.git.InvalidateAllForgeCache()
		}
	}
	return redactedSettings(next), nil
}

func settingsRollbackPatch(previous settings.Settings, patch map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(previous)
	if err != nil {
		return nil, fmt.Errorf("settings rollback: encode previous settings: %w", err)
	}
	var values map[string]any
	if err := json.Unmarshal(encoded, &values); err != nil {
		return nil, fmt.Errorf("settings rollback: decode previous settings: %w", err)
	}
	rollback := make(map[string]any, len(patch))
	for key := range patch {
		value, ok := values[key]
		if !ok {
			return nil, fmt.Errorf("settings rollback: previous value for %q is unavailable", key)
		}
		rollback[key] = value
	}
	return rollback, nil
}

// GetModelsForProvider returns the known model registry for the given provider.
//
// Each provider's catalog source is declared by its Capabilities, not by a
// name check here: Codex's list comes live off `model/list` (one CLI spawn,
// TTL-cached), Claude's is the shipped catalog enriched by whatever the last
// zero-token account probe reported (never a spawn of its own), and anything
// else is the shipped list verbatim.
func (a *App) GetModelsForProvider(providerName string) ([]provider.ModelInfo, error) {
	switch provider.CapabilitiesForProvider(providerName).ModelCatalog {
	case provider.CodexLiveModelCatalog:
		return a.codexModelsForBinary(context.Background(), a.providerBinaryPath(providerName))
	case provider.ClaudeProbeEnrichedCatalog:
		return a.claudeModelsForProvider(providerName), nil
	default:
		return provider.ModelsForProvider(providerName), nil
	}
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
	// Harness mode pins every provider spawn to ao-mockprovider,
	// regardless of what the (mutable) settings say — see the field doc.
	if a.providerBinaryOverride != "" {
		return a.providerBinaryOverride
	}
	cfg := a.currentSettings()

	switch providerName {
	case string(provider.Claude), string(provider.ClaudeTUI):
		// The interactive TUI provider drives the same `claude` binary as the
		// headless one — one binary setting backs both.
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
