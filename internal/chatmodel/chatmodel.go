// Package chatmodel owns the pure (`provider`+`store`-only) logic for
// chat-thread model profiles: defaults, normalization, validation,
// capability detection, and sameness checks.
//
// The App's persistence-coupled helpers (`rememberChatModelProfile`,
// `seedChatModelProfile`, etc.) compose this package's pure pieces with
// their store reads/writes. Everything here is safe to call without a
// running App.
package chatmodel

import (
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// FallbackProvider names the provider used when no other signal narrows
// the choice. Claude is the desktop app's first-class provider; Codex
// is reachable but optional.
func FallbackProvider() string {
	return string(provider.Claude)
}

// FallbackModelForProvider returns the first model the provider
// registry advertises. Empty string means the registry has no models
// for that provider (a misconfiguration the caller surfaces).
func FallbackModelForProvider(providerName string) string {
	models := provider.ModelsForProvider(providerName)
	if len(models) == 0 {
		return ""
	}
	return models[0].Slug
}

// FallbackProfile is the zero-history baseline: enough fields populated
// for the UI to render a chat bar even when SQLite has no
// previously-remembered profile for this (provider, model) pair.
func FallbackProfile(providerName, model string) store.ChatModelProfile {
	providerName = strings.TrimSpace(providerName)
	if providerName == "" {
		providerName = FallbackProvider()
	}
	model = strings.TrimSpace(model)
	if model == "" {
		model = FallbackModelForProvider(providerName)
	}
	return store.ChatModelProfile{
		Provider:                   providerName,
		Model:                      model,
		ReasoningEffort:            string(provider.DefaultReasoningEffortForModel(providerName, model, provider.DefaultReasoningEffort)),
		FastMode:                   false,
		ContextWindow:              DefaultContextWindow(providerName, model, 0),
		AutoCompactStandardPercent: 0,
		AutoCompactExtendedPercent: 0,
		RuntimeMode:                string(provider.DefaultRuntimeMode),
	}
}

// ProfileFromThread projects a stored thread's chat-model settings into
// the standalone ChatModelProfile shape used by the "remember last
// model" cache. Fast-mode is dropped when the model doesn't support
// it so a stale flag can't survive a model swap.
func ProfileFromThread(thread store.Thread) store.ChatModelProfile {
	effort := provider.CoerceReasoningEffortForModel(
		thread.Provider,
		thread.Model,
		provider.NormalizeReasoningEffort(thread.ReasoningEffort),
	)
	fastMode := thread.FastMode && SupportsStoredFastMode(thread.Provider, thread.Model)
	return store.ChatModelProfile{
		Provider:                   thread.Provider,
		Model:                      thread.Model,
		ReasoningEffort:            string(effort),
		FastMode:                   fastMode,
		ContextWindow:              thread.ContextWindow,
		AutoCompactStandardPercent: thread.AutoCompactStandardPercent,
		AutoCompactExtendedPercent: thread.AutoCompactExtendedPercent,
		RuntimeMode:                string(provider.NormalizeRuntimeMode(thread.RuntimeMode)),
	}
}

// SameProfile compares two profiles for equality under the same
// normalization the rest of the package uses (runtime-mode is
// normalized before compare; everything else is a plain ==).
func SameProfile(a, b store.ChatModelProfile) bool {
	return a.Provider == b.Provider &&
		a.Model == b.Model &&
		a.ReasoningEffort == b.ReasoningEffort &&
		a.FastMode == b.FastMode &&
		a.ContextWindow == b.ContextWindow &&
		a.AutoCompactStandardPercent == b.AutoCompactStandardPercent &&
		a.AutoCompactExtendedPercent == b.AutoCompactExtendedPercent &&
		provider.NormalizeRuntimeMode(a.RuntimeMode) == provider.NormalizeRuntimeMode(b.RuntimeMode)
}

// SanitizeProfile clamps every field to a registry-supported value.
// Use it after loading a stored profile to defend the UI from rows
// written under an older provider catalog.
func SanitizeProfile(profile store.ChatModelProfile) store.ChatModelProfile {
	profile.Model = provider.NormalizeModelSlug(profile.Provider, profile.Model)
	profile.ReasoningEffort = string(provider.CoerceReasoningEffortForModel(
		profile.Provider,
		profile.Model,
		provider.NormalizeReasoningEffort(profile.ReasoningEffort),
	))
	profile.ContextWindow = SanitizeContextWindow(profile.Provider, profile.Model, profile.ContextWindow)
	if !SupportsStoredFastMode(profile.Provider, profile.Model) {
		profile.FastMode = false
	}
	return profile
}

// SanitizeContextWindow returns the closest registry-supported window
// for the (provider, model) pair. When the registry has no opinion,
// the caller's tokens pass through if positive; otherwise the
// provider-level default is used.
func SanitizeContextWindow(providerName, model string, tokens int) int {
	options := ContextWindowOptions(providerName, model)
	if len(options) == 0 {
		if tokens > 0 {
			return tokens
		}
		return provider.DefaultContextWindowForModel(providerName, model, 0)
	}
	if ContextWindowSupported(options, tokens) {
		return tokens
	}
	return provider.DefaultContextWindowForModel(providerName, model, options[0].Tokens)
}

// ContextWindowOptions returns the registry-advertised window choices
// for the (provider, model) pair. The slice may be empty when the
// provider doesn't gate context windows through the registry.
func ContextWindowOptions(providerName, model string) []provider.ContextWindowOption {
	return provider.ContextWindowOptionsForModel(providerName, model)
}

// ContextWindowSupported reports whether `tokens` matches any of the
// registry-advertised options.
func ContextWindowSupported(options []provider.ContextWindowOption, tokens int) bool {
	for _, option := range options {
		if option.Tokens == tokens {
			return true
		}
	}
	return false
}

// DefaultContextWindow returns the provider-level default window for
// the (provider, model) pair, falling back to `fallback` when the
// registry has nothing to offer.
func DefaultContextWindow(providerName, model string, fallback int) int {
	return provider.DefaultContextWindowForModel(providerName, strings.TrimSpace(model), fallback)
}

// IsValidContextWindow reports whether `tokens` is a usable size. A
// non-positive value means "no opinion, fall back to the default."
func IsValidContextWindow(tokens int) bool {
	return tokens > 0
}

// SupportsStoredFastMode reports whether a stored fast-mode flag
// should be honored for the (provider, model) pair. Codex models that
// aren't in the registry get a permissive "yes" because the live model
// catalog is the source of truth for Codex; for everything else the
// registry decision is authoritative.
func SupportsStoredFastMode(providerName, model string) bool {
	model = provider.NormalizeModelSlug(providerName, model)
	candidate, found := provider.FindModel(providerName, model)
	if !found {
		return providerName == string(provider.Codex) && strings.TrimSpace(model) != ""
	}
	return HasCapability(candidate, provider.ModelCapabilityFastMode)
}

// HasCapability reports whether a model advertises a named capability.
// Equality on the capability string — the registry treats capabilities
// as opaque tags.
func HasCapability(model provider.ModelInfo, capability string) bool {
	for _, existing := range model.Capabilities {
		if existing == capability {
			return true
		}
	}
	return false
}
