package app

import (
	"context"
	"fmt"
	"strings"

	"agent-overflow/internal/editor"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
)

// GetProviderStatuses reports provider binary availability using the configured paths.
// Also pushes a `provider:status` event per non-ready provider so thread-level
// banners (ProviderStatusBanner) stay in sync with the settings page without
// polling. Idempotent: re-emitting the same state is harmless — the frontend
// keeps only the latest per-provider entry.
//
//ao:scope access:admin
//ao:route home
func (a *App) GetProviderStatuses() ([]provider.ProviderStatus, error) {
	return a.providerDiscoveryService().ProviderStatuses(), nil
}

func (a *App) currentSettings() settings.Settings {
	if a.settings == nil {
		return settings.DefaultSettings
	}
	return a.settings.Get()
}

// GetSettings returns the current persisted settings merged over defaults,
// with every secret redacted (see redactedSettings).
//
// Resolved PER CALLER. The host and user tiers are global to this backend;
// the device tier comes out of the calling connection's own ui_state bucket
// over its DEVICE-CLASS defaults (docs/specs/remote-access.md §6,
// internal/settings/classdefaults.go), so two screens attached to one backend
// see two font sizes and one shared set of confirmations, and a paired phone
// that never touched lowPowerMode reads it on. A caller with no device behind
// it — a background saga, a test — reads the desktop class's defaults.
//
//ao:scope settings:read
//ao:route home
func (a *App) GetSettings(ctx context.Context) (settings.Settings, error) {
	if a.settings == nil {
		return settings.DefaultSettings, nil
	}
	caller, err := a.settingsCaller(ctx)
	if err != nil {
		return settings.Settings{}, err
	}
	return redactedSettings(caller.Get()), nil
}

// redactedSettings is the projection every bound method returning a full
// Settings value goes through.
//
// SECURITY: the values of custom environment variables flagged sensitive
// are cleared. A LAN-attached token-holder calling GetSettings must not be
// able to harvest credentials — a sensitive environment value has no read
// path at all, and the UI overwrites it by re-entry.
//
// The values are copied before clearing: settings.Service.Get returns a value
// copy of Settings, but its maps share backing memory with the service's
// cache, so clearing in place would corrupt every later reader.
func redactedSettings(current settings.Settings) settings.Settings {
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
// Each key is routed to its own tier's storage — settings.json for host keys,
// the `user:default` ui_state scope for user keys, the CALLING connection's
// bucket for device keys (docs/specs/remote-access.md §6). Validation runs on
// the whole merged struct first, so every key is validated the same way
// wherever it ends up.
//
// The patch is applied over the caller's CLASS-RESOLVED view, which is what
// lets a device write the opposite of its class default: a phone patching
// lowPowerMode to false is a change from the true its class resolves to, so
// it persists a row and outranks the table from then on.
//
// The returned snapshot is redacted like GetSettings': the frontend store
// re-seeds from it, and the two read paths must not disagree about whether the
// store holds a plaintext secret. (Nothing consumes the secrets from here —
// tokens are fetched through GetRemoteEndpointToken and sensitive environment
// values have no read path at all.)
//
// The scope below is the FLOOR, and it is the floor VALUE: one method carries
// all three of §6's tiers, so the only honest thing its name can require is a
// live session. requireSettingsTier is where the real answer is decided, per
// key — device keys ride the session, user keys need settings:write, host keys
// need a fresh step-up proof.
//
//ao:scope session
//ao:route home
func (a *App) UpdateSettings(ctx context.Context, patch map[string]any) (settings.Settings, error) {
	if a.settings == nil {
		return settings.Settings{}, fmt.Errorf("settings service unavailable")
	}
	if err := a.requireSettingsTier(ctx, patch); err != nil {
		return settings.Settings{}, err
	}
	caller, err := a.settingsCaller(ctx)
	if err != nil {
		return settings.Settings{}, err
	}
	prev := a.settings.Get()
	next, err := caller.Update(patch)
	if err != nil {
		return settings.Settings{}, err
	}
	if patchTouchesLiveClaudeAxis(patch) {
		// The settings-owned session axes a LIVE session reacts to. They
		// converge three different ways and each needs its own trigger,
		// because nothing else reconciles on a settings save — the
		// remaining axes here are spawn-only with no live consequence at
		// all, so without this the change would sit in the file until some
		// unrelated thread-config edit happened to reconcile.
		//
		//   - claudeThinking is adopted outright over a control request
		//     (set_max_thinking_tokens), except for the return to "Claude
		//     Code decides", which has no wire form and defers a restart.
		//   - claudeCrossSession binds once at spawn, so it can only
		//     converge by a DEFERRED restart the reconciler queues.
		//   - claudePromptOverrides is the headline live apply:
		//     reconcileSettingsOwnedAxes RESOLVES the override rather than
		//     pinning it, so an edited-or-enabled entry lands live as
		//     set_model.system_prompt and a disabled one defers a restart.
		//     Without this key the axis that motivated the whole live path
		//     would never fire from the save that causes it.
		//
		// codexPromptOverrides is deliberately NOT a trigger:
		// reconcileSettingsOwnedAxes PINS the prompt on every non-Claude
		// provider (no set_model.system_prompt wire there), so the sweep
		// could only be a no-op. Codex converges on its next spawn, which
		// is the contract docs/specs/prompt-tool-overrides.md states.
		//
		// Off the binding's goroutine: reconciling N threads means N control
		// requests, each with its own timeout, and a wedged provider process
		// must not hold up the save the user just made.
		a.scheduleLiveClaudeReconcile()
	}
	if patchTouchesKeepAwake(patch) {
		// Keep awake applies live, with no restart: re-derive the mode
		// from the saved snapshot and push it at both this process's OS
		// and the launcher that owns the host's power state. See
		// app_power.go for why one mode string rather than the two keys.
		a.applyKeepAwake(next)
	}
	if patchTouchesBrowserSettings(patch) {
		// Disabling revokes tool calls synchronously; enabling publishes the
		// tools after the manager accepts the new config. Process teardown and
		// provider refresh remain asynchronous and require no provider restart.
		a.scheduleBrowserSettings(next)
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

// liveClaudeSettingsAxes are the settings keys whose save must converge
// already-running Claude sessions. Kept as one list so the trigger and the
// reconciler cannot drift; see the call site for how each one converges.
var liveClaudeSettingsAxes = [...]string{
	"claudeThinking",
	"claudeCrossSession",
	"claudePromptOverrides",
}

// patchTouchesLiveClaudeAxis reports whether an UpdateSettings patch names
// any key a live Claude session reacts to. Presence is what counts, not the
// value: clearing an axis converges exactly as setting one does.
func patchTouchesLiveClaudeAxis(patch map[string]any) bool {
	for _, key := range liveClaudeSettingsAxes {
		if _, ok := patch[key]; ok {
			return true
		}
	}
	return false
}

// GetModelsForProvider returns the known model registry for the given provider.
//
// Each provider's catalog source is declared by its Capabilities, not by a
// name check here: Codex's list comes live off `model/list` (one CLI spawn,
// TTL-cached), Claude's is the shipped catalog enriched by whatever the last
// zero-token account probe reported (never a spawn of its own), and anything
// else is the shipped list verbatim.
//
//ao:scope threads:operate
//ao:route selected
func (a *App) GetModelsForProvider(providerName string) ([]provider.ModelInfo, error) {
	return a.providerDiscoveryService().ModelsForProvider(context.Background(), providerName)
}

func (a *App) refreshCodexModelCatalog() {
	a.providerDiscoveryService().RefreshCodexModelCatalog()
}

func (a *App) codexModelsForBinary(ctx context.Context, binary string) ([]provider.ModelInfo, error) {
	return a.providerDiscoveryService().CodexModelsForBinary(ctx, binary)
}

func (a *App) cachedCodexModelsForBinary(binary string) ([]provider.ModelInfo, error, bool) {
	return a.providerDiscoveryService().CachedCodexModelsForBinary(binary)
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
