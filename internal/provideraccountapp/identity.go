package provideraccountapp

import (
	"fmt"
	"strings"

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
