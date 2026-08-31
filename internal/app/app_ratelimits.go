package app

import (
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
)

func (a *App) rememberRateLimitsEvent(name eventchan.Channel, data any) {
	a.providerLifecycleService().RememberEvent(name, data)
}

// GetRateLimitsSnapshots returns the last known account-scoped quota for each
// provider. The data is already published on the remote-safe provider:usage
// channel, so this read-only RPC intentionally remains available to connected
// clients. It closes the first-connect/reconnect race where the startup probe
// completed before the frontend subscribed to that channel.
func (a *App) GetRateLimitsSnapshots() []provider.RateLimitsSnapshot {
	return a.providerLifecycleService().Snapshots()
}

func (a *App) hydratePersistedAccountRateLimits() {
	a.providerLifecycleService().Hydrate()
}
