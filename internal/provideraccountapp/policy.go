package provideraccountapp

import (
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/agy"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"
)

// ManagedAccountProviders enumerates every provider with managed multi-account
// storage, login flows, and quota polling.
var ManagedAccountProviders = []string{
	string(provider.Claude),
	string(provider.Codex),
	string(provider.Agy),
}

// CredentialSignedOut identifies provider-native bytes that represent a
// completed sign-out rather than a usable login.
func CredentialSignedOut(providerName string, data []byte) bool {
	return providerName == string(provider.Claude) && claude.CredentialsSignedOut(data)
}

// CredentialChainPosition reports a comparable position on a provider's
// credential rotation chain: Claude's access-token expiry, Codex's
// last_refresh in epoch milliseconds.
func CredentialChainPosition(providerName string, data []byte) (int64, bool) {
	switch providerName {
	case string(provider.Claude):
		return claude.CredentialExpiresAt(data)
	case string(provider.Codex):
		refreshed, ok := codex.CredentialLastRefresh(data)
		if !ok {
			return 0, false
		}
		return refreshed.UnixMilli(), true
	case string(provider.Agy):
		return agy.CredentialExpiresAt(data)
	default:
		return 0, false
	}
}

// CredentialLoginExpiresAt reports when the login provider-native bytes hold
// stops working, in epoch milliseconds (claude.LoginExpiresAt). Past it the
// CLI's next renewal answers invalid_grant and leaves its sign-out husk
// behind. Only Claude records a session deadline; ok=false for everything
// else, which reads as unknown rather than expired.
func CredentialLoginExpiresAt(providerName string, data []byte) (int64, bool) {
	if providerName != string(provider.Claude) {
		return 0, false
	}
	expiresAt, ok := claude.LoginExpiresAt(data)
	if !ok {
		return 0, false
	}
	return expiresAt.UnixMilli(), true
}

// CredentialRefreshTokenFingerprint identifies the refresh token
// provider-native bytes hold (claude and codex
// CredentialRefreshTokenFingerprint).
func CredentialRefreshTokenFingerprint(providerName string, data []byte) (string, bool) {
	switch providerName {
	case string(provider.Claude):
		return claude.CredentialRefreshTokenFingerprint(data)
	case string(provider.Codex):
		return codex.CredentialRefreshTokenFingerprint(data)
	case string(provider.Agy):
		return agy.CredentialRefreshTokenFingerprint(data)
	default:
		return "", false
	}
}

// CredentialPolicy is the shared provider-native credential write policy.
func CredentialPolicy() provideraccounts.Policy {
	return provideraccounts.Policy{
		SignedOut:        CredentialSignedOut,
		ChainPosition:    CredentialChainPosition,
		LoginExpiresAt:   CredentialLoginExpiresAt,
		TokenFingerprint: CredentialRefreshTokenFingerprint,
	}
}

// loginExpiresAtMillis is CredentialLoginExpiresAt as the store's field: 0
// for bytes that name no deadline.
func loginExpiresAtMillis(providerName string, data []byte) int64 {
	expiresAt, ok := CredentialLoginExpiresAt(providerName, data)
	if !ok {
		return 0
	}
	return expiresAt
}

// AccountFromInfo projects observed provider identity into saved metadata.
func AccountFromInfo(accountID, providerName string, info provider.AccountInfo) provideraccounts.Account {
	return provideraccounts.Account{
		ID: accountID, Provider: providerName,
		Email: strings.TrimSpace(info.Email), OrgID: strings.TrimSpace(info.OrgID),
		OrgName: strings.TrimSpace(info.OrgName), DisplayName: strings.TrimSpace(info.DisplayName),
		SubscriptionType: strings.TrimSpace(info.SubscriptionType),
		TokenSource:      strings.TrimSpace(info.TokenSource), APIProvider: strings.TrimSpace(info.APIProvider),
		OrgNameReported: info.OrgNameReported,
	}
}

// AccountInfo projects saved metadata into the provider-neutral identity DTO.
func AccountInfo(account provideraccounts.Account) provider.AccountInfo {
	return provider.AccountInfo{
		Email: account.Email, OrgID: account.OrgID, OrgName: account.OrgName,
		DisplayName: account.DisplayName, SubscriptionType: account.SubscriptionType,
		TokenSource: account.TokenSource, APIProvider: account.APIProvider,
	}
}

// ValidateProvider restricts managed accounts to the implemented providers.
func ValidateProvider(providerName string) error {
	for _, p := range ManagedAccountProviders {
		if providerName == p {
			return nil
		}
	}
	return fmt.Errorf("unsupported account provider %q", providerName)
}
