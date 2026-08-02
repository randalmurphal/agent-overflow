package main

import (
	"fmt"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claudetui"
	"agent-overflow/internal/settings"
)

// --- User-defined provider environment ---
//
// Custom environment variables applied to the provider processes Agent
// Overflow spawns: chat sessions (including claude-tui), the account /
// identity / rate-limit probes, and the text-generation CLI. The shape,
// the reserved-name rules, and why sensitive values are redacted on read
// live in internal/settings/providerenv.go.
//
// These are dedicated mutators rather than UpdateSettings keys for the same
// reason the remote-endpoint CRUD exists: GetSettings redacts sensitive
// values, so a read-mutate-write round trip through the generic patch path
// would persist the redaction. settings.Service.Update rejects the keys, so
// this is the only way in.

// SetProviderCustomEnvVar adds or replaces one custom environment variable for
// a provider and returns the redacted settings snapshot the frontend store
// re-seeds from. Reserved names, malformed names, oversize values, and
// duplicates are rejected here — never dropped silently at spawn time.
func (a *App) SetProviderCustomEnvVar(
	providerName, name, value string,
	sensitive bool,
) (settings.Settings, error) {
	if a.settings == nil {
		return settings.Settings{}, fmt.Errorf("settings service unavailable")
	}
	previous := a.providerCustomEnv(providerName)
	next, err := a.settings.SetProviderEnvVar(providerName, name, value, sensitive)
	if err != nil {
		return settings.Settings{}, err
	}
	a.afterProviderCustomEnvChange(providerName, previous)
	return redactedSettings(next), nil
}

// DeleteProviderCustomEnvVar removes one custom environment variable. A name
// that isn't configured is an error, so a UI acting on a stale list learns
// about it instead of appearing to succeed.
func (a *App) DeleteProviderCustomEnvVar(providerName, name string) (settings.Settings, error) {
	if a.settings == nil {
		return settings.Settings{}, fmt.Errorf("settings service unavailable")
	}
	previous := a.providerCustomEnv(providerName)
	next, err := a.settings.DeleteProviderEnvVar(providerName, name)
	if err != nil {
		return settings.Settings{}, err
	}
	a.afterProviderCustomEnvChange(providerName, previous)
	return redactedSettings(next), nil
}

// claudetuiUpstream is where a take-control session's loopback gateway
// forwards. Empty means the real Anthropic API.
//
// The interactive CLI's own ANTHROPIC_BASE_URL is owned by claudetui (it must
// resolve to the per-session gateway), so a user-configured endpoint has to
// arrive here instead — otherwise the one Claude surface that ignores the
// setting would be the interactive one, silently talking to a different
// backend than every other session and probe.
func (a *App) claudetuiUpstream() string {
	return a.providerCustomEnv(string(provider.ClaudeTUI))[claudetui.BaseURLEnv]
}

// afterProviderCustomEnvChange drops the account-probe answers that were
// resolved under the previous environment.
//
// The custom environment is already a ProbeCacheKey dimension, so the NEW
// environment simply misses the cache — identity re-resolves on the next probe
// without any help. This exists for the other half: the entry cached under the
// OLD environment is now unreachable and would sit for the rest of its TTL,
// and — the case that matters — flipping a variable back would serve that
// stale pre-change answer instead of re-asking. Probes cost zero tokens; a
// wrong account label does not.
func (a *App) afterProviderCustomEnvChange(providerName string, previous map[string]string) {
	// The probe caches only ever hold entries built by the CANONICAL
	// provider's probe paths — claude-tui shares Claude's binary, custom
	// environment, and account store, and never probes under its own name.
	// A claude-tui-named mutation (the bound method accepts it, even though
	// the settings UI happens to send "claude") must therefore evict under
	// Claude's identity: Store.Active is a raw map lookup, so asking it for
	// "claude-tui" would resolve an empty account id and both evictions
	// would miss the entries the probes actually use.
	canonical := providerName
	if providerName == string(provider.ClaudeTUI) {
		canonical = string(provider.Claude)
	}
	binary := a.providerBinaryPath(canonical)
	accountID := ""
	if a.providerAccounts != nil {
		if account, ok := a.providerAccounts.Active(canonical, time.Now()); ok {
			accountID = account.ID
		}
	}
	stale := providerProbeCacheKeyForAccountEnv(binary, accountID, previous)
	switch canonical {
	case string(provider.Claude):
		claudeAccountProbeCache().Invalidate(stale)
		claudeAccountProbeCache().Invalidate(
			a.providerProbeCacheKeyForAccount(canonical, binary, accountID),
		)
	case string(provider.Codex):
		codexAccountProbeCache().Invalidate(stale)
		codexAccountProbeCache().Invalidate(
			a.providerProbeCacheKeyForAccount(canonical, binary, accountID),
		)
	}
}
