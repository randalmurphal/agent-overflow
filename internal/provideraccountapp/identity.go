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

// providerLabel is a provider's name as a person reads it.
func providerLabel(providerName string) string {
	switch providerName {
	case string(provider.Claude):
		return "Claude"
	case string(provider.Codex):
		return "Codex"
	case string(provider.Agy):
		return "Antigravity"
	default:
		return providerName
	}
}

// unresolvedLoginError reports a canonical login that names no saved account
// for certain: several saved accounts match it, or it carries no identity
// and the selected account does. Its text is the sentence a person reads;
// the selection stays and nothing of the login is saved.
type unresolvedLoginError struct{ sentence string }

func (e unresolvedLoginError) Error() string { return e.sentence }

// PublicDetails makes the sentence the client-safe text (errorsx.Detailed).
func (e unresolvedLoginError) PublicDetails() (code, message string) {
	return "account_login_unresolved", e.sentence
}

// replaceUnresolvedLogin is what a person can do about an unresolved login:
// a sign-in from the app, or a switch, installs a saved login over it.
const replaceUnresolvedLogin = "Sign in from the app, or switch to another saved account, to replace it."

// The unresolved sentences say only what the bytes show: the CLI's own
// refresh of the selected account's login and a sign-in outside the app look
// the same, so neither sentence says which one it was.

// errAmbiguousLogin is the unresolved verdict for a login whose email more
// than one saved account uses, naming no organization to tell them apart.
func errAmbiguousLogin(providerName string, info provider.AccountInfo) error {
	unit := "organization"
	if providerName == string(provider.Codex) {
		unit = "workspace"
	}
	return unresolvedLoginError{sentence: fmt.Sprintf(
		"The %s login in use is %s, an email more than one saved account uses, and it names no %s, so the app can't tell which account it is. %s",
		providerLabel(providerName), strings.TrimSpace(info.Email), unit, replaceUnresolvedLogin,
	)}
}

// errIdentitylessLogin is the unresolved verdict for a login that carries no
// account identity, while the selected account has one.
func errIdentitylessLogin(providerName string, selected string) error {
	example := ""
	if providerName == string(provider.Codex) {
		example = ", such as an API key"
	}
	return unresolvedLoginError{sentence: fmt.Sprintf(
		"The %s login in use names no account%s, so the app can't confirm it is %s's and doesn't read its usage. %s",
		providerLabel(providerName), example, selected, replaceUnresolvedLogin,
	)}
}
