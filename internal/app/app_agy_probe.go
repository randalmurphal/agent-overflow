package app

import (
	"agent-overflow/internal/provider"
)

// ProbeAgyAccount runs the zero-token Antigravity identity/tier probe and
// caches the answer per binary/account/environment.
//
//ao:scope access:admin
//ao:route home
func (a *App) ProbeAgyAccount() (provider.AccountInfo, error) {
	return a.providerDiscoveryService().ProbeAgyAccount()
}

// RecheckAgyAccount evicts the cached Antigravity identity for the configured
// binary and re-runs ProbeAgyAccount.
//
//ao:scope access:admin
//ao:route home
func (a *App) RecheckAgyAccount() (provider.AccountInfo, error) {
	return a.providerDiscoveryService().RecheckAgyAccount()
}

func (a *App) startAgyRateLimitProbeLoop() {
	a.providerLifecycleService().StartAgyPoll()
}
