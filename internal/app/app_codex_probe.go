package app

import (
	"agent-overflow/internal/provider"
)

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

func (a *App) startCodexRateLimitProbeLoop() {
	a.providerLifecycleService().StartCodexPoll()
}
