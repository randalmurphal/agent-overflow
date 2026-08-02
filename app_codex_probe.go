package main

import (
	"context"
	"sync"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"
)

// codexProbeCache is a package-level cache shared across App instances
// within the process. Probe results depend only on the binary path and
// the ambient Codex auth state, so a single per-process cache is
// correct — mirrors the Claude probe pattern.
//
// The cache is guarded by codexProbeCacheMu rather than sync.Once so
// tests can reassign codexProbeCache to a fresh instance between
// subtests without racing against production initialization.
var (
	codexProbeCacheMu sync.Mutex
	codexProbeCache   *codex.ProbeCache
)

func codexAccountProbeCache() *codex.ProbeCache {
	codexProbeCacheMu.Lock()
	defer codexProbeCacheMu.Unlock()
	if codexProbeCache == nil {
		codexProbeCache = codex.NewProbeCache(codex.DefaultProbeTTL)
	}
	return codexProbeCache
}

// resetCodexProbeCacheForTest swaps the package-level cache for a fresh
// instance. Mirrors resetClaudeProbeCacheForTest.
func resetCodexProbeCacheForTest() {
	codexProbeCacheMu.Lock()
	defer codexProbeCacheMu.Unlock()
	codexProbeCache = codex.NewProbeCache(codex.DefaultProbeTTL)
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
// false positives. See drift D3 in
// `docs/architecture/refactor-phase-1/duplication.md`.
func (a *App) ProbeCodexAccount() (provider.AccountInfo, error) {
	binary := a.providerBinaryPath(string(provider.Codex))
	selection := a.captureProviderAccountSelection(string(provider.Codex))
	var observedSnapshot *provider.RateLimitsSnapshot
	return a.runAccountProbe(providerProbeRunner{
		providerName: string(provider.Codex),
		cache:        codexAccountProbeCache(),
		key:          a.providerProbeCacheKeyForAccount(string(provider.Codex), binary, selection.AccountID),
		probe: func(ctx context.Context) (provider.AccountInfo, error) {
			cfg := a.codexProbeConfig(binary, nil)
			cfg.OnSnapshot = func(snapshot provider.RateLimitsSnapshot) {
				copy := cloneRateLimitsSnapshot(snapshot)
				observedSnapshot = &copy
			}
			return codex.ProbeAccount(ctx, cfg)
		},
		afterAdopt: func(account provideraccounts.Account) {
			if observedSnapshot == nil {
				return
			}
			observedSnapshot.AccountID = account.ID
			a.emitRateLimitsSnapshot(*observedSnapshot)
		},
	})
}

// RecheckCodexAccount evicts the cached result for the configured
// Codex binary and re-runs ProbeCodexAccount. Mirrors
// `RecheckClaudeAccount` — the symmetry exists so any user-initiated
// auth-state refresh has explicit intent on both providers; today
// only Claude has a Recheck UI but the surface is here when Codex
// needs one (e.g. after a `codex login` flow lands).
func (a *App) RecheckCodexAccount() (provider.AccountInfo, error) {
	binary := a.providerBinaryPath(string(provider.Codex))
	codexAccountProbeCache().Invalidate(a.providerProbeCacheKey(string(provider.Codex), binary))
	return a.ProbeCodexAccount()
}
