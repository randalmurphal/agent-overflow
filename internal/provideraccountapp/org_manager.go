package provideraccountapp

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"
)

// This file owns organization capture: identifying a Claude credential through
// the API and filling a Codex identity from its credential bytes, the one
// resolver every adoption path uses to map an observed identity onto a
// saved account, and the Codex legacy-account backfill adoption paths run
// before resolving. Matching semantics live in provideraccounts
// (identity_match.go); this file only gathers the inputs.

// identifyClaudeCredential replaces the identity a Claude probe reported with
// the one the API gives for credential's own access token. The probe's answer
// comes from `~/.claude.json`'s `oauthAccount` record, a shared file the CLI
// rewrites asynchronously, so it can describe the previous account while the
// canonical store holds the next one's tokens. With
// two organizations on one email that pairing passes every email check and
// lands one organization's tokens in the other's slot. The profile answer is
// bound to the token, so it is the only identity a credential is matched on.
//
// A credential that cannot be identified (no access token, no profile scope)
// reports no identity: blank is unknown, which the matching lattice never
// treats as a match on its own. A failed request is an error, because
// attributing the credential without an answer is the defect this prevents.
func (m *Manager) identifyClaudeCredential(
	info provider.AccountInfo,
	credential []byte,
) (provider.AccountInfo, error) {
	info.Email, info.OrgID, info.OrgName = "", "", ""
	if len(credential) == 0 {
		return info, nil
	}
	profile, err := claude.ProbeProfile(m.context(), m.providerHTTPClient(), credential)
	switch {
	case errors.Is(err, claude.ErrNoCredentials), errors.Is(err, claude.ErrNoProfileScope):
		return info, nil
	case err != nil:
		return provider.AccountInfo{}, fmt.Errorf("identify the signed-in Claude account: %w", err)
	}
	info.Email, info.OrgID, info.OrgName = profile.Email, profile.OrgID, profile.OrgName
	return info, nil
}

// identifyCanonicalClaudeCredential is identifyClaudeCredential for the
// canonical store. A credential whose fingerprint is the one AO recorded when
// it installed, adopted or reconciled it is already attributed to the selected
// account, so its identity is that account's and needs no request; only an
// unattributed credential is asked about. It takes mu's read side, so a
// caller holding mu uses identifyClaudeCredential.
func (m *Manager) identifyCanonicalClaudeCredential(
	info provider.AccountInfo,
	credential []byte,
) (provider.AccountInfo, error) {
	claudeName := string(provider.Claude)
	m.mu.RLock()
	known, attributed := m.fingerprints[claudeName]
	var active provideraccounts.Account
	hasActive := false
	if m.store != nil {
		active, hasActive = m.store.Active(claudeName, time.Now())
	}
	m.mu.RUnlock()
	if attributed && hasActive && len(credential) > 0 && known == sha256.Sum256(credential) {
		info.Email, info.OrgID, info.OrgName = active.Email, active.OrgID, active.OrgName
		return info, nil
	}
	return m.identifyClaudeCredential(info, credential)
}

// findAccountByObservedIdentity answers "is this observed login one of
// the saved accounts?" — a pure lookup over provideraccounts'
// FindByIdentity lattice, no writes. Adoption paths that may be seeing a
// NEW workspace for a known email call backfillCodexOrgIDs first, so a
// legacy blank-org Codex account cannot blank-match a different
// workspace's login.
func (m *Manager) findAccountByObservedIdentity(
	providerName string,
	info provider.AccountInfo,
) (provideraccounts.Account, bool, error) {
	return m.store.FindByIdentity(
		providerName, provideraccounts.IdentityFromInfo(info))
}

// backfillCodexOrgIDs resolves blank-org saved Codex accounts sharing the
// observed email by reading each one's saved slot: Codex slots carry the
// workspace id inside the credential bytes, so the legacy blank is
// resolvable on the spot (Claude's credential bytes carry no org, so a
// legacy blank-org Claude account genuinely stays unknown-until-enriched).
// Called from adoption paths only — attribution guards run per turn and
// must not pick up slot reads under mu. No-op for other
// providers. Best-effort per account: a slot that is missing, husked, or
// unparseable stays unknown — refusing the whole adoption over a side
// account's unreadable slot would trade a rare mis-merge for a common
// hard failure — but the read failure is audited so it cannot rot
// silently.
func (m *Manager) backfillCodexOrgIDs(providerName, email string) {
	if providerName != string(provider.Codex) || strings.TrimSpace(email) == "" {
		return
	}
	for _, account := range m.store.List(providerName, time.Now()) {
		if strings.TrimSpace(account.OrgID) != "" ||
			!strings.EqualFold(strings.TrimSpace(account.Email), email) {
			continue
		}
		data, err := m.credentials.ReadCredential(providerName, account.ID, false)
		if err != nil {
			m.audit(
				"could not read codex account %s's slot for workspace backfill: %v",
				account.ID, err,
			)
			continue
		}
		orgID, ok := codex.CredentialOrgID(data)
		if !ok {
			continue
		}
		account.OrgID = orgID
		if _, err := m.store.UpdateMetadata(account); err != nil {
			m.audit(
				"could not backfill codex account %s with workspace %s: %v",
				account.ID, orgID, err,
			)
		}
	}
}
