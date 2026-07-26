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
// An inactive account is probed from a short-lived home seeded with only its
// credential; a rotation there reaches the canonical home only after selection
// and fingerprint validation. The selected Claude account is probed in the
// canonical home instead, because a refresh must not fork its rotation chain —
// see probeSelectedClaudeRateLimits.
func (a *App) RefreshProviderAccountUsage(
	providerName,
	accountID string,
) (retErr error) {
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
	if a.providerAccounts == nil || a.providerCredentials == nil {
		return errors.New("provider account storage is unavailable")
	}
	refreshMu := a.providerCredentialReconcileMutex(providerName)
	refreshMu.Lock()
	defer refreshMu.Unlock()

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

	// The selected Claude account refreshes in the canonical home, where
	// Claude's own cross-process refresh lock lives, so it needs no copy of
	// itself to probe against. See probeSelectedClaudeRateLimits.
	refreshesInCanonicalHome := isSelected && providerName == string(provider.Claude)

	var (
		snapshot            provider.RateLimitsSnapshot
		refreshedCredential []byte
		ephemeral           *provideraccounts.EphemeralHome
		// observed stays zero unless the probe itself reported an identity.
		observed provider.AccountInfo
	)
	if !refreshesInCanonicalHome {
		ephemeral, err = a.providerCredentials.NewEphemeralHomeWithCredential(
			providerName,
			credential.Data,
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
		if refreshesInCanonicalHome {
			snapshot, refreshedCredential, err = a.probeSelectedClaudeRateLimits(
				ctx,
				selection,
				credential.Data,
			)
		} else {
			snapshot, refreshedCredential, err = a.probeClaudeRateLimitsForSelection(
				ctx,
				claudeRateLimitSelection{
					providerAccountSelection: selection,
					EphemeralHome:            ephemeral,
					Env: map[string]string{
						"CLAUDE_CONFIG_DIR": ephemeral.Path,
					},
				},
			)
		}
		if err != nil {
			return err
		}
	case string(provider.Codex):
		info, probeErr := codex.ProbeAccount(ctx, codex.ProbeConfig{
			Binary: a.providerBinaryPath(providerName),
			Env:    map[string]string{"CODEX_HOME": ephemeral.Path},
			OnSnapshot: func(value provider.RateLimitsSnapshot) {
				snapshot = value
			},
		})
		if probeErr != nil {
			return probeErr
		}
		if len(snapshot.Limits) == 0 {
			return errors.New("Codex did not return usage limits")
		}
		refreshed, readErr := a.providerCredentials.ReadEphemeralCredential(ephemeral)
		if readErr != nil {
			return fmt.Errorf("capture refreshed Codex credentials: %w", readErr)
		}
		refreshedCredential = refreshed.Data
		observed = info
	}

	a.providerAccountMu.Lock()
	commit, err := a.commitProviderUsageRefreshLocked(providerUsageRefresh{
		providerName:           providerName,
		accountID:              accountID,
		generation:             generation,
		isSelected:             isSelected,
		probed:                 credential.Data,
		refreshed:              refreshedCredential,
		rotatedInCanonicalHome: refreshesInCanonicalHome,
		observed:               observed,
	})
	a.providerAccountMu.Unlock()
	if err != nil {
		return err
	}
	if commit.account != nil {
		a.emitProviderAccount(
			providerName,
			*commit.account,
			providerAccountInfo(*commit.account),
			commit.generation,
		)
	}
	snapshot.Provider = providerName
	snapshot.AccountID = accountID
	a.emitRateLimitsSnapshot(snapshot)
	return nil
}

// providerUsageRefresh is one completed usage probe awaiting commit.
type providerUsageRefresh struct {
	providerName string
	accountID    string
	generation   uint64
	isSelected   bool
	// probed is the credential the probe started from.
	probed []byte
	// refreshed is the rotated credential, empty when nothing rotated.
	refreshed []byte
	// rotatedInCanonicalHome reports that the provider wrote the rotation
	// straight into the canonical store, so it is already published there.
	rotatedInCanonicalHome bool
	// observed is the identity the provider reported during the probe, when
	// it reports one at all.
	observed provider.AccountInfo
}

type providerUsageCommit struct {
	account    *provideraccounts.Account
	generation uint64
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

	// What the canonical store must still hold: the rotated value when the
	// provider rotated in place, otherwise whatever the probe started from.
	canonical := refresh.probed
	if refresh.rotatedInCanonicalHome {
		canonical = refresh.refreshed
	}
	if refresh.isSelected {
		if err := a.verifyCanonicalProviderCredentialLocked(
			refresh.providerName,
			canonical,
		); err != nil {
			return commit, err
		}
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

	// latest is the value every copy of this account must end up holding.
	latest := refresh.refreshed
	if len(latest) == 0 {
		latest = refresh.probed
	}
	if !refresh.isSelected {
		if len(refresh.refreshed) == 0 {
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
