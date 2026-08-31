package app

import (
	"context"
	"net/http"

	"agent-overflow/internal/claudecatalog"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccountapp"
	"agent-overflow/internal/providerdiscoveryapp"
)

var (
	errClaudeUsageStale         = provideraccountapp.ErrClaudeUsageStale
	errClaudeCredentialsExpired = provideraccountapp.ErrClaudeCredentialsExpired
)

var rateLimitProbeHTTPClient = &http.Client{}

func claudeAccountProbeCache() *provider.ProbeCache {
	return providerdiscoveryapp.DefaultCaches().Claude
}

// resetClaudeProbeCacheForTest swaps the package-level cache for a
// fresh instance. Call from test setup to guarantee a clean cache
// without racing against concurrent probes via claudeAccountProbeCache.
//
// It resets the probe-enriched model catalog and command list with it, because
// one probe fills all three: a test that cleared only the identity cache would
// re-probe and then compare against another test's model list. One reset, no
// drift.
func resetClaudeProbeCacheForTest() {
	providerdiscoveryapp.ResetDefaultCachesForTest()
	claudecatalog.Reset()
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
	return a.providerDiscoveryService().ProbeClaudeAccount()
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
	return a.providerDiscoveryService().RecheckClaudeAccount()
}

func (a *App) assertSelectedClaudeIdentity(selection providerAccountSelection, info provider.AccountInfo) error {
	return a.providerAccounts.AssertSelectedClaudeIdentity(selection, info)
}

func (a *App) probeClaudeRateLimits(ctx context.Context) error {
	return a.providerLifecycleService().ProbeClaudeRateLimits(ctx)
}

func (a *App) rateLimitProbeClient() *http.Client {
	if a.rateLimitProbeClientOverride != nil {
		return a.rateLimitProbeClientOverride
	}
	return rateLimitProbeHTTPClient
}

func (a *App) startClaudeRateLimitProbeLoop() {
	a.providerLifecycleService().StartClaudePoll()
}
