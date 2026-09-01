package app

import (
	"context"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/providerdiscoveryapp"
)

func codexAccountProbeCache() *provider.ProbeCache {
	return providerdiscoveryapp.DefaultCaches().Codex
}

// resetCodexProbeCacheForTest swaps the package-level cache for a fresh
// instance. Mirrors resetClaudeProbeCacheForTest.
func resetCodexProbeCacheForTest() {
	providerdiscoveryapp.ResetDefaultCachesForTest()
}

// ProbeCodexAccount spawns a short-lived `codex app-server` subprocess,
// runs the JSON-RPC initialize handshake, calls
// `account/rateLimits/read`, and returns AccountInfo whose
// SubscriptionType carries the wire's planType (e.g. "pro", "plus").
// Results are cached per binary path for 5 minutes.
//
// Zero tokens are consumed: no thread is opened, no inference is
// performed. Failure modes (binary missing, auth required) propagate as
// typed errors so the caller can decide whether to surface a banner.
//
// On any cache-miss success we also emit a `provider:account` event so
// the rate-limit ring popover's plan label hydrates from the same code
// path that the startup hook uses. Cache hits do NOT re-emit — the
// frontend store already has the value from the original miss.
//
// On the same cache-miss success we ALSO emit a `provider:usage` event
// carrying the rate-limit snapshot the probe pulled from the same
// `account/rateLimits/read` response. Without this, the 5h/7d rings
// stay empty until the user runs a turn — and stale if the user
// exhausted the limit in another Codex surface (TUI, CLI) before the
// app started. Cache hits skip the emit by design: the value is at
// most ProbeCache.TTL old AND the store already has it from the
// original miss; re-emitting could overwrite a fresher snapshot
// pushed by an active session via account/rateLimits/updated.
//
// Unlike Claude, Codex deliberately omits the unauthenticated-banner
// hook: an empty planType is ambiguous (backend latency can produce it
// for an authenticated user) so surfacing a banner here would create
// false positives. (Drift D3 from the app-root refactor.)
//
//ao:scope access:admin
//ao:route home
func (a *App) ProbeCodexAccount() (provider.AccountInfo, error) {
	return a.providerDiscoveryService().ProbeCodexAccount()
}

// RecheckCodexAccount evicts the cached result for the configured
// Codex binary and re-runs ProbeCodexAccount. Mirrors
// `RecheckClaudeAccount` — the symmetry exists so any user-initiated
// auth-state refresh has explicit intent on both providers; today
// only Claude has a Recheck UI but the surface is here when Codex
// needs one (e.g. after a `codex login` flow lands).
//
//ao:scope access:admin
//ao:route home
func (a *App) RecheckCodexAccount() (provider.AccountInfo, error) {
	return a.providerDiscoveryService().RecheckCodexAccount()
}

func (a *App) probeCodexRateLimits(ctx context.Context) error {
	return a.providerLifecycleService().ProbeCodexRateLimits(ctx)
}

func (a *App) startCodexRateLimitProbeLoop() {
	a.providerLifecycleService().StartCodexPoll()
}
