package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
)

// providerProbeRunner bundles the per-provider hooks runAccountProbe
// composes around the shared cache-hit / cache-miss / emit dance.
//
// providerName seeds the `provider:account` event payload; cache + key
// address the memo; probe is the wire call that runs on a miss;
// unauthenticated + emitUnauth are an optional pair — Claude wires both
// to surface a "run claude login" banner, Codex leaves them nil because
// its empty planType is an ambiguous signal that would produce false
// positives.
type providerProbeRunner struct {
	providerName    string
	cache           *provider.ProbeCache
	key             provider.ProbeCacheKey
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
	if cached, hit := r.cache.Get(r.key); hit {
		if r.unauthenticated != nil && r.emitUnauth != nil && r.unauthenticated(cached) {
			r.emitUnauth()
		}
		return cached, nil
	}

	reconcileMu := a.providerCredentialReconcileMutex(r.providerName)
	reconcileMu.Lock()
	info, credential, err := a.runStableAccountProbe(r.providerName, r.probe)
	if err != nil {
		reconcileMu.Unlock()
		return provider.AccountInfo{}, err
	}

	if r.unauthenticated != nil && r.emitUnauth != nil && r.unauthenticated(info) {
		r.emitUnauth()
	}
	account, _, err := a.adoptCurrentProviderAccount(r.providerName, info, credential)
	if err != nil {
		reconcileMu.Unlock()
		return provider.AccountInfo{}, err
	}
	reconcileMu.Unlock()
	r.cache.Set(r.key, info)
	a.emitProviderAccountIfCurrent(r.providerName, account, info)
	if r.afterAdopt != nil {
		r.afterAdopt(account)
	}
	return info, nil
}

// runStableAccountProbe pairs provider identity with one stable canonical
// credential value. A provider may legitimately rotate credentials while its
// probe initializes, so one changed read retries the probe. A second change is
// treated as a concurrent native login and left for the next reconciliation.
func (a *App) runStableAccountProbe(
	providerName string,
	probe func(context.Context) (provider.AccountInfo, error),
) (provider.AccountInfo, *provideraccounts.CredentialSnapshot, error) {
	if a.providerCredentials == nil {
		info, err := probe(a.lifeCtx())
		return info, nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		before, beforePresent, err := a.readCanonicalCredentialIfPresent(providerName)
		if err != nil {
			return provider.AccountInfo{}, nil, err
		}
		info, err := probe(a.lifeCtx())
		if err != nil {
			return provider.AccountInfo{}, nil, err
		}
		after, afterPresent, err := a.readCanonicalCredentialIfPresent(providerName)
		if err != nil {
			return provider.AccountInfo{}, nil, err
		}
		if beforePresent == afterPresent &&
			(!beforePresent || sha256.Sum256(before.Data) == sha256.Sum256(after.Data)) {
			if !afterPresent {
				return info, nil, nil
			}
			return info, &after, nil
		}
	}
	return provider.AccountInfo{}, nil, fmt.Errorf(
		"%s credentials changed while identifying the active account; retry",
		providerName,
	)
}

func (a *App) readCanonicalCredentialIfPresent(
	providerName string,
) (provideraccounts.CredentialSnapshot, bool, error) {
	snapshot, err := a.providerCredentials.ReadCredentialSnapshot(
		providerName,
		"",
		true,
	)
	if provideraccounts.IsCredentialMissing(err) {
		return provideraccounts.CredentialSnapshot{}, false, nil
	}
	if err != nil {
		return provideraccounts.CredentialSnapshot{}, false, fmt.Errorf(
			"read active %s credentials: %w",
			providerName,
			err,
		)
	}
	return snapshot, true, nil
}

func (a *App) emitProviderAccount(
	providerName string,
	account provideraccounts.Account,
	info provider.AccountInfo,
	generation uint64,
) {
	a.emit("provider:account", ProviderAccountEvent{
		Provider:   providerName,
		AccountID:  account.ID,
		Account:    info,
		Generation: generation,
	})
}

func (a *App) emitProviderAccountIfCurrent(
	providerName string,
	account provideraccounts.Account,
	info provider.AccountInfo,
) {
	if a.providerAccounts == nil {
		// Pre-startup and focused test apps can probe provider identity before
		// the managed-account store is attached. There is no competing
		// account generation in that state, so preserve the probe contract
		// and emit the unscoped identity directly.
		a.emitProviderAccount(providerName, account, info, 0)
		return
	}
	a.providerAccountMu.RLock()
	active, ok := a.providerAccounts.Active(providerName, time.Now())
	generation := a.providerAccounts.Generation(providerName)
	a.providerAccountMu.RUnlock()
	if !ok || active.ID != account.ID {
		return
	}
	a.emitProviderAccount(providerName, account, info, generation)
}

func (a *App) emitProviderAccountCleared(providerName string, generation uint64) {
	a.emit("provider:account", ProviderAccountEvent{
		Provider:   providerName,
		Generation: generation,
		Cleared:    true,
	})
}
