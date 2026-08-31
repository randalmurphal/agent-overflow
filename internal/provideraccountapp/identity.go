package provideraccountapp

import (
	"fmt"
	"strings"

	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
)

// EnrichCodexIdentity reads the workspace id carried by verified credential
// bytes into an otherwise incomplete Codex identity.
func EnrichCodexIdentity(providerName string, info provider.AccountInfo, credential []byte) provider.AccountInfo {
	if providerName != string(provider.Codex) || strings.TrimSpace(info.OrgID) != "" {
		return info
	}
	if orgID, ok := codex.CredentialOrgID(credential); ok {
		info.OrgID = orgID
	}
	return info
}

// EnrichClaudeIdentity applies an email-matched oauthAccount record without
// overwriting identity fields already known from the probe.
func EnrichClaudeIdentity(info provider.AccountInfo, config *claudeconfig.Store) provider.AccountInfo {
	record, ok := config.ReadOAuthAccount()
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

// DescribeObservedAccount formats identity for account-scoped errors.
func DescribeObservedAccount(info provider.AccountInfo) string {
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
