package app

import (
	"context"
	"net/http"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccountapp"
)

var (
	errClaudeUsageStale         = provideraccountapp.ErrClaudeUsageStale
	errClaudeCredentialsExpired = provideraccountapp.ErrClaudeCredentialsExpired
)

var rateLimitProbeHTTPClient = &http.Client{}

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
//
//ao:scope access:admin
//ao:route home
func (a *App) RecheckClaudeAccount() (provider.AccountInfo, error) {
	return a.providerDiscoveryService().RecheckClaudeAccount()
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
