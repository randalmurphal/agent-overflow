package identity

import (
	"fmt"

	"agent-overflow/internal/store"
)

// BootstrapResult is what first boot produced.
type BootstrapResult struct {
	// Owner is the account every later pairing binds to.
	Owner store.User
	// SigningKey is the key session claims are signed with.
	SigningKey store.SigningKey
	// RecoveryCodes is non-empty ONLY on the boot that minted them. They
	// are returned once, in plaintext, and never readable again — a later
	// boot returns nil, which means "this account already has codes
	// somebody wrote down", not "minting failed".
	RecoveryCodes []string
}

// Bootstrap brings first-boot identity into existence: the signing key,
// the owner account, and that account's recovery codes. Idempotent — safe
// to call on every boot, which is how it is meant to be wired.
//
// Codes are minted only when the account has NO recovery-code rows at all,
// spent ones included. Keying on "no UNSPENT rows" would silently re-mint
// for someone who had used their last code, replacing a set they still
// believed in with one they were never shown. Spent rows survive a re-mint
// precisely so this question stays answerable.
func Bootstrap(st *store.Store, backendID, ownerName string) (*Sessions, BootstrapResult, error) {
	sessions, err := NewSessions(st, backendID)
	if err != nil {
		return nil, BootstrapResult{}, err
	}
	key, err := sessions.EnsureSigningKey()
	if err != nil {
		return nil, BootstrapResult{}, err
	}
	owner, err := st.EnsureOwnerUser(ownerName)
	if err != nil {
		return nil, BootstrapResult{}, err
	}
	result := BootstrapResult{Owner: owner, SigningKey: key}

	minted, err := st.CountRecoveryCodes(owner.ID)
	if err != nil {
		return nil, BootstrapResult{}, err
	}
	if minted == 0 {
		codes, err := sessions.MintRecoveryCodes(owner.ID)
		if err != nil {
			return nil, BootstrapResult{}, fmt.Errorf("identity: bootstrap recovery codes: %w", err)
		}
		result.RecoveryCodes = codes
	}
	return sessions, result, nil
}
