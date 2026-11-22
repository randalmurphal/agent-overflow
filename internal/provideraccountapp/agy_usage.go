package provideraccountapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/agy"
	"agent-overflow/internal/provideraccounts"
)

// refreshAgyAccountUsage runs read-only Antigravity quota HTTP probes OUTSIDE
// the provider-wide reconcile mutex so slow network reads never stall account
// switches or sibling refreshes. Only canonical token promotion, expired-token
// CLI refresh (coalesced by canonical credential change), and final commit
// acquire reconcileMutex, fenced by store generation.
func (m *Manager) refreshAgyAccountUsage(
	ctx context.Context,
	accountID string,
	backoffHold func() error,
) error {
	providerName := string(provider.Agy)
	refreshMu := m.reconcileMutex(providerName)

	// Phase 1: short critical section to check backoff, promote slot, and
	// snapshot the credential + generation.
	if err := refreshMu.LockContext(ctx); err != nil {
		return fmt.Errorf("wait for the %s account lock to read account %s's usage: %w", providerName, accountID, err)
	}
	if err := backoffHold(); err != nil {
		refreshMu.Unlock()
		return err
	}
	if err := m.promoteActiveSlot(providerName); err != nil {
		refreshMu.Unlock()
		return err
	}
	m.mu.RLock()
	account, exists := m.store.Get(providerName, accountID, time.Now())
	if !exists {
		m.mu.RUnlock()
		refreshMu.Unlock()
		return fmt.Errorf("%s account %q is not saved", providerName, accountID)
	}
	active, isActive := m.store.Active(providerName, time.Now())
	isSelected := isActive && active.ID == accountID
	generation := m.store.Generation(providerName)
	var credential, slotAtStart provideraccounts.CredentialSnapshot
	var err error
	if isSelected {
		credential, err = m.credentials.ReadCredentialSnapshot(providerName, "", true)
		if err == nil {
			slotAtStart, err = m.credentials.ReadCredentialSnapshot(providerName, accountID, false)
			if provideraccounts.IsCredentialMissing(err) {
				err = nil
			}
		}
	} else {
		credential, err = m.credentials.ReadCredentialSnapshot(providerName, accountID, false)
	}
	m.mu.RUnlock()
	refreshMu.Unlock()
	if err != nil {
		return fmt.Errorf("read saved %s credentials for usage refresh: %w", providerName, err)
	}

	refresh := providerUsageRefresh{
		providerName: providerName,
		accountID:    accountID,
		generation:   generation,
		isSelected:   isSelected,
		probed:       credential,
		slotAtStart:  slotAtStart,
	}

	var (
		snapshot provider.RateLimitsSnapshot
		probeErr error
		probeRan bool
	)
	defer func() {
		if probeRan && m.deps.Backoff != nil {
			m.deps.Backoff.Note(providerName, accountID, probeErr)
		}
	}()

	// Phase 2: read-only HTTP quota probe without holding reconcileMutex.
	probeRan = true
	snapshot, probeErr = agy.ProbeRateLimitsFromCredentialData(ctx, m.providerHTTPClient(), credential.Bytes())
	if refresh.refreshesInCanonicalHome() && errors.Is(probeErr, agy.ErrOAuthUnauthorized) {
		rotated, refreshErr := m.refreshSelectedAgyCanonicalToken(ctx, accountID, generation, credential.Bytes(), backoffHold)
		if refreshErr != nil {
			probeErr = refreshErr
		} else {
			refresh.refreshed = provideraccounts.NewCredentialSnapshot(rotated)
			snapshot, probeErr = agy.ProbeRateLimitsFromCredentialData(ctx, m.providerHTTPClient(), rotated)
		}
	}
	if probeErr != nil && len(refresh.refreshed.Bytes()) == 0 {
		return mapProviderAccountLoginError(providerName, account, probeErr)
	}

	// Phase 3: brief critical section to commit any rotated credential and
	// publish the snapshot fenced by generation.
	if err := refreshMu.LockContext(ctx); err != nil {
		return fmt.Errorf("wait for the %s account lock to commit account %s's usage: %w", providerName, accountID, err)
	}
	m.mu.Lock()
	commit, commitErr := m.commitProviderUsageRefreshLocked(refresh)
	m.mu.Unlock()
	refreshMu.Unlock()

	if commit.account != nil {
		m.emitProviderAccount(
			providerName,
			*commit.account,
			AccountInfo(*commit.account),
			commit.generation,
		)
	}
	if commit.snapshotDescribesAccount && probeErr == nil {
		snapshot.Provider = providerName
		snapshot.AccountID = accountID
		m.publishRateLimits(snapshot)
	}
	return errors.Join(probeErr, commitErr)
}

// refreshSelectedAgyCanonicalToken refreshes the selected account's OAuth
// token under reconcileMutex. If another goroutine already rotated the
// canonical credential while waiting for the lock, it reuses the updated bytes
// instead of spawning a second cold agy acp process.
func (m *Manager) refreshSelectedAgyCanonicalToken(
	ctx context.Context,
	accountID string,
	startGen uint64,
	staleCred []byte,
	backoffHold func() error,
) ([]byte, error) {
	providerName := string(provider.Agy)
	refreshMu := m.reconcileMutex(providerName)
	if err := refreshMu.LockContext(ctx); err != nil {
		return nil, fmt.Errorf("wait for the %s account lock to refresh token: %w", providerName, err)
	}
	defer refreshMu.Unlock()

	if err := backoffHold(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	curGen := m.store.Generation(providerName)
	active, isActive := m.store.Active(providerName, time.Now())
	m.mu.RUnlock()
	if curGen != startGen || !isActive || active.ID != accountID {
		return nil, agy.ErrOAuthUnauthorized
	}

	if cur, present, readErr := m.readCanonicalCredentialIfPresent(providerName); readErr == nil && present {
		if curBytes := cur.Bytes(); len(curBytes) > 0 && !bytes.Equal(curBytes, staleCred) {
			return curBytes, nil
		}
	}

	binary := m.providerBinaryPath(providerName)
	if _, refreshErr := agy.ProbeAccount(ctx, m.agyProbeConfig(binary, "", "", nil)); refreshErr != nil {
		return nil, fmt.Errorf("refresh Antigravity OAuth token: %w", refreshErr)
	}
	refreshed, present, readErr := m.readCanonicalCredentialIfPresent(providerName)
	if readErr != nil || !present {
		return nil, errors.New("Antigravity credentials missing after refresh; sign in again")
	}
	return refreshed.Bytes(), nil
}

// identifyAgyCredential enriches a probed Antigravity AccountInfo with email,
// display name, and Google account/workspace identity from userinfo when
// available. Non-fatal on offline/test networks if tier info is already present.
func (m *Manager) identifyAgyCredential(
	ctx context.Context,
	info provider.AccountInfo,
	credential []byte,
) provider.AccountInfo {
	if len(credential) == 0 {
		return info
	}
	if identified, err := agy.IdentifyAccount(ctx, m.providerHTTPClient(), credential); err == nil {
		if identified.Email != "" {
			info.Email = identified.Email
		}
		if identified.DisplayName != "" {
			info.DisplayName = identified.DisplayName
		}
		if identified.OrgID != "" {
			info.OrgID = identified.OrgID
		}
		if identified.OrgNameReported {
			info.OrgName = identified.OrgName
			info.OrgNameReported = true
		}
		if identified.TokenSource != "" {
			info.TokenSource = identified.TokenSource
		}
		if identified.APIProvider != "" {
			info.APIProvider = identified.APIProvider
		}
	}
	// Offline / network-failure fallback: agy persists id_token_email inside
	// the OAuth token payload itself.
	if info.Email == "" {
		if cred, err := agy.ParseCredential(credential); err == nil && cred.Email != "" {
			info.Email = cred.Email
		}
	}
	return info
}

func splitAgyEphemeralHome(home string) (geminiDir, appDataSubdir string) {
	cleaned := filepath.Clean(strings.TrimSpace(home))
	if cleaned == "" || cleaned == "." {
		return "", ""
	}
	return filepath.Dir(cleaned), filepath.Base(cleaned)
}
