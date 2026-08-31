package provideraccountapp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/providerstatus"

	"github.com/google/uuid"
)

// This file owns the native-login saga: run the provider's own browser flow
// in a temporary home, verify what it produced, and land it in a slot and the
// canonical store — with a rollback for every step that can fail partway.

// LoginProviderAccount runs the provider's native browser login in a
// short-lived isolated home, retains only the resulting native credential,
// atomically activates it, and registers non-secret metadata.
func (m *Manager) LoginProviderAccount(
	providerName string,
) (_ ManagedAccount, retErr error) {
	if m.shuttingDown() {
		return ManagedAccount{}, m.shutdownError()
	}
	if err := ValidateProvider(providerName); err != nil {
		return ManagedAccount{}, err
	}
	if m.store == nil || m.credentials == nil {
		return ManagedAccount{}, errors.New("provider account storage is unavailable")
	}

	binary := m.providerBinaryPath(providerName)
	reconcileMu := m.reconcileMutex(providerName)
	reconcileMu.Lock()
	m.mu.Lock()
	if err := m.adoptCanonicalProviderAccountLocked(providerName, binary); err != nil {
		m.mu.Unlock()
		reconcileMu.Unlock()
		return ManagedAccount{}, fmt.Errorf("preserve current %s account: %w", providerName, err)
	}
	m.mu.Unlock()
	reconcileMu.Unlock()

	loginHome, err := m.credentials.NewEphemeralHome(providerName)
	if err != nil {
		return ManagedAccount{}, fmt.Errorf("prepare %s login: %w", providerName, err)
	}
	defer func() {
		if cleanupErr := loginHome.Cleanup(); cleanupErr != nil {
			retErr = errors.Join(
				retErr,
				fmt.Errorf("clean temporary %s login home: %w", providerName, cleanupErr),
			)
		}
	}()

	switch providerName {
	case string(provider.Claude):
		if m.deps.BrowserExecutable == nil {
			return ManagedAccount{}, errors.New("locate browser bridge: browser executable resolver unavailable")
		}
		executable, err := m.deps.BrowserExecutable()
		if err != nil {
			return ManagedAccount{}, fmt.Errorf("locate browser bridge: %w", err)
		}
		if err := claude.Login(m.context(), claude.LoginConfig{
			Binary:            binary,
			ConfigDir:         loginHome.Path,
			BrowserExecutable: executable,
		}); err != nil {
			return ManagedAccount{}, err
		}
	case string(provider.Codex):
		if err := codex.Login(m.context(), codex.LoginConfig{
			Binary: binary,
			Env:    map[string]string{"CODEX_HOME": loginHome.Path},
			OpenURL: func(rawURL string) error {
				if m.deps.OpenBrowser == nil {
					return errors.New("browser opener unavailable")
				}
				return m.deps.OpenBrowser(context.Background(), rawURL)
			},
		}); err != nil {
			return ManagedAccount{}, err
		}
	}

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
	if err := loginHome.Cleanup(); err != nil {
		return ManagedAccount{}, fmt.Errorf("clean temporary %s login home: %w", providerName, err)
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

	current, hasCurrent := m.store.Active(providerName, time.Now())
	currentID := ""
	if hasCurrent {
		currentID = current.ID
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

	m.rememberProviderCredentialFingerprintLocked(providerName, loginCredential.Data)
	generation := m.store.Generation(providerName)
	m.mu.Unlock()
	accountLocked = false
	reconcileMu.Unlock()
	reconcileLocked = false
	m.invalidateProviderAccountProbe(providerName, binary)
	m.emitProviderAccount(providerName, account, info, generation)
	m.applySelection(providerName, generation, account.ID)
	if err := m.RefreshProviderAccountUsage(providerName, account.ID); err != nil {
		// Login and activation succeeded. Quota refresh is independently
		// retryable from Settings and must not roll the account back.
		m.publishUsageError(providerName, account.ID, err)
	}
	return ManagedAccount{Account: account, Active: true, Generation: generation}, nil
}
