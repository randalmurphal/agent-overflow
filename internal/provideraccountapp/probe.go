package provideraccountapp

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
//
// unauthenticated only ever reads the identity the PROVIDER reported, so it
// can never be the whole answer on its own — see emitUnauthenticatedIfNoLogin,
// which is the only thing allowed to call emitUnauth.
type ProbeRequest struct {
	ProviderName    string
	Cache           *provider.ProbeCache
	Key             provider.ProbeCacheKey
	Probe           func(ctx context.Context) (provider.AccountInfo, error)
	Unauthenticated func(provider.AccountInfo) bool
	EmitUnauth      func()
	AfterAdopt      func(provideraccounts.Account)
}

// runAccountProbe is the shared cache-aware probe orchestrator behind
// Probe{Claude,Codex}Account. Cache hits return immediately and do NOT
// re-emit `provider:account` (the frontend already has the value); cache
// misses run the provider's probe, stash the result, and emit. The
// optional unauthenticated/emitUnauth hook fires on both hit and miss
// for providers (Claude) where an empty AccountInfo is an unambiguous
// "not logged in" signal.
func (m *Manager) RunAccountProbe(r ProbeRequest) (provider.AccountInfo, error) {
	if cached, hit := r.Cache.Get(r.Key); hit {
		m.emitUnauthenticatedIfNoLogin(r, cached)
		return cached, nil
	}

	reconcileMu := m.reconcileMutex(r.ProviderName)
	reconcileMu.Lock()
	info, credential, err := m.runStableAccountProbe(r.ProviderName, r.Probe)
	if err != nil {
		reconcileMu.Unlock()
		return provider.AccountInfo{}, err
	}

	m.emitUnauthenticatedIfNoLogin(r, info)
	account, _, err := m.adoptCurrentProviderAccount(r.ProviderName, info, credential)
	if err != nil {
		reconcileMu.Unlock()
		return provider.AccountInfo{}, err
	}
	reconcileMu.Unlock()
	r.Cache.Set(r.Key, info)
	m.emitProviderAccountIfCurrent(r.ProviderName, account, info)
	if r.AfterAdopt != nil {
		r.AfterAdopt(account)
	}
	return info, nil
}

// emitUnauthenticatedIfNoLogin raises the "run `claude login`" banner only
// when there is actually no login to speak of.
//
// The identity heuristic alone cannot decide this. Claude reports its identity
// out of `~/.claude.json`'s `oauthAccount` record, and Agent Overflow DELETES
// that record on every account switch (provideraccounts.retireProviderIdentity)
// so the provider re-derives it rather than describing one account's email over
// another's tokens. Spike-verified against 2.1.234: with the record absent, a
// perfectly healthy Claude Max login probes as
//
//	{"subscriptionType":"Claude Max","apiProvider":"firstParty"}
//
// — no email, no displayName, no tokenSource — which is byte-for-byte the shape
// a destroyed login echoes, and which ClaudeUnauthenticated therefore reads as
// logged out. Holding the probe's process open does not help: the CLI does not
// write the record back on a `--max-turns 0` start at all (12s hold, still
// absent), so only a real session restores it. That is why every switch was
// followed by a false "Claude is not authenticated" banner.
//
// The credential is the tiebreaker the identity cannot be. Bytes that exist and
// are not the provider's sign-out husk mean the user IS logged in, whatever the
// provider just said about itself — and telling them to run `claude login` in
// that state is advice that would replace a working login.
//
// A read failure is treated as "cannot prove there is no login", which keeps a
// transient Keychain error from raising a banner that instructs the user to
// sign in again.
func (m *Manager) emitUnauthenticatedIfNoLogin(r ProbeRequest, info provider.AccountInfo) {
	if r.Unauthenticated == nil || r.EmitUnauth == nil || !r.Unauthenticated(info) {
		return
	}
	if m.credentials != nil {
		snapshot, present, err := m.readCanonicalCredentialIfPresent(r.ProviderName)
		if err != nil {
			return
		}
		if present && !m.credentials.CredentialSignedOut(r.ProviderName, snapshot.Data) {
			return
		}
	}
	r.EmitUnauth()
}

// runStableAccountProbe pairs provider identity with one stable canonical
// credential value. A provider may legitimately rotate credentials while its
// probe initializes, so one changed read retries the probe. A second change is
// treated as a concurrent native login and left for the next reconciliation.
func (m *Manager) runStableAccountProbe(
	providerName string,
	probe func(context.Context) (provider.AccountInfo, error),
) (provider.AccountInfo, *provideraccounts.CredentialSnapshot, error) {
	if m.credentials == nil {
		info, err := probe(m.context())
		return info, nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		before, beforePresent, err := m.readCanonicalCredentialIfPresent(providerName)
		if err != nil {
			return provider.AccountInfo{}, nil, err
		}
		info, err := probe(m.context())
		if err != nil {
			return provider.AccountInfo{}, nil, err
		}
		// Claude's org uuid lives beside the credential (oauthAccount in the
		// same home), not in the wire answer. Reading it HERE — before the
		// after-read — keeps it inside the stability bracket: an external
		// login landing between the probe and this read also rewrites the
		// credential file, so the digest comparison below discards the
		// attempt instead of pairing one account's org with another's tokens.
		info = m.enrichClaudeObservedIdentity(providerName, info)
		after, afterPresent, err := m.readCanonicalCredentialIfPresent(providerName)
		if err != nil {
			return provider.AccountInfo{}, nil, err
		}
		if beforePresent == afterPresent &&
			(!beforePresent || sha256.Sum256(before.Data) == sha256.Sum256(after.Data)) {
			if !afterPresent {
				return info, nil, nil
			}
			// Codex's workspace id is IN the credential bytes; parsing the
			// verified after-snapshot pairs identity and org by construction.
			return EnrichCodexIdentity(providerName, info, after.Data), &after, nil
		}
	}
	return provider.AccountInfo{}, nil, fmt.Errorf(
		"%s credentials changed while identifying the active account; retry",
		providerName,
	)
}

func (m *Manager) readCanonicalCredentialIfPresent(
	providerName string,
) (provideraccounts.CredentialSnapshot, bool, error) {
	snapshot, err := m.credentials.ReadCredentialSnapshot(
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

func (m *Manager) emitProviderAccount(
	providerName string,
	account provideraccounts.Account,
	info provider.AccountInfo,
	generation uint64,
) {
	if m.deps.Accounts != nil {
		m.deps.Accounts.PublishAccount(providerName, account, info, generation)
	}
}

func (m *Manager) emitProviderAccountIfCurrent(
	providerName string,
	account provideraccounts.Account,
	info provider.AccountInfo,
) {
	if m.store == nil {
		// Pre-startup and focused test apps can probe provider identity before
		// the managed-account store is attached. There is no competing
		// account generation in that state, so preserve the probe contract
		// and emit the unscoped identity directly.
		m.emitProviderAccount(providerName, account, info, 0)
		return
	}
	m.mu.RLock()
	active, ok := m.store.Active(providerName, time.Now())
	generation := m.store.Generation(providerName)
	m.mu.RUnlock()
	if !ok || active.ID != account.ID {
		return
	}
	m.emitProviderAccount(providerName, account, info, generation)
}

// PublishAccountIfCurrent emits enriched metadata only if the selection did
// not change while a root-owned live-session read was in flight.
func (m *Manager) PublishAccountIfCurrent(
	providerName string,
	account provideraccounts.Account,
	info provider.AccountInfo,
) {
	m.emitProviderAccountIfCurrent(providerName, account, info)
}

func (m *Manager) emitProviderAccountCleared(providerName string, generation uint64) {
	if m.deps.Accounts != nil {
		m.deps.Accounts.PublishCleared(providerName, generation)
	}
}
