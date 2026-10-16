package provideraccountapp

import (
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provideraccounts"
)

// SignIn reports one login landing on a saved account: a sign-in started in
// Agent Overflow, or a provider-native login completed outside it and adopted
// by reconciliation. Clients name the account from it, so it carries the card
// rather than an id resolved against a listing that may not have reloaded.
type SignIn struct {
	Provider string                   `json:"provider"`
	Account  provideraccounts.Account `json:"account"`
	// Added is a login no saved account held; it was appended to the list.
	Added bool `json:"added"`
	// Renewed is a fresh login for an account already saved.
	Renewed bool `json:"renewed"`
	// Switched is the selection moving to this account from another one.
	Switched bool `json:"switched"`
	// Active reports that this account is the selected one afterward.
	Active bool `json:"active"`
}

func (s SignIn) noteworthy() bool {
	return s.Added || s.Renewed || s.Switched
}

// emitSignIn publishes a sign-in outcome. A credential that changed without
// any of the three (a token rotation, a boot re-adoption) publishes nothing.
func (m *Manager) emitSignIn(signIn SignIn) {
	if signIn.noteworthy() && m.deps.Accounts != nil {
		m.deps.Accounts.PublishSignIn(signIn)
	}
}

// CredentialLoginRenewed reports that next holds a fresh sign-in rather than
// a rotation of the login in previous. Only Claude can tell the two apart:
// its refresh deadline is set at sign-in and every rotation carries it
// unchanged. Bytes without a deadline are never a renewal.
func CredentialLoginRenewed(providerName string, previous, next []byte) bool {
	if providerName != string(provider.Claude) {
		return false
	}
	before, ok := claude.RefreshTokenExpiresAt(previous)
	if !ok {
		return false
	}
	after, ok := claude.RefreshTokenExpiresAt(next)
	return ok && after.After(before)
}
