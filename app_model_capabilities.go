package main

import (
	"strings"

	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func (a *App) supportsFastModeForModel(providerName, model string) bool {
	candidate, found, catalogAuthoritative := a.modelInfoForProvider(providerName, model)
	if found {
		return chatmodel.HasCapability(candidate, provider.ModelCapabilityFastMode)
	}
	if catalogAuthoritative {
		return false
	}
	return chatmodel.SupportsStoredFastMode(providerName, model)
}

// fastModeTierIDForModel resolves the wire service-tier id a fast-mode turn on
// this model must carry (Codex; Claude models never declare one). Empty means
// "no catalog opinion" — an unresolved slug, a catalog that could not be
// reached, or a provider without tiers — and the provider translator falls back
// to its legacy default rather than dropping fast mode.
func (a *App) fastModeTierIDForModel(providerName, model string) string {
	candidate, found, _ := a.modelInfoForProvider(providerName, model)
	if !found || candidate.FastModeTier == nil {
		return ""
	}
	return candidate.FastModeTier.ID
}

func (a *App) reasoningEffortSupportedForModel(providerName, model, effort string) bool {
	candidate, found, catalogAuthoritative := a.modelInfoForProvider(providerName, model)
	if found {
		return provider.ModelInfoSupportsReasoningEffort(candidate, effort)
	}
	if catalogAuthoritative {
		return false
	}
	return provider.ReasoningEffortSupportedForModel(providerName, model, effort)
}

func (a *App) coerceReasoningEffortForModel(providerName, model, effort string) string {
	candidate, found, _ := a.modelInfoForProvider(providerName, model)
	normalized := provider.NormalizeReasoningEffort(effort)
	if found {
		return string(provider.CoerceReasoningEffortForModelInfo(candidate, normalized))
	}
	return string(provider.CoerceReasoningEffortForModel(providerName, model, normalized))
}

// sanitizeChatModelProfile applies the pure static-registry sanitation first,
// then lets a successful live Codex catalog override effort and Fast support.
// A catalog failure deliberately leaves the static fallback intact.
func (a *App) sanitizeChatModelProfile(profile store.ChatModelProfile) store.ChatModelProfile {
	profile = chatmodel.SanitizeProfile(profile)
	candidate, found, catalogAuthoritative := a.modelInfoForProvider(profile.Provider, profile.Model)
	if !found {
		if catalogAuthoritative {
			profile.FastMode = false
		}
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

// sanitizeThreadModelSettings coerces the thread's *model-derived* settings
// (model slug, reasoning effort, fast mode, context window) to what the
// resolved model actually supports.
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

// modelInfoForProvider resolves one model against the best catalog the
// provider has. The final return value distinguishes a successful catalog miss
// from a catalog error: a miss is an authoritative rejection, while an error
// permits the bundled registry to keep the app usable when the provider cannot
// be reached.
//
// Only Codex's catalog is ever authoritative. Claude's probe-enriched catalog
// is a SUPERSET of the shipped one (the CLI's list is a picker shortlist that
// omits older-but-usable models), so a miss there says nothing beyond what the
// shipped list already said — and reporting it as authoritative would strip
// capabilities off every model the CLI happens not to list.
func (a *App) modelInfoForProvider(providerName, model string) (info provider.ModelInfo, found, catalogAuthoritative bool) {
	providerName = strings.TrimSpace(providerName)
	model = provider.NormalizeModelSlug(providerName, strings.TrimSpace(model))
	switch provider.CapabilitiesForProvider(providerName).ModelCatalog {
	case provider.CodexLiveModelCatalog:
		models, err := a.GetModelsForProvider(providerName)
		if err == nil {
			for _, candidate := range models {
				if candidate.Slug == model {
					return candidate, true, true
				}
			}
			return provider.ModelInfo{}, false, true
		}
	case provider.ClaudeProbeEnrichedCatalog:
		for _, candidate := range a.claudeModelsForProvider(providerName) {
			if candidate.Slug == model {
				return candidate, true, false
			}
		}
		return provider.ModelInfo{}, false, false
	}

	info, found = provider.FindModel(providerName, model)
	return info, found, false
}
