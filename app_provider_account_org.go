package main

import (
	"fmt"
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
// through providerCredentials' own paths so a harness/soak home override
// never reads the developer's real config. The caller (the stable-probe
// bracket) is responsible for running this where a concurrent credential
// change invalidates the attempt. Not-Claude answers pass through.
func (a *App) enrichClaudeObservedIdentity(
	providerName string,
	info provider.AccountInfo,
) provider.AccountInfo {
	if providerName != string(provider.Claude) || strings.TrimSpace(info.OrgID) != "" {
		return info
	}
	if a.providerCredentials == nil {
		return info
	}
	paths, err := a.providerCredentials.Paths(providerName)
	if err != nil || paths.GlobalConfig == "" {
		return info
	}
	return enrichClaudeInfoFromOAuthRecord(info, claudeconfig.New(paths.GlobalConfig))
}

// enrichCodexObservedIdentity fills a Codex probe answer's workspace id
// from credential bytes verified to pair with the same identity. The
// bytes ARE the identity source (auth.json carries the claims), so this
// needs no acceptance rule beyond the parse. Not-Codex answers pass
// through.
func enrichCodexObservedIdentity(
	providerName string,
	info provider.AccountInfo,
	credential []byte,
) provider.AccountInfo {
	if providerName != string(provider.Codex) || strings.TrimSpace(info.OrgID) != "" {
		return info
	}
	if orgID, ok := codex.CredentialOrgID(credential); ok {
		info.OrgID = orgID
	}
	return info
}

// enrichClaudeInfoFromOAuthRecord applies the email-paired oauthAccount
// record onto a Claude probe answer. Split out so the login flow can run
// the same acceptance rule against the ephemeral login home's config.
// The record is accepted ONLY when its email matches the probe-reported
// one, so a racing external login cannot stamp one account's org onto
// another's tokens; a mismatch or absence leaves the org unknown, which
// every consumer treats as compatible-with-any. Fills blanks only —
// a probe answer that already knows its org keeps it.
func enrichClaudeInfoFromOAuthRecord(
	info provider.AccountInfo,
	store *claudeconfig.Store,
) provider.AccountInfo {
	record, ok := store.ReadOAuthAccount()
	if !ok || !record.EmailMatches(info.Email) {
		return info
	}
	if strings.TrimSpace(info.OrgID) == "" {
		info.OrgID = strings.TrimSpace(record.OrganizationUUID)
	}
	if strings.TrimSpace(info.OrgName) == "" {
		info.OrgName = strings.TrimSpace(record.OrganizationName)
	}
	return info
}

// findAccountByObservedIdentity answers "is this observed login one of
// the saved accounts?" — a pure lookup over provideraccounts'
// FindByIdentity lattice, no writes. Adoption paths that may be seeing a
// NEW workspace for a known email call backfillCodexOrgIDs first, so a
// legacy blank-org Codex account cannot blank-match a different
// workspace's login.
func (a *App) findAccountByObservedIdentity(
	providerName string,
	info provider.AccountInfo,
) (provideraccounts.Account, bool, error) {
	return a.providerAccounts.FindByIdentity(
		providerName, provideraccounts.IdentityFromInfo(info))
}

// backfillCodexOrgIDs resolves blank-org saved Codex accounts sharing the
// observed email by reading each one's saved slot: Codex slots carry the
// workspace id inside the credential bytes, so the legacy blank is
// resolvable on the spot (Claude's credential bytes carry no org, so a
// legacy blank-org Claude account genuinely stays unknown-until-enriched).
// Called from adoption paths only — attribution guards run per turn and
// must not pick up slot reads under providerAccountMu. No-op for other
// providers. Best-effort per account: a slot that is missing, husked, or
// unparseable stays unknown — refusing the whole adoption over a side
// account's unreadable slot would trade a rare mis-merge for a common
// hard failure — but the read failure is audited so it cannot rot
// silently.
func (a *App) backfillCodexOrgIDs(providerName, email string) {
	if providerName != string(provider.Codex) || strings.TrimSpace(email) == "" {
		return
	}
	for _, account := range a.providerAccounts.List(providerName, time.Now()) {
		if strings.TrimSpace(account.OrgID) != "" ||
			!strings.EqualFold(strings.TrimSpace(account.Email), email) {
			continue
		}
		data, err := a.providerCredentials.ReadCredential(providerName, account.ID, false)
		if err != nil {
			a.auditAccountEvent(
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
		if _, err := a.providerAccounts.UpdateMetadata(account); err != nil {
			a.auditAccountEvent(
				"could not backfill codex account %s with workspace %s: %v",
				account.ID, orgID, err,
			)
		}
	}
}

// describeObservedAccount labels an observed login for user-facing errors:
// the email plus the organization when one is known.
func describeObservedAccount(info provider.AccountInfo) string {
	email := strings.TrimSpace(info.Email)
	org := strings.TrimSpace(info.OrgName)
	if org == "" {
		org = strings.TrimSpace(info.OrgID)
	}
	if org == "" {
		return email
	}
	return fmt.Sprintf("%s (%s)", email, org)
}
