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
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// FallbackProvider names the provider used when no other signal narrows
// the choice. Claude is the desktop app's first-class provider; Codex
// is reachable but optional.
//
// availableProviders lists which provider binaries the caller has
// confirmed resolve on PATH (via `exec.LookPath` or equivalent). Omit
// the arg when no probe data is available — the function then returns
// Claude unconditionally (the historical canonical default, which fails
// loudly when invoked and is the right surface for the error path).
//
// Resolution order when probe data IS available:
//   - Claude present → Claude (preserves prior behavior when both
//     providers are installed).
//   - else Codex present → Codex (only fallback for Claude-missing
//     environments).
//   - else → Claude (caller's error path stays consistent).
func FallbackProvider(availableProviders ...string) string {
	if len(availableProviders) == 0 {
		return string(provider.Claude)
	}
	hasCodex := false
	for _, p := range availableProviders {
		switch p {
		case string(provider.Claude):
			return string(provider.Claude)
		case string(provider.Codex):
			hasCodex = true
		}
	}
	if hasCodex {
		return string(provider.Codex)
	}
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
//
// availableProviders is forwarded to FallbackProvider for the blank-
// provider case; see that function's doc for the semantics. Omit the
// arg when the caller passes an explicit providerName (the slice is
// only consulted when providerName is blank).
func FallbackProfile(providerName, model string, availableProviders ...string) store.ChatModelProfile {
	providerName = strings.TrimSpace(providerName)
	if providerName == "" {
		providerName = FallbackProvider(availableProviders...)
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

// SanitizeThread clamps the thread's model-related fields the same
// way SanitizeProfile clamps a stored ChatModelProfile. Use it
// before persisting a thread row written by older code, or before
// comparing two threads with SameModelFields: if either side carries
// a non-canonical value the comparison is meaningless.
func SanitizeThread(thread store.Thread) store.Thread {
	thread.Model = provider.NormalizeModelSlug(thread.Provider, thread.Model)
	thread.ReasoningEffort = string(provider.CoerceReasoningEffortForModel(
		thread.Provider,
		thread.Model,
		provider.NormalizeReasoningEffort(thread.ReasoningEffort),
	))
	thread.ContextWindow = SanitizeContextWindow(thread.Provider, thread.Model, thread.ContextWindow)
	if !SupportsStoredFastMode(thread.Provider, thread.Model) {
		thread.FastMode = false
	}
	return thread
}

// SameModelFields reports whether two Threads have identical
// model-related fields (Model, ReasoningEffort, FastMode,
// ContextWindow). Used by the persistence side to decide whether a
// thread's stored row already matches the sanitized form — skipping
// a redundant UPDATE saves a SQLite write per session start.
//
// Inputs should be sanitized first; otherwise non-canonical drift
// (e.g. unnormalised model slugs) flags the rows as different when
// they aren't.
func SameModelFields(a, b store.Thread) bool {
	return a.Model == b.Model &&
		a.ReasoningEffort == b.ReasoningEffort &&
		a.FastMode == b.FastMode &&
		a.ContextWindow == b.ContextWindow
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
	return provider.DefaultContextWindowForModel(providerName, model, 0)
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

// DefaultContextWindowFor prefers the flagged default among CALLER-RESOLVED
// options (the App's merged catalogs, which carry family-inherited windows
// for wire-only models the static registry lacks), falling back to the
// static registry default only when the options carry nothing.
func DefaultContextWindowFor(options []provider.ContextWindowOption, providerName, model string) int {
	if tokens, ok := provider.DefaultContextWindowForOptions(options); ok {
		return tokens
	}
	return provider.DefaultContextWindowForModel(providerName, model, 0)
}

// FallbackProfileWith is FallbackProfile with the context window re-stamped
// from resolve, the caller's catalog-aware options lookup. FallbackProfile's
// static default for a wire-only model is the provider-wide standard window,
// which IS one of the merged catalog's options and so survives every
// supported-window check, silently displacing the family's flagged default
// (claude-fable-5-1 starting at 200k instead of 1M). A fallback profile
// carries no user choice, so the catalog default always wins here; for a
// statically-known model the re-stamp is the same value.
func FallbackProfileWith(
	resolve func(providerName, model string) []provider.ContextWindowOption,
	providerName, model string,
	availableProviders ...string,
) store.ChatModelProfile {
	profile := FallbackProfile(providerName, model, availableProviders...)
	profile.ContextWindow = DefaultContextWindowFor(resolve(profile.Provider, profile.Model), profile.Provider, profile.Model)
	return profile
}

// IsValidContextWindow reports whether `tokens` is a usable size. A
// non-positive value means "no opinion, fall back to the default."
func IsValidContextWindow(tokens int) bool {
	return tokens > 0
}

// ValidateContextUpdate runs the per-field bounds checks shared by
// the global `UpdateContextSettingsProfile` and the per-thread
// `UpdateThreadContextSettings` paths: trims and requires
// provider/model, asserts that the caller-resolved options are
// non-empty (an empty slice means the pair is unknown to every
// catalog the caller consulted), asserts that contextWindow is one
// of those options, and asserts that the two auto-compact percent
// thresholds sit in the inclusive 0..90 range that
// `IsValidAutoCompactPercent` accepts.
//
// The options are a PARAMETER, not an internal lookup, because the
// static registry alone is the wrong authority: wire-only models
// exist only in the App's merged catalogs, and this package stays
// process-free. Callers resolve via `App.contextWindowOptionsForModel`.
//
// Returns the trimmed provider + model strings on success so callers
// don't re-trim. The int inputs are bounds-checked but never
// rewritten — callers continue to use the raw int values they
// passed in. Errors carry a "context settings:" prefix so the
// caller's wrap matches the existing user-facing string.
func ValidateContextUpdate(options []provider.ContextWindowOption, rawProvider, rawModel string, contextWindow, autoCompactStandardPercent, autoCompactExtendedPercent int) (providerName, model string, err error) {
	providerName = strings.TrimSpace(rawProvider)
	model = strings.TrimSpace(rawModel)
	if providerName == "" || model == "" {
		return "", "", fmt.Errorf("context settings: provider and model are required")
	}
	if len(options) == 0 {
		return "", "", fmt.Errorf("context settings: unknown provider/model %s/%s", providerName, model)
	}
	if !ContextWindowSupported(options, contextWindow) {
		return "", "", fmt.Errorf("context settings: unsupported context window %d for %s/%s", contextWindow, providerName, model)
	}
	if autoCompactStandardPercent < 0 || autoCompactStandardPercent > 90 {
		return "", "", fmt.Errorf("context settings: standard auto-compact percent must be between 0 and 90")
	}
	if autoCompactExtendedPercent < 0 || autoCompactExtendedPercent > 90 {
		return "", "", fmt.Errorf("context settings: extended auto-compact percent must be between 0 and 90")
	}
	return providerName, model, nil
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
