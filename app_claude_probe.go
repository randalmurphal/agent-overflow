package main

import (
	"context"
	"sync"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/providerstatus"
)

// claudeProbeCache is a package-level cache shared across App instances
// within the process. Probe results depend only on the binary path and
// the ambient Claude auth state, not on App configuration, so a single
// cache per process is correct.
//
// The cache is guarded by claudeProbeCacheMu rather than sync.Once so
// tests can reassign claudeProbeCache to a fresh instance between
// subtests without racing against production initialization. The
// previous sync.Once pattern made test-side resets fragile: if a test
// ran before the Once had fired, its reset was silently overwritten by
// the Once body on first production access; if the Once had fired the
// reset worked, so identical tests passed or failed depending on suite
// ordering.
var (
	claudeProbeCacheMu sync.Mutex
	claudeProbeCache   *claude.ProbeCache
)

func claudeAccountProbeCache() *claude.ProbeCache {
	claudeProbeCacheMu.Lock()
	defer claudeProbeCacheMu.Unlock()
	if claudeProbeCache == nil {
		claudeProbeCache = claude.NewProbeCache(claude.DefaultProbeTTL)
	}
	return claudeProbeCache
}

// resetClaudeProbeCacheForTest swaps the package-level cache for a
// fresh instance. Call from test setup to guarantee a clean cache
// without racing against concurrent probes via claudeAccountProbeCache.
func resetClaudeProbeCacheForTest() {
	claudeProbeCacheMu.Lock()
	defer claudeProbeCacheMu.Unlock()
	claudeProbeCache = claude.NewProbeCache(claude.DefaultProbeTTL)
}

// ProbeClaudeAccount spawns a short-lived Claude CLI subprocess (via
// the SDK initialize handshake) and returns the authenticated account
// metadata. Results are cached per binary path for 5 minutes. Zero
// tokens are consumed — the CLI never runs inference (`--max-turns 0`).
//
// On a successful probe that returns an empty AccountInfo (no
// subscription and no token source) we emit a `provider:status` event
// with Status="unauthenticated" so the thread-level banner can prompt
// the user to run `claude login`. On any cache-miss success we also
// emit a `provider:account` event so the rate-limit ring popover's
// plan label hydrates from the same code path that the startup hook
// uses — no separate emit step in callers.
//
// Cache hits do NOT re-emit `provider:account`: the frontend store
// already has the value from the original miss.
func (a *App) ProbeClaudeAccount() (provider.AccountInfo, error) {
	binary := a.providerBinaryPath(string(provider.Claude))
	selection := a.captureProviderAccountSelection(string(provider.Claude))
	return a.runAccountProbe(providerProbeRunner{
		providerName: string(provider.Claude),
		cache:        claudeAccountProbeCache(),
		binary:       providerProbeCacheKeyForAccount(binary, selection.AccountID),
		probe: func(ctx context.Context) (provider.AccountInfo, error) {
			return claude.ProbeAccount(ctx, claude.ProbeConfig{
				Binary: binary,
			})
		},
		unauthenticated: providerstatus.ClaudeUnauthenticated,
		emitUnauth:      a.emitClaudeUnauthenticatedStatus,
	})
}

// RecheckClaudeAccount evicts the cached result for the configured
// Claude binary and re-runs ProbeClaudeAccount. This is the surface
// the auth banner's "Recheck Auth" button calls after the user runs
// `claude login` (or logs out and wants to clear the cached plan
// info). Without invalidation, the cache would mask the new state
// for up to 5 minutes — see the comment on `Invalidate`.
//
// Splitting Recheck from Probe keeps user intent visible at the call
// site: any caller that wants live state asks for it explicitly,
// rather than passing a `bypassCache` flag that's easy to forget.
func (a *App) RecheckClaudeAccount() (provider.AccountInfo, error) {
	binary := a.providerBinaryPath(string(provider.Claude))
	claudeAccountProbeCache().Invalidate(a.providerProbeCacheKey(string(provider.Claude), binary))
	return a.ProbeClaudeAccount()
}
