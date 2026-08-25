package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"
)

// RefreshProviderAccountUsage probes one saved account without changing the
// provider-wide selection.
//
// An inactive Codex account is probed from a short-lived home seeded with only
// its credential; a rotation there reaches the canonical home only after
// selection and fingerprint validation. Claude never uses a temporary home
// here: the selected account is probed in the canonical home (the only place
// its single-use rotation may happen — see probeSelectedClaudeRateLimits) and
// an inactive one is read over HTTP without refreshing at all — see
// probeInactiveClaudeRateLimits.
//
// This manual path deliberately bypasses the usage gate's cooldown — a user
// demand should not silently coalesce away. Server-imposed 429 backoffs are
// enforced inside refreshProviderAccountUsage, scoped to the one account that
// earned them: the throttle is per-bearer, so another account's card must stay
// refreshable while the selected account waits one out.
func (a *App) RefreshProviderAccountUsage(providerName, accountID string) error {
	return a.refreshProviderAccountUsage(a.lifeCtx(), providerName, accountID)
}

func (a *App) refreshProviderAccountUsage(
	ctx context.Context,
	providerName,
	accountID string,
) (retErr error) {
	if err := validateManagedProvider(providerName); err != nil {
		return err
	}
	// Refuse up front while this account's 429 backoff holds — retrying into
	// one only extends the penalty. The outcome of the probe (and only the
	// probe: a request has to have been sent for its result to say anything
	// about the throttle) is recorded on the way out so every later caller,
	// manual or automatic, is held the same way.
	backoffHold := func() error {
		if remaining := a.usageProbe.backoff.Remaining(providerName, accountID); remaining > 0 {
			return fmt.Errorf(
				"the usage endpoint rate limited this account; try again in %s",
				remaining.Round(time.Second),
			)
		}
		return nil
	}
	if err := backoffHold(); err != nil {
		return err
	}
	if a.providerAccounts == nil || a.providerCredentials == nil {
		return errors.New("provider account storage is unavailable")
	}
	refreshMu := a.providerCredentialReconcileMutex(providerName)
	refreshMu.Lock()
	defer refreshMu.Unlock()
	// Re-checked now that this call holds the mutex: the manual path and the
	// automatic poll gate independently, so both can pass the pre-lock check,
	// serialize here, and the second would otherwise send a request straight
	// into the hold the first just earned.
	if err := backoffHold(); err != nil {
		return err
	}

	a.providerAccountMu.RLock()
	account, exists := a.providerAccounts.Get(providerName, accountID, time.Now())
	if !exists {
		a.providerAccountMu.RUnlock()
		return fmt.Errorf("%s account %q is not saved", providerName, accountID)
	}
	active, isActive := a.providerAccounts.Active(providerName, time.Now())
	isSelected := isActive && active.ID == accountID
	generation := a.providerAccounts.Generation(providerName)
	selection := providerAccountSelection{
		Generation: generation,
		AccountID:  accountID,
		Account:    providerAccountInfo(account),
	}
	var credential provideraccounts.CredentialSnapshot
	var err error
	if isSelected {
		selection = a.providerAccountSelectionLocked(providerName)
		credential, err = a.providerCredentials.ReadCredentialSnapshot(
			providerName,
			"",
			true,
		)
	} else {
		credential, err = a.providerCredentials.ReadCredentialSnapshot(
			providerName,
			accountID,
			false,
		)
	}
	if err != nil {
		a.providerAccountMu.RUnlock()
		return fmt.Errorf("read saved %s credentials for usage refresh: %w", providerName, err)
	}
	a.providerAccountMu.RUnlock()

	refresh := providerUsageRefresh{
		providerName: providerName,
		accountID:    accountID,
		generation:   generation,
		isSelected:   isSelected,
		probed:       credential.Data,
	}

	var (
		snapshot provider.RateLimitsSnapshot
		probeErr error
		probeRan bool
	)
	// The ledger hears about the probe's own outcome, nothing else: a success
	// proves the throttle lifted even if the bookkeeping after it fails, and
	// an operation that never probed (a backoff refusal, an unreadable
	// credential) proved nothing in either direction.
	defer func() {
		if probeRan {
			a.usageProbe.backoff.Note(providerName, accountID, probeErr)
		}
	}()
	// Only Codex probes from a temporary home now: its app-server has to run
	// to answer at all, so it needs a home to run in, and its rotation is
	// captured out of that home below. Claude reads its usage over HTTP from
	// the credential bytes directly — selected or not, no home, no CLI.
	var ephemeral *provideraccounts.EphemeralHome
	if providerName == string(provider.Codex) {
		ephemeral, err = a.providerCredentials.NewEphemeralHomeWithCredential(
			providerName,
			credential.Data,
			accountID,
		)
		if err != nil {
			return err
		}
		defer func() {
			if cleanupErr := ephemeral.Cleanup(); cleanupErr != nil {
				retErr = errors.Join(retErr, cleanupErr)
			}
		}()
	}

	switch providerName {
	case string(provider.Claude):
		probeRan = true
		if refresh.refreshesInCanonicalHome() {
			snapshot, refresh.refreshed, probeErr = a.probeSelectedClaudeRateLimits(
				ctx,
				selection,
				credential.Data,
			)
			// The canonical refresh can rotate the credential and still fail:
			// the usage retry after a successful token rotation can be
			// throttled even though the token endpoint was not. Claude refresh
			// tokens are single-use, so a rotation must reach the commit even
			// when the probe's answer is an error — dropping it here would
			// leave the slot on a consumed token.
			if probeErr != nil && len(refresh.refreshed) == 0 {
				return mapProviderAccountLoginError(providerName, account, probeErr)
			}
		} else {
			snapshot, probeErr = a.probeInactiveClaudeRateLimits(ctx, credential.Data)
			if probeErr != nil {
				return mapProviderAccountLoginError(providerName, account, probeErr)
			}
		}
	case string(provider.Codex):
		probeCfg := a.codexProbeConfig(
			a.providerBinaryPath(providerName),
			map[string]string{"CODEX_HOME": ephemeral.Path},
		)
		probeCfg.OnSnapshot = func(value provider.RateLimitsSnapshot) {
			snapshot = value
		}
		probeRan = true
		info, codexErr := codex.ProbeAccount(ctx, probeCfg)
		if codexErr != nil {
			probeErr = codexErr
			return codexErr
		}
		if len(snapshot.Limits) == 0 {
			probeErr = errors.New("Codex did not return usage limits")
			return probeErr
		}
		refreshed, readErr := a.providerCredentials.ReadEphemeralCredential(ephemeral)
		if readErr != nil {
			return fmt.Errorf("capture refreshed Codex credentials: %w", readErr)
		}
		refresh.refreshed = refreshed.Data
		refresh.observed = info
	}

	// A Codex rotation for a non-selected account is durable NOWHERE once the
	// deferred ephemeral cleanup runs — its slot is the only holder of the
	// chain. Persist it before the commit's selection re-validation gets a
	// chance to refuse (a metadata generation bump is no reason to discard
	// a rotation): the slot is keyed by account ID, and the reconcile
	// mutex held for this whole call keeps switches, logins, and removals
	// from re-homing that ID underneath us. A failed write falls through —
	// the commit's own slot write below is the retry.
	// (Selected accounts stay out of this: their commit goes canonical-first
	// through CommitSelectedCredential, and a slot-ahead-of-canonical write
	// would invert that ordering. Inactive CLAUDE accounts never reach here
	// with anything: they no longer refresh, so their probe returns no bytes.)
	if !refresh.isSelected &&
		len(refresh.refreshed) > 0 &&
		!bytes.Equal(refresh.refreshed, refresh.probed) {
		refresh.slotPersisted = a.providerCredentials.WriteAccountCredential(
			refresh.providerName,
			refresh.accountID,
			refresh.refreshed,
		) == nil
	}

	a.providerAccountMu.Lock()
	commit, commitErr := a.commitProviderUsageRefreshLocked(refresh)
	a.providerAccountMu.Unlock()

	// Metadata the commit enriched is already written, so publish it even when
	// a later step failed.
	if commit.account != nil {
		a.emitProviderAccount(
			providerName,
			*commit.account,
			providerAccountInfo(*commit.account),
			commit.generation,
		)
	}
	// The snapshot is what the probe measured. It does not depend on the
	// credential bookkeeping around it settling, so withholding it on a commit
	// failure would blank the rings for data that was fetched successfully. A
	// failed probe measured nothing, though — its zero snapshot must not reach
	// the rings even when a rotation was worth committing.
	if commit.snapshotDescribesAccount && probeErr == nil {
		snapshot.Provider = providerName
		snapshot.AccountID = accountID
		a.emitRateLimitsSnapshot(snapshot)
	}
	return errors.Join(probeErr, commitErr)
}

// providerUsageRefresh is one usage probe awaiting commit.
type providerUsageRefresh struct {
	providerName string
	accountID    string
	generation   uint64
	isSelected   bool
	// probed is the credential the probe started from.
	probed []byte
	// refreshed is the rotated credential, empty when nothing rotated.
	// Only two probes can fill it: the selected Claude account's canonical
	// refresh, and any Codex probe (which rotates inside its temporary home).
	// An inactive Claude account never rotates, so its refresh is always empty.
	refreshed []byte
	// observed is the identity the provider reported during the probe, when
	// it reports one at all. Stays zero otherwise.
	observed provider.AccountInfo
	// slotPersisted records that the rotated credential already reached this
	// account's slot ahead of the commit, so the commit's non-selected slot
	// write can skip a redundant rewrite.
	slotPersisted bool
}

// refreshesInCanonicalHome reports that this account's token refresh, if the
// probe turns out to need one, happens in the canonical home. Only the
// selected Claude account holds the canonical credential, and it is the only
// Claude account AO refreshes at all: an inactive one is read-only, because a
// rotation whose response is lost cannot be recovered from anywhere — see
// probeInactiveClaudeRateLimits. Claude's single-use rotation is serialized on
// a lockfile scoped to the config home, so the selected account's refresh must
// happen there and nowhere else — see probeSelectedClaudeRateLimits.
func (r providerUsageRefresh) refreshesInCanonicalHome() bool {
	return r.isSelected && r.providerName == string(provider.Claude)
}

// canonicalCredential is the value the canonical native store must still hold
// for this probe's outcome to be committable. A rotation performed in the
// canonical home is already published there; every other outcome leaves
// canonical holding exactly what the probe started from.
//
// Both halves of that condition are load-bearing. Deriving this from "would
// refresh in the canonical home" alone claimed a publish on every selected
// Claude probe — including the common one where the token was still valid and
// nothing rotated at all, which then compared the canonical file against an
// empty expectation that can never match.
func (r providerUsageRefresh) canonicalCredential() []byte {
	if r.refreshesInCanonicalHome() && len(r.refreshed) > 0 {
		return r.refreshed
	}
	return r.probed
}

type providerUsageCommit struct {
	account    *provideraccounts.Account
	generation uint64
	// snapshotDescribesAccount reports that this probe provably measured
	// refresh.accountID, so its snapshot stays publishable even when the
	// credential bookkeeping underneath failed. It is false only where the
	// attribution itself is in doubt: a selection that moved under the probe,
	// or a provider that authenticated as somebody else.
	snapshotDescribesAccount bool
}

// commitProviderUsageRefreshLocked publishes the outcome of one usage probe:
// it re-checks that the selection the probe was made under still holds, then
// brings every copy of that account's credential up to the probed value. The
// caller holds providerAccountMu for the whole call.
func (a *App) commitProviderUsageRefreshLocked(
	refresh providerUsageRefresh,
) (providerUsageCommit, error) {
	commit := providerUsageCommit{generation: refresh.generation}
	if err := a.validateProviderUsageRefreshSelectionLocked(
		refresh.providerName,
		refresh.accountID,
		refresh.generation,
		refresh.isSelected,
	); err != nil {
		return commit, err
	}
	if refresh.providerName == string(provider.Codex) {
		observedAccountID, updated, err := a.accountIDForObservedIdentity(
			refresh.providerName,
			refresh.accountID,
			refresh.observed,
		)
		if err != nil {
			return commit, err
		}
		if observedAccountID != refresh.accountID {
			return commit, fmt.Errorf(
				"Codex account %q authenticated as saved account %q",
				refresh.accountID,
				observedAccountID,
			)
		}
		commit.account = updated
	}
	// Everything above decides whether this probe measured refresh.accountID at
	// all; the identity checks come first for that reason. Past this line the
	// attribution holds, so the caller may publish the snapshot even if the
	// credential work below fails.
	commit.snapshotDescribesAccount = true

	canonical := refresh.canonicalCredential()
	if refresh.isSelected {
		if err := a.verifyCanonicalProviderCredentialLocked(
			refresh.providerName,
			canonical,
		); err != nil {
			return commit, err
		}
	}

	// latest is the value every copy of this account must end up holding.
	latest := refresh.refreshed
	if len(latest) == 0 {
		latest = refresh.probed
	}
	if !refresh.isSelected {
		if len(refresh.refreshed) == 0 || refresh.slotPersisted {
			return commit, nil
		}
		if err := a.providerCredentials.WriteAccountCredential(
			refresh.providerName,
			refresh.accountID,
			refresh.refreshed,
		); err != nil {
			return commit, fmt.Errorf(
				"save refreshed %s credentials: %w",
				refresh.providerName,
				err,
			)
		}
		return commit, nil
	}

	if bytes.Equal(latest, canonical) {
		// The canonical store already holds it — just verified. Only the
		// saved slot needs to catch up; rewriting the file the provider
		// watches for its own rotations would be a needless race with it.
		if err := a.providerCredentials.WriteAccountCredential(
			refresh.providerName,
			refresh.accountID,
			latest,
		); err != nil {
			return commit, fmt.Errorf(
				"save current %s credentials: %w",
				refresh.providerName,
				err,
			)
		}
	} else {
		if err := a.providerCredentials.CommitSelectedCredential(
			refresh.providerName,
			refresh.accountID,
			latest,
		); err != nil {
			delete(a.providerCredentialFingerprints, refresh.providerName)
			return commit, err
		}
		if err := a.verifyCanonicalProviderCredentialLocked(
			refresh.providerName,
			latest,
		); err != nil {
			return commit, err
		}
	}
	if refresh.providerName == string(provider.Codex) && !bytes.Equal(latest, refresh.probed) {
		advanced, err := a.providerAccounts.AdvanceActiveCredential(
			refresh.providerName,
			refresh.accountID,
		)
		if err != nil {
			delete(a.providerCredentialFingerprints, refresh.providerName)
			return commit, fmt.Errorf(
				"record refreshed %s credentials: %w",
				refresh.providerName,
				err,
			)
		}
		commit.generation = a.providerAccounts.Generation(refresh.providerName)
		commit.account = &advanced
	}
	a.rememberProviderCredentialFingerprintLocked(refresh.providerName, latest)
	return commit, nil
}

func (a *App) validateProviderUsageRefreshSelectionLocked(
	providerName string,
	accountID string,
	generation uint64,
	wasSelected bool,
) error {
	if a.providerAccounts.Generation(providerName) != generation {
		return fmt.Errorf("%s account changed while refreshing usage; retry", providerName)
	}
	if _, exists := a.providerAccounts.Get(providerName, accountID, time.Now()); !exists {
		return fmt.Errorf("%s account %q was removed while refreshing usage", providerName, accountID)
	}
	active, hasActive := a.providerAccounts.Active(providerName, time.Now())
	isSelected := hasActive && active.ID == accountID
	if isSelected != wasSelected {
		return fmt.Errorf("%s account selection changed while refreshing usage; retry", providerName)
	}
	return nil
}
