package main

import (
	"context"
	"sync"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
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
// Unlike Claude, Codex deliberately omits the unauthenticated-banner
// hook: an empty planType is ambiguous (backend latency can produce it
// for an authenticated user) so surfacing a banner here would create
// false positives. See drift D3 in
// `docs/architecture/refactor-phase-1/duplication.md`.
func (a *App) ProbeCodexAccount() (provider.AccountInfo, error) {
	binary := a.providerBinaryPath(string(provider.Codex))
	return a.runAccountProbe(providerProbeRunner{
		providerName: string(provider.Codex),
		cache:        codexAccountProbeCache(),
		binary:       binary,
		probe: func(ctx context.Context) (provider.AccountInfo, error) {
			return codex.ProbeAccount(ctx, codex.ProbeConfig{Binary: binary})
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
	codexAccountProbeCache().Invalidate(binary)
	return a.ProbeCodexAccount()
}
