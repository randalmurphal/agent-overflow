package provideraccountapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/agy"
	"agent-overflow/internal/provideraccounts"
)

// refreshAgyAccountUsage runs read-only Antigravity quota HTTP probes OUTSIDE
// the provider-wide reconcile mutex so slow network reads never stall account
// switches or sibling refreshes. Only canonical token promotion, the
// reconciliation of a selected account's unrecorded canonical credential,
// expired-token CLI refresh (coalesced by canonical credential change), and
// final commit acquire reconcileMutex, fenced by store generation.
func (m *Manager) refreshAgyAccountUsage(
	ctx context.Context,
	accountID string,
	backoffHold func() error,
) error {
	providerName := string(provider.Agy)
	refreshMu := m.reconcileMutex(providerName)

	// Phase 1: short critical section to check backoff, promote slot, and
	// snapshot the credential + generation. A selected account's canonical
	// credential is reconciled first, as every reading's is: the recorded
	// one costs nothing more, another one is identified, which may move the
	// selection off this account, and a login reconciliation refuses is the
	// reading's answer. The record then proves the probed credential is the
	// account's.
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
	start, _, err := m.reconciledUsageStart(ctx, providerName, accountID)
	refreshMu.Unlock()
	if err != nil {
		return err
	}
	account, isSelected, generation := start.account, start.isSelected, start.generation
	credential, slotAtStart := start.credential, start.slotAtStart

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

// agyLoginContinues reports that candidate is provably the login probed
// was after a token refresh: the same refresh token. An email proves
// nothing here: a second sign-in to the same Google account carries it too,
// and so may an outside sign-in to another account saved under that email.
func agyLoginContinues(candidate, probed []byte) bool {
	next, err := agy.ParseCredential(candidate)
	if err != nil {
		return false
	}
	prev, err := agy.ParseCredential(probed)
	if err != nil {
		return false
	}
	return next.RefreshToken != "" && next.RefreshToken == prev.RefreshToken
}

// refreshSelectedAgyCanonicalToken refreshes the selected account's OAuth
// token under reconcileMutex after its quota request was refused. The
// canonical credential is the account's only while it continues the login
// the request was made with (agyLoginContinues): another goroutine's
// refresh of it is reused instead of spawning a second cold agy acp
// process, and agy's own refresh is taken. Anything else, including a
// refresh in which Google rotated the refresh token, writes nothing and
// fails the reading with a retryable answer; the next reading reconciles
// the canonical credential first.
func (m *Manager) refreshSelectedAgyCanonicalToken(
	ctx context.Context,
	accountID string,
	startGen uint64,
	probed []byte,
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
	err := m.validateProviderUsageRefreshSelectionLocked(providerName, accountID, startGen, true)
	m.mu.RUnlock()
	if err != nil {
		return nil, err
	}

	cur, present, err := m.readCanonicalCredentialIfPresent(providerName)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, credentialsChangedWhileRefreshing(providerName)
	}
	if !bytes.Equal(cur.Bytes(), probed) {
		if agyLoginContinues(cur.Bytes(), probed) {
			return cur.Bytes(), nil
		}
		return nil, credentialsChangedWhileRefreshing(providerName)
	}

	binary := m.providerBinaryPath(providerName)
	if _, refreshErr := m.probeCanonicalAgy(ctx, binary, m.providerCustomEnv(providerName)); refreshErr != nil {
		return nil, fmt.Errorf("refresh Antigravity OAuth token: %w", refreshErr)
	}
	refreshed, present, readErr := m.readCanonicalCredentialIfPresent(providerName)
	if readErr != nil {
		return nil, readErr
	}
	if !present {
		return nil, errors.New("missing Antigravity credentials after refresh; sign in again")
	}
	rotated := refreshed.Bytes()
	if !bytes.Equal(rotated, probed) && !agyLoginContinues(rotated, probed) {
		return nil, credentialsChangedWhileRefreshing(providerName)
	}
	return rotated, nil
}

// agyIdentifyTimeout bounds the Google userinfo lookup. Reconciliation runs
// it under the reconcile lock right after the ACP probe, so it has the
// probe's own bound (agy.ProbeAccount).
var agyIdentifyTimeout = 25 * time.Second

// identifyAgyCredential enriches a probed Antigravity AccountInfo with email,
// display name, and Google account identity from userinfo. When the lookup
// fails, the email agy keeps inside the credential (id_token_email) still
// names the login. A failed lookup of a credential that carries no email is
// an error, never a login that names nobody: only a lookup that answered
// may say that.
func (m *Manager) identifyAgyCredential(
	ctx context.Context,
	info provider.AccountInfo,
	credential []byte,
) (provider.AccountInfo, error) {
	if len(credential) == 0 {
		return info, nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, agyIdentifyTimeout)
	identified, lookupErr := agy.IdentifyAccount(lookupCtx, m.providerHTTPClient(), credential)
	cancel()
	if lookupErr == nil {
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
	if info.Email == "" {
		info.Email = credentialIdentity(string(provider.Agy), credential).Email
	}
	if lookupErr != nil && info.Email == "" {
		return info, fmt.Errorf("look up the Google account of the Antigravity login: %w", lookupErr)
	}
	return info, nil
}
