package main

import (
	"context"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
)

// providerProbeRunner bundles the per-provider hooks runAccountProbe
// composes around the shared cache-hit / cache-miss / emit dance.
//
// providerName seeds the `provider:account` event payload; cache and
// binary key the cache; probe is the wire call that runs on a miss;
// unauthenticated + emitUnauth are an optional pair — Claude wires both
// to surface a "run claude login" banner, Codex leaves them nil because
// its empty planType is an ambiguous signal that would produce false
// positives.
type providerProbeRunner struct {
	providerName    string
	cache           *provider.ProbeCache
	binary          string
	probe           func(ctx context.Context) (provider.AccountInfo, error)
	unauthenticated func(provider.AccountInfo) bool
	emitUnauth      func()
	afterAdopt      func(provideraccounts.Account)
}

// runAccountProbe is the shared cache-aware probe orchestrator behind
// Probe{Claude,Codex}Account. Cache hits return immediately and do NOT
// re-emit `provider:account` (the frontend already has the value); cache
// misses run the provider's probe, stash the result, and emit. The
// optional unauthenticated/emitUnauth hook fires on both hit and miss
// for providers (Claude) where an empty AccountInfo is an unambiguous
// "not logged in" signal.
func (a *App) runAccountProbe(r providerProbeRunner) (provider.AccountInfo, error) {
	if cached, hit := r.cache.Get(r.binary); hit {
		if r.unauthenticated != nil && r.emitUnauth != nil && r.unauthenticated(cached) {
			r.emitUnauth()
		}
		return cached, nil
	}

	info, err := r.probe(a.lifeCtx())
	if err != nil {
		return provider.AccountInfo{}, err
	}

	r.cache.Set(r.binary, info)
	if r.unauthenticated != nil && r.emitUnauth != nil && r.unauthenticated(info) {
		r.emitUnauth()
	}
	account, _ := a.adoptCurrentProviderAccount(r.providerName, info)
	a.emitProviderAccount(r.providerName, account, info)
	if r.afterAdopt != nil {
		r.afterAdopt(account)
	}
	return info, nil
}

func (a *App) emitProviderAccount(providerName string, account provideraccounts.Account, info provider.AccountInfo) {
	a.emit("provider:account", ProviderAccountEvent{
		Provider:  providerName,
		AccountID: account.ID,
		Account:   info,
	})
}
