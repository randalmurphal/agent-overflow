package app

import (
	"database/sql"
	"errors"
	"log"
	"strings"

	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func (a *App) supportsFastModeForModel(providerName, model string) bool {
	candidate, found := a.modelInfoForProvider(providerName, model)
	supported := supportsFastModeFromModelInfo(providerName, model, candidate, found)
	if !supported && providerName == string(provider.Codex) && a.store != nil {
		if profile, ok := a.rememberedModelProfile(providerName, model); ok {
			return profile.FastMode
		}
	}
	return supported
}

func supportsFastModeFromModelInfo(
	providerName, model string,
	candidate provider.ModelInfo,
	found bool,
) bool {
	if found {
		return chatmodel.HasCapability(candidate, provider.ModelCapabilityFastMode)
	}
	return chatmodel.SupportsStoredFastMode(providerName, model)
}

// fastModeTierIDForModel resolves the wire service-tier id a fast-mode turn on
// this model must carry (Codex; Claude models never declare one). Empty means
// "no catalog opinion" — an unresolved slug, a catalog that could not be
// reached, or a provider without tiers — and the provider translator falls back
// to its legacy default rather than dropping fast mode.
func (a *App) fastModeTierIDForModel(providerName, model string) string {
	candidate, found := a.modelInfoForProvider(providerName, model)
	if !found || candidate.FastModeTier == nil {
		return ""
	}
	return candidate.FastModeTier.ID
}

func (a *App) reasoningEffortSupportedForModel(providerName, model, effort string) bool {
	candidate, found := a.modelInfoForProvider(providerName, model)
	if found {
		if provider.ModelInfoSupportsReasoningEffort(candidate, effort) {
			return true
		}
		if providerName == string(provider.Codex) && a.store != nil && string(provider.NormalizeReasoningEffort(effort)) == effort {
			if profile, ok := a.rememberedModelProfile(providerName, model); ok {
				return profile.ReasoningEffort == effort
			}
		}
		return false
	}
	return provider.ReasoningEffortSupportedForModel(providerName, model, effort)
}

func (a *App) coerceReasoningEffortForModel(providerName, model, effort string) string {
	if providerName == string(provider.Codex) && a.reasoningEffortSupportedForModel(providerName, model, effort) {
		return effort
	}
	candidate, found := a.modelInfoForProvider(providerName, model)
	return coerceReasoningEffortFromModelInfo(providerName, model, effort, candidate, found)
}

func coerceReasoningEffortFromModelInfo(
	providerName, model, effort string,
	candidate provider.ModelInfo,
	found bool,
) string {
	normalized := provider.NormalizeReasoningEffort(effort)
	if found {
		return string(provider.CoerceReasoningEffortForModelInfo(candidate, normalized))
	}
	return string(provider.CoerceReasoningEffortForModel(providerName, model, normalized))
}

// draftModelDefaults preserves remembered Codex choices and uses one local
// Claude catalog snapshot to resolve its model-derived settings.
func (a *App) draftModelDefaults(providerName, model, effort string, fastMode bool) (string, bool) {
	if providerName == string(provider.Codex) {
		return string(provider.NormalizeReasoningEffort(effort)), fastMode
	}
	candidate, found := a.modelInfoForProvider(providerName, model)
	effort = coerceReasoningEffortFromModelInfo(providerName, model, effort, candidate, found)
	fastMode = fastMode && supportsFastModeFromModelInfo(providerName, model, candidate, found)
	return effort, fastMode
}

// sanitizeChatModelProfile normalizes stored values without revoking Codex
// selections when catalog availability changes.
func (a *App) sanitizeChatModelProfile(profile store.ChatModelProfile) store.ChatModelProfile {
	profile = chatmodel.SanitizeProfile(profile)
	if profile.Provider == string(provider.Codex) {
		return profile
	}
	candidate, found := a.modelInfoForProvider(profile.Provider, profile.Model)
	if !found {
		return profile
	}

	profile.ReasoningEffort = string(provider.CoerceReasoningEffortForModelInfo(
		candidate,
		provider.NormalizeReasoningEffort(profile.ReasoningEffort),
	))
	profile.FastMode = profile.FastMode && chatmodel.HasCapability(candidate, provider.ModelCapabilityFastMode)
	if !chatmodel.ContextWindowSupported(candidate.ContextWindows, profile.ContextWindow) {
		if tokens, ok := provider.DefaultContextWindowForOptions(candidate.ContextWindows); ok {
			profile.ContextWindow = tokens
		}
	}
	return profile
}

// sanitizeThreadModelSettings normalizes model-derived settings while
// preserving accepted Codex effort and fast-mode selections.
//
// It deliberately does not touch thread.RuntimeMode. Runtime mode is the
// access/approval axis, not a model capability: it is chosen by the user per
// thread or by a workflow phase's `access` declaration, and no model catalog
// entry can legitimately override that choice. Callers therefore do not need
// to re-stamp RuntimeMode after calling this.
func (a *App) sanitizeThreadModelSettings(thread store.Thread) store.Thread {
	profile := a.sanitizeChatModelProfile(store.ChatModelProfile{
		Provider:        thread.Provider,
		Model:           thread.Model,
		ReasoningEffort: thread.ReasoningEffort,
		FastMode:        thread.FastMode,
		ContextWindow:   thread.ContextWindow,
		RuntimeMode:     thread.RuntimeMode,
	})
	thread.Model = profile.Model
	thread.ReasoningEffort = profile.ReasoningEffort
	thread.FastMode = profile.FastMode
	thread.ContextWindow = profile.ContextWindow
	return thread
}

// contextWindowOptionsForModel resolves known windows without provider I/O.
func (a *App) contextWindowOptionsForModel(providerName, model string) []provider.ContextWindowOption {
	candidate, found := a.modelInfoForProvider(providerName, model)
	if found && len(candidate.ContextWindows) > 0 {
		return candidate.ContextWindows
	}
	return chatmodel.ContextWindowOptions(providerName, model)
}

func (a *App) defaultContextWindowForModel(providerName, model string) int {
	return chatmodel.DefaultContextWindowFor(a.contextWindowOptionsForModel(providerName, model), providerName, model)
}

// fallbackChatModelProfile is the no-stored-opinion profile with its window
// resolved through the merged catalogs (see chatmodel.FallbackProfileWith).
func (a *App) fallbackChatModelProfile(providerName, model string, availableProviders ...string) store.ChatModelProfile {
	return chatmodel.FallbackProfileWith(a.contextWindowOptionsForModel, providerName, model, availableProviders...)
}

// modelInfoForProvider uses capability evidence already available locally.
// Catalog expiry, refresh failure and omission cannot revoke a known choice.
func (a *App) modelInfoForProvider(providerName, model string) (info provider.ModelInfo, found bool) {
	providerName = strings.TrimSpace(providerName)
	model = provider.NormalizeModelSlug(providerName, strings.TrimSpace(model))
	if provider.CapabilitiesForProvider(providerName).ModelCatalog == provider.CodexLiveModelCatalog {
		if candidate, ok := a.providerDiscoveryService().KnownCodexModel(a.providerBinaryPath(providerName), model); ok {
			return candidate, true
		}
	}
	return a.modelInfoWithoutLiveCodex(providerName, model)
}

func (a *App) modelInfoWithoutLiveCodex(providerName, model string) (info provider.ModelInfo, found bool) {
	if provider.CapabilitiesForProvider(providerName).ModelCatalog == provider.ClaudeProbeEnrichedCatalog {
		return findModelInCatalog(a.claudeModelsForProvider(providerName), model)
	}
	info, found = provider.FindModel(providerName, model)
	return info, found
}

func findModelInCatalog(models []provider.ModelInfo, model string) (provider.ModelInfo, bool) {
	for _, candidate := range models {
		if candidate.Slug == model {
			return candidate, true
		}
	}
	return provider.ModelInfo{}, false
}

func (a *App) rememberedModelProfile(providerName, model string) (store.ChatModelProfile, bool) {
	profile, err := a.store.GetChatModelProfile(providerName, model)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("model capabilities: read remembered %s/%s profile: %v", providerName, model, err)
	}
	return profile, err == nil
}
