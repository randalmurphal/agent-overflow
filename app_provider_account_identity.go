package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"

	"github.com/google/uuid"
)

// reconcileExternalProviderAccount detects a provider-native login completed
// outside Agent Overflow. The common path reads a small credential value and
// compares an in-memory digest. Only a changed (or not-yet-observed) value
// starts the provider's zero-token identity probe.
//
// The digest never leaves this process. Credential bytes remain owned by the
// provider-native store and are discarded immediately after hashing or copying
// into another provider-native credential slot.
func (a *App) reconcileExternalProviderAccount(providerName string) error {
	if a.providerAccounts == nil || a.providerCredentials == nil {
		return nil
	}
	if providerName != string(provider.Claude) && providerName != string(provider.Codex) {
		return nil
	}
	reconcileMu := a.providerCredentialReconcileMutex(providerName)
	reconcileMu.Lock()
	defer reconcileMu.Unlock()
	return a.reconcileExternalProviderAccountWithMutexHeld(providerName)
}

// reconcileExternalProviderAccountWithMutexHeld is the reconciliation body for
// managed account mutations that must keep provider identity probes and usage
// refreshes ordered through their canonical credential write. The caller holds
// providerCredentialReconcileMutex(providerName).
func (a *App) reconcileExternalProviderAccountWithMutexHeld(
	providerName string,
) error {
	before, present, err := a.readCanonicalCredentialIfPresent(providerName)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}

	a.providerAccountMu.RLock()
	known, observed := a.providerCredentialFingerprints[providerName]
	a.providerAccountMu.RUnlock()
	if observed && known == sha256.Sum256(before.Data) {
		return nil
	}

	// An external login process does not participate in providerAccountMu, so
	// the identity has to be paired with a canonical credential value that did
	// not move across the probe — otherwise one account's email could be stored
	// against another account's tokens. runStableAccountProbe enforces that
	// pairing and, unlike a single before/after comparison, tolerates the
	// provider legitimately rotating the token while its probe initializes.
	binary := a.providerBinaryPath(providerName)
	info, credential, err := a.runStableAccountProbe(
		providerName,
		a.canonicalProviderAccountProbe(providerName, binary),
	)
	if err != nil {
		return fmt.Errorf("identify externally selected %s account: %w", providerName, err)
	}
	if credential == nil {
		// The login was signed out while it was being identified. There is
		// nothing to adopt, and the next reconciliation sees the absence.
		return nil
	}
	fingerprint := sha256.Sum256(credential.Data)

	a.providerAccountMu.Lock()
	// Serialize against Agent Overflow's own account switch and repeat the
	// stability check inside that boundary before changing metadata.
	current, err := a.providerCredentials.ReadCredentialSnapshot(providerName, "", true)
	if err != nil {
		a.providerAccountMu.Unlock()
		return fmt.Errorf("recheck active %s credentials before activation: %w", providerName, err)
	}
	if sha256.Sum256(current.Data) != fingerprint {
		a.providerAccountMu.Unlock()
		return fmt.Errorf("%s credentials changed before account activation; retry", providerName)
	}

	account, changed, err := a.reconcileObservedAccountLocked(providerName, info, current)
	if err == nil {
		if a.providerCredentialFingerprints == nil {
			a.providerCredentialFingerprints = make(map[string][32]byte)
		}
		a.providerCredentialFingerprints[providerName] = fingerprint
	}
	generation := a.providerAccounts.Generation(providerName)
	a.providerAccountMu.Unlock()
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}

	a.invalidateProviderAccountProbe(providerName, binary)
	a.emitProviderAccount(providerName, account, info, generation)

	// Claude processes intentionally follow the canonical native credential
	// store, so every live Claude session adopts the observed account without
	// a restart. Codex app-servers cache auth and remain attributed to their
	// old account until the existing reconnect-on-next-send gate runs.
	if providerName == string(provider.Claude) {
		a.applyProviderAccountSelectionToSessions(providerName, generation, account.ID)
	}
	return nil
}

func (a *App) providerCredentialReconcileMutex(providerName string) *sync.Mutex {
	if providerName == string(provider.Claude) {
		return &a.claudeCredentialReconcileMu
	}
	return &a.codexCredentialReconcileMu
}

// canonicalProviderAccountProbe asks the provider who is logged in to its
// canonical native home. It deliberately sets no config-home override: Claude
// keys "is this the default home" off the variable being absent rather than off
// its value, and on macOS a non-default home hashes into a different Keychain
// service.
func (a *App) canonicalProviderAccountProbe(
	providerName string,
	binary string,
) func(context.Context) (provider.AccountInfo, error) {
	return func(ctx context.Context) (provider.AccountInfo, error) {
		switch providerName {
		case string(provider.Claude):
			return claude.ProbeAccount(ctx, a.claudeProbeConfig(binary, nil))
		case string(provider.Codex):
			return codex.ProbeIdentity(ctx, a.codexProbeConfig(binary, nil))
		default:
			return provider.AccountInfo{}, fmt.Errorf("unsupported provider %q", providerName)
		}
	}
}

// reconcileObservedAccountLocked maps an observed provider identity onto the
// saved credential set. The caller has verified that the canonical credential
// value is stable and holds providerAccountMu.
func (a *App) reconcileObservedAccountLocked(
	providerName string,
	info provider.AccountInfo,
	credential provideraccounts.CredentialSnapshot,
) (
	account provideraccounts.Account,
	changed bool,
	retErr error,
) {
	active, hasActive := a.providerAccounts.Active(providerName, time.Now())
	email := strings.TrimSpace(info.Email)
	var (
		target provideraccounts.Account
		found  bool
	)
	if email == "" {
		// Codex documents email as nullable and API-key auth has no user email.
		// Without a provider identity, retain the current assignment rather
		// than manufacturing a duplicate. There is no safe assignment when no
		// managed account is active yet.
		if !hasActive {
			return active, false, nil
		}
		target = active
		found = true
	} else {
		target, found = a.providerAccounts.FindByEmail(providerName, email)
		if !found && hasActive && strings.TrimSpace(active.Email) == "" {
			// Enrich an account adopted before identity was available rather
			// than manufacturing a duplicate the first time the provider
			// returns email.
			target = active
			found = true
		}
	}
	if !found {
		target.ID = uuid.NewString()
		target.Provider = providerName
	}
	saved, err := a.providerCredentials.CaptureAccountCredential(providerName, target.ID)
	if err != nil {
		return provideraccounts.Account{}, false, err
	}
	restoreCredentialOnError := true
	defer func() {
		if !restoreCredentialOnError || retErr == nil {
			return
		}
		if cleanupErr := a.providerCredentials.RestoreAccountCredential(saved); cleanupErr != nil {
			retErr = errors.Join(
				retErr,
				fmt.Errorf(
					"restore saved %s credential %s after external login failure: %w",
					providerName,
					target.ID,
					cleanupErr,
				),
			)
		}
	}()

	if err := a.providerCredentials.WriteAccountCredential(
		providerName,
		target.ID,
		credential.Data,
	); err != nil {
		return provideraccounts.Account{}, false, fmt.Errorf(
			"preserve externally selected %s credentials: %w",
			providerName,
			err,
		)
	}
	stable, err := a.providerCredentials.ReadCredentialSnapshot(providerName, "", true)
	if err != nil {
		return provideraccounts.Account{}, false, fmt.Errorf(
			"recheck externally selected %s credentials: %w",
			providerName,
			err,
		)
	}
	if sha256.Sum256(stable.Data) != sha256.Sum256(credential.Data) {
		return provideraccounts.Account{}, false, fmt.Errorf(
			"%s credentials changed before metadata activation; retry",
			providerName,
		)
	}

	updated := target
	if email != "" {
		updated = accountFromInfo(target.ID, providerName, info)
	}
	account, err = a.providerAccounts.UpsertAndActivateCredential(updated)
	if err != nil {
		return provideraccounts.Account{}, false, fmt.Errorf(
			"activate externally selected %s account metadata: %w",
			providerName,
			err,
		)
	}
	restoreCredentialOnError = false
	// Reaching this point means the canonical credential fingerprint changed.
	// Publish the new generation even when provider identity metadata stayed
	// identical so Codex threads reconnect before their next safe send.
	changed = true
	return account, changed, nil
}

// rememberProviderCredentialFingerprintLocked records the exact credential
// value Agent Overflow just activated. It deliberately does not reread the
// canonical file: an external login can replace that file without taking
// providerAccountMu, and blessing the later value would suppress reconciliation.
// The caller holds providerAccountMu.
func (a *App) rememberProviderCredentialFingerprintLocked(
	providerName string,
	credential []byte,
) {
	if a.providerCredentialFingerprints == nil {
		a.providerCredentialFingerprints = make(map[string][32]byte)
	}
	a.providerCredentialFingerprints[providerName] = sha256.Sum256(credential)
}

// verifyCanonicalProviderCredentialLocked confirms an activation still owns
// the canonical native store before metadata is committed. External provider
// login processes do not share providerAccountMu, so every managed write needs
// this explicit compare. The caller holds providerAccountMu.
func (a *App) verifyCanonicalProviderCredentialLocked(
	providerName string,
	expected []byte,
) error {
	current, err := a.providerCredentials.ReadCredential(providerName, "", true)
	if err != nil {
		delete(a.providerCredentialFingerprints, providerName)
		return fmt.Errorf("verify active %s credentials: %w", providerName, err)
	}
	if sha256.Sum256(current) != sha256.Sum256(expected) {
		delete(a.providerCredentialFingerprints, providerName)
		return fmt.Errorf(
			"%s credentials changed during account activation; retry",
			providerName,
		)
	}
	return nil
}

// accountIDForObservedIdentity prevents a usage snapshot from being stamped
// onto the selected metadata account when the fresh provider probe identifies
// a different login. Empty-email modes cannot be disambiguated and retain the
// expected saved-account assignment.
func (a *App) accountIDForObservedIdentity(
	providerName string,
	expectedAccountID string,
	info provider.AccountInfo,
) (string, *provideraccounts.Account, error) {
	email := strings.TrimSpace(info.Email)
	if email == "" || a.providerAccounts == nil {
		return expectedAccountID, nil, nil
	}
	if expected, ok := a.providerAccounts.Get(providerName, expectedAccountID, time.Now()); ok &&
		strings.EqualFold(expected.Email, email) {
		return expectedAccountID, nil, nil
	}
	if observed, ok := a.providerAccounts.FindByEmail(providerName, email); ok {
		return observed.ID, nil, nil
	}
	if expected, ok := a.providerAccounts.Get(providerName, expectedAccountID, time.Now()); ok &&
		strings.TrimSpace(expected.Email) == "" {
		metadata := accountFromInfo(expected.ID, providerName, info)
		if metadata.DisplayName == "" {
			metadata.DisplayName = expected.DisplayName
		}
		if metadata.SubscriptionType == "" {
			metadata.SubscriptionType = expected.SubscriptionType
		}
		if metadata.TokenSource == "" {
			metadata.TokenSource = expected.TokenSource
		}
		if metadata.APIProvider == "" {
			metadata.APIProvider = expected.APIProvider
		}
		updated, err := a.providerAccounts.UpdateMetadata(metadata)
		if err != nil {
			return "", nil, fmt.Errorf("save observed %s account identity: %w", providerName, err)
		}
		return updated.ID, &updated, nil
	}
	return "", nil, fmt.Errorf(
		"%s identity probe returned unsaved account %s instead of selected account",
		providerName,
		email,
	)
}
