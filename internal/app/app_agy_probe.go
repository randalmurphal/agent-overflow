package app

import (
	"agent-overflow/internal/provider"
)

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
