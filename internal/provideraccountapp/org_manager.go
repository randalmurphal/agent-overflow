package provideraccountapp

import (
	"strings"
	"time"

	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"
)

// This file owns organization capture: filling provider.AccountInfo's
// OrgID/OrgName from the sources the probe wire cannot carry, the one
// resolver every adoption path uses to map an observed identity onto a
// saved account, and the Codex legacy-account backfill adoption paths run
// before resolving. Matching semantics live in provideraccounts
// (identity_match.go); this file only gathers the inputs.

// enrichClaudeObservedIdentity fills a Claude probe answer's org fields
// from the credential home's ~/.claude.json oauthAccount record — a file
// the CLI rewrites asynchronously and AO clears on every switch. Resolved
// through credentials' own paths so a harness/soak home override
// never reads the developer's real config. The caller (the stable-probe
// bracket) is responsible for running this where a concurrent credential
// change invalidates the attempt. Not-Claude answers pass through.
func (m *Manager) enrichClaudeObservedIdentity(
	providerName string,
	info provider.AccountInfo,
) provider.AccountInfo {
	if providerName != string(provider.Claude) || strings.TrimSpace(info.OrgID) != "" {
		return info
	}
	if m.credentials == nil {
		return info
	}
	paths, err := m.credentials.Paths(providerName)
	if err != nil || paths.GlobalConfig == "" {
		return info
	}
	return EnrichClaudeIdentity(info, claudeconfig.New(paths.GlobalConfig))
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
