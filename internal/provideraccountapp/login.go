package provideraccountapp

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/providerstatus"

	"github.com/google/uuid"
)

// This file owns the two halves of the native-login saga that surround the
// provider's own sign-in: the PREPARATION that has to happen before a provider
// process runs, and the ADOPTION that verifies what it produced and lands it
// in a slot and the canonical store — with a rollback for every step that can
// fail partway. What happens between them is driven by loginsession.go, which
// owns one live sign-in per provider and the state the UI reads.

// loginAttempt is one sign-in's preparation, carried from beginLogin to
// adoptLogin. The temporary home is the whole point: the provider CLI writes
// whatever it produces there, so nothing it does can disturb the account the
// user is signed in as until adoption decides to switch.
type loginAttempt struct {
	provider string
	binary   string
	home     *provideraccounts.EphemeralHome
}

// beginLogin preserves the account currently in the canonical location and
// cuts the temporary home the provider's sign-in will write into. It runs
// before any provider process starts, because both halves are about what
// happens to the CURRENT login if the new one never completes.
func (m *Manager) beginLogin(providerName string) (*loginAttempt, error) {
	if m.store == nil || m.credentials == nil {
		return nil, errors.New("provider account storage is unavailable")
	}

	binary := m.providerBinaryPath(providerName)
	reconcileMu := m.reconcileMutex(providerName)
	reconcileMu.Lock()
	m.mu.Lock()
	adopted, err := m.adoptCanonicalProviderAccountLocked(providerName, binary)
	m.mu.Unlock()
	reconcileMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("preserve current %s account: %w", providerName, err)
	}
	m.emitReconciledSignIn(adopted)

	home, err := m.credentials.NewEphemeralHome(providerName)
	if err != nil {
		return nil, fmt.Errorf("prepare %s login: %w", providerName, err)
	}
	return &loginAttempt{provider: providerName, binary: binary, home: home}, nil
}

// cleanup removes the temporary home. Adoption cleans up as soon as it has
// read everything it needs, so the coordinator's own teardown call is usually
// a no-op — which is exactly why it is safe on every exit path.
func (a *loginAttempt) cleanup() error {
	if a == nil {
		return nil
	}
	if err := a.home.Cleanup(); err != nil {
		return fmt.Errorf("clean temporary %s login home: %w", a.provider, err)
	}
	return nil
}

// adoptLogin verifies what the provider's sign-in wrote into the temporary
// home, retains only the resulting native credential, and registers
// non-secret metadata. The login's identity decides where it lands: a saved
// account is renewed in place and a new one is appended. Only a login for the
// selected account, or the first login when nothing is selected, replaces
// the canonical credential; a sign-in never switches accounts.
func (m *Manager) adoptLogin(attempt *loginAttempt) (_ ManagedAccount, retErr error) {
	providerName := attempt.provider
	binary := attempt.binary
	loginHome := attempt.home
	reconcileMu := m.reconcileMutex(providerName)

	info, err := m.probeProviderAccountAtHome(providerName, binary, loginHome.Path)
	if err != nil {
		return ManagedAccount{}, fmt.Errorf("verify %s login: %w", providerName, err)
	}
	if providerName == string(provider.Claude) && providerstatus.ClaudeUnauthenticated(info) {
		return ManagedAccount{}, errors.New("Claude login completed without an authenticated account")
	}
	loginCredential, err := m.credentials.ReadEphemeralCredential(loginHome)
	if err != nil {
		return ManagedAccount{}, fmt.Errorf("capture %s login credentials: %w", providerName, err)
	}
	// Organization capture, from material the login home pairs with this
	// exact credential: Codex carries the workspace id inside the
	// credential bytes; Claude's login just wrote the oauthAccount record
	// (org uuid + name) into the home's own .claude.json. Read it before
	// the home is cleaned up. The email-paired acceptance rule applies as
	// everywhere: a record describing a different email is discarded.
	info = EnrichCodexIdentity(providerName, info, loginCredential.Data)
	if providerName == string(provider.Claude) {
		info = EnrichClaudeIdentity(
			info,
			claudeconfig.New(filepath.Join(loginHome.Path, ".claude.json")),
		)
	}
	if err := attempt.cleanup(); err != nil {
		return ManagedAccount{}, err
	}
	reconcileMu.Lock()
	reconcileLocked := true
	defer func() {
		if reconcileLocked {
			reconcileMu.Unlock()
		}
	}()
	if err := m.reconcileExternalProviderAccountWithMutexHeld(providerName); err != nil {
		return ManagedAccount{}, fmt.Errorf(
			"check current %s account before activation: %w",
			providerName,
			err,
		)
	}

	m.mu.Lock()
	accountLocked := true
	defer func() {
		if accountLocked {
			m.mu.Unlock()
		}
	}()

	targetID := uuid.NewString()
	m.backfillCodexOrgIDs(providerName, info.Email)
	existing, found, err := m.findAccountByObservedIdentity(providerName, info)
	if err != nil {
		return ManagedAccount{}, fmt.Errorf(
			"resolve %s login %s against saved accounts: %w",
			providerName,
			DescribeObservedAccount(info),
			err,
		)
	}
	if found {
		targetID = existing.ID
	}
	current, hasCurrent := m.store.Active(providerName, time.Now())
	currentID := ""
	if hasCurrent {
		currentID = current.ID
	}
	if hasCurrent && currentID != targetID {
		account, err := m.saveLoginBesideActiveLocked(
			providerName,
			targetID,
			found,
			info,
			loginCredential.Data,
		)
		if err != nil {
			return ManagedAccount{}, err
		}
		generation := m.store.Generation(providerName)
		m.mu.Unlock()
		accountLocked = false
		reconcileMu.Unlock()
		reconcileLocked = false
		m.emitProviderAccountsChanged()
		m.emitSignIn(SignIn{Provider: providerName, Account: account, Added: !found, Renewed: found})
		if err := m.RefreshProviderAccountUsage(providerName, account.ID); err != nil {
			m.publishUsageError(providerName, account.ID, err)
		}
		return ManagedAccount{Account: account, Generation: generation}, nil
	}
	// Capture before the write so a failure downstream restores exactly
	// what was here — including the case where re-logging into a saved
	// account finds its slot momentarily unreadable. Rolling that back by
	// deletion would cost the user the very account they were repairing.
	savedTarget, err := m.credentials.CaptureAccountCredential(providerName, targetID)
	if err != nil {
		return ManagedAccount{}, err
	}
	if err := m.credentials.WriteAccountCredential(
		providerName,
		targetID,
		loginCredential.Data,
	); err != nil {
		return ManagedAccount{}, fmt.Errorf("save %s login credentials: %w", providerName, err)
	}
	restoreTarget := func() error {
		return m.credentials.RestoreAccountCredential(savedTarget)
	}

	preserveID, currentCredential, err := m.reconciledActiveCredentialLocked(
		providerName,
		currentID,
		targetID,
	)
	if err != nil {
		if restoreErr := restoreTarget(); restoreErr != nil {
			return ManagedAccount{}, fmt.Errorf(
				"%w (saved credential rollback also failed: %v)",
				err,
				restoreErr,
			)
		}
		return ManagedAccount{}, err
	}
	if err := m.credentials.ActivateWithSnapshot(
		providerName,
		preserveID,
		targetID,
		currentCredential,
	); err != nil {
		if restoreErr := restoreTarget(); restoreErr != nil {
			return ManagedAccount{}, fmt.Errorf(
				"activate %s account: %w (saved credential rollback also failed: %v)",
				providerName,
				err,
				restoreErr,
			)
		}
		return ManagedAccount{}, fmt.Errorf("activate %s account: %w", providerName, err)
	}
	if err := m.verifyCanonicalProviderCredentialLocked(
		providerName,
		loginCredential.Data,
	); err != nil {
		if restoreErr := restoreTarget(); restoreErr != nil {
			return ManagedAccount{}, errors.Join(err, restoreErr)
		}
		return ManagedAccount{}, err
	}

	account, err := m.store.UpsertAndActivateCredential(
		AccountFromInfo(targetID, providerName, info),
	)
	if err != nil {
		var rollbackErrs []error
		if restoreErr := restoreTarget(); restoreErr != nil {
			rollbackErrs = append(rollbackErrs, restoreErr)
		}
		if rollbackErr := m.rollbackProviderAccountActivation(providerName, targetID, currentID); rollbackErr != nil {
			rollbackErrs = append(rollbackErrs, rollbackErr)
		}
		if len(rollbackErrs) > 0 {
			return ManagedAccount{}, fmt.Errorf(
				"%w (credential rollback also failed: %v)",
				err,
				errors.Join(rollbackErrs...),
			)
		}
		return ManagedAccount{}, err
	}

	// A fresh sign-in is where the provider sets the session deadline this
	// account will live by, so it is the first place worth reading it.
	account = m.withRecordedLoginExpiry(account, loginCredential.Data)
	m.rememberProviderCredentialFingerprintLocked(providerName, loginCredential.Data)
	generation := m.store.Generation(providerName)
	m.mu.Unlock()
	accountLocked = false
	reconcileMu.Unlock()
	reconcileLocked = false
	m.invalidateProviderAccountProbe(providerName, binary)
	m.emitProviderAccount(providerName, account, info, generation)
	// A sign-in adds a card, which no per-account frame can express.
	m.emitProviderAccountsChanged()
	m.applySelection(providerName, generation, account.ID)
	m.emitSignIn(SignIn{
		Provider: providerName, Account: account, Added: !found, Renewed: found, Active: true,
	})
	if err := m.RefreshProviderAccountUsage(providerName, account.ID); err != nil {
		// Login and activation succeeded. Quota refresh is independently
		// retryable from Settings and must not roll the account back.
		m.publishUsageError(providerName, account.ID, err)
	}
	return ManagedAccount{Account: account, Active: true, Generation: generation}, nil
}

// saveLoginBesideActiveLocked lands a login for an account other than the
// selected one in that account's slot, renewing a saved account or appending
// a new one. The canonical credential, the selection and its generation are
// untouched, so nothing running on the selected account notices. The caller
// holds mu.
func (m *Manager) saveLoginBesideActiveLocked(
	providerName string,
	accountID string,
	saved bool,
	info provider.AccountInfo,
	credential []byte,
) (provideraccounts.Account, error) {
	previous, err := m.credentials.CaptureAccountCredential(providerName, accountID)
	if err != nil {
		return provideraccounts.Account{}, err
	}
	if err := m.credentials.WriteAccountCredential(providerName, accountID, credential); err != nil {
		return provideraccounts.Account{}, fmt.Errorf("save %s login credentials: %w", providerName, err)
	}
	metadata := AccountFromInfo(accountID, providerName, info)
	var account provideraccounts.Account
	if saved {
		account, err = m.store.UpdateMetadata(metadata)
	} else {
		account, err = m.store.Add(metadata)
	}
	if err != nil {
		if restoreErr := m.credentials.RestoreAccountCredential(previous); restoreErr != nil {
			return provideraccounts.Account{}, fmt.Errorf(
				"%w (saved credential rollback also failed: %v)",
				err,
				restoreErr,
			)
		}
		return provideraccounts.Account{}, err
	}
	return m.withRecordedLoginExpiry(account, credential), nil
}
