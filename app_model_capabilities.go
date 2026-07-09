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
	if len(candidate.ContextWindows) > 0 && !chatmodel.ContextWindowSupported(candidate.ContextWindows, profile.ContextWindow) {
		profile.ContextWindow = candidate.ContextWindows[0].Tokens
	}
	return profile
}

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

// modelInfoForProvider prefers Codex's live catalog. The final return value
// distinguishes a successful catalog miss from a catalog error: a miss is an
// authoritative rejection, while an error permits the bundled registry to
// keep the app usable when Codex cannot be reached.
func (a *App) modelInfoForProvider(providerName, model string) (info provider.ModelInfo, found, catalogAuthoritative bool) {
	providerName = strings.TrimSpace(providerName)
	model = provider.NormalizeModelSlug(providerName, strings.TrimSpace(model))
	if providerName == string(provider.Codex) {
		models, err := a.GetModelsForProvider(providerName)
		if err == nil {
			for _, candidate := range models {
				if candidate.Slug == model {
					return candidate, true, true
				}
			}
			return provider.ModelInfo{}, false, true
		}
	}

	info, found = provider.FindModel(providerName, model)
	return info, found, false
}
