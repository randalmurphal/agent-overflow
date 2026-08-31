package app

import (
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
//
//ao:scope settings:write
//ao:stepup
func (a *App) SetProviderCustomEnvVar(
	providerName, name, value string,
	sensitive bool,
) (settings.Settings, error) {
	next, err := a.providerDiscoveryService().SetProviderCustomEnvVar(providerName, name, value, sensitive)
	if err != nil {
		return settings.Settings{}, err
	}
	return redactedSettings(next), nil
}

// DeleteProviderCustomEnvVar removes one custom environment variable. A name
// that isn't configured is an error, so a UI acting on a stale list learns
// about it instead of appearing to succeed.
//
//ao:scope settings:write
//ao:stepup
func (a *App) DeleteProviderCustomEnvVar(providerName, name string) (settings.Settings, error) {
	next, err := a.providerDiscoveryService().DeleteProviderCustomEnvVar(providerName, name)
	if err != nil {
		return settings.Settings{}, err
	}
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
