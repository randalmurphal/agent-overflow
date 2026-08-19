package provideraccounts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// SavedCredential is one account slot's STRUCTURE captured before a
// multi-step account mutation, so a failure can remove whatever that
// operation introduced.
//
// It exists because "this slot has no credential", "this slot has one",
// and "this slot did not exist" are three different starting states with
// three different undos, and none of them can be told apart from the bytes
// alone. Inferring the undo from a nil credential conflates them, and the
// conflation destroys saved logins: an account whose credential was
// momentarily unreadable looks identical to one this operation just
// created, so the rollback deletes a slot it never made — and the account
// can only be recovered by logging in again.
//
// It deliberately does NOT retain the credential bytes. See
// RestoreAccountCredential for why putting them back is never the right
// undo.
//
// Capture before you write; undo only what you captured. The zero value
// restores nothing, so a rollback path that forgot to capture cannot
// delete anything either.
type SavedCredential struct {
	providerName    string
	accountID       string
	hadCredential   bool
	hadAccountDir   bool
	capturedForSlot bool
}

// CaptureAccountCredential records the current state of one saved
// account slot. A slot with no credential, and a slot that does not
// exist at all, are both valid captures — the distinction is what makes
// the matching restore safe.
//
// A slot holding the provider's sign-out husk captures as "no credential":
// the husk is not a login, so an undo may remove it. Both states put the
// account in the same place — needing a fresh sign-in — and the removal is
// the honest one.
func (c *Credentials) CaptureAccountCredential(providerName, accountID string) (SavedCredential, error) {
	accountDir, err := c.accountDirectory(providerName, accountID)
	if err != nil {
		return SavedCredential{}, err
	}
	saved := SavedCredential{
		providerName:    providerName,
		accountID:       accountID,
		capturedForSlot: true,
	}
	switch _, statErr := os.Lstat(accountDir); {
	case statErr == nil:
		saved.hadAccountDir = true
	case errors.Is(statErr, os.ErrNotExist):
	default:
		return SavedCredential{}, fmt.Errorf(
			"provideraccounts: inspect saved %s account before change: %w",
			providerName,
			statErr,
		)
	}

	snapshot, readErr := c.ReadCredentialSnapshot(providerName, accountID, false)
	switch {
	case readErr == nil:
		if c.credentialSignedOut(providerName, snapshot.Data) {
			break
		}
		saved.hadCredential = true
	case IsCredentialMissing(readErr):
	default:
		return SavedCredential{}, fmt.Errorf(
			"provideraccounts: capture saved %s credentials before change: %w",
			providerName,
			readErr,
		)
	}
	return saved, nil
}

// HadCredential reports whether the slot held a usable credential at
// capture time. Callers use it to distinguish "this account already
// existed" from "this operation is introducing it"; a husked slot counts
// as the latter, because there was no login there to return to.
func (s SavedCredential) HadCredential() bool {
	return s.hadCredential
}

// RestoreAccountCredential removes the storage one operation introduced: a
// credential written into a slot that held none, or a slot the operation
// created. It never removes storage that predates the capture, and it never
// rewrites credential bytes.
//
// Rewriting them is what this used to do, and it cannot be correct for a
// provider whose refresh tokens are single-use. The captured bytes are one
// position in a rotation chain; by the time a rollback runs the chain has
// often already moved — the operation being rolled back is frequently what
// moved it, since spawning the CLI at all can rotate the token. Writing the
// captured bytes back then enshrines a refresh token the server has already
// retired, and the account is dead until the user signs in again. Content has
// no state to return to. Structure does, and that is all this undoes.
//
// A slot that already held a credential is therefore left exactly as it
// stands: whatever is in it now is at least as current as what was captured.
//
// An uncaptured (zero) value is a no-op rather than an error: rollback runs
// on the failure path, where turning a missed capture into a second error
// would bury the original cause.
func (c *Credentials) RestoreAccountCredential(saved SavedCredential) error {
	if !saved.capturedForSlot || saved.hadCredential {
		return nil
	}
	if saved.hadAccountDir {
		return c.removeAccountCredentialOnly(saved.providerName, saved.accountID)
	}
	return c.RemoveAccount(saved.providerName, saved.accountID)
}

// removeAccountCredentialOnly deletes the credential from a slot whose
// enclosing directory predates this operation, leaving the directory
// and anything else in it untouched.
func (c *Credentials) removeAccountCredentialOnly(providerName, accountID string) error {
	accountDir, err := c.accountDirectory(providerName, accountID)
	if err != nil {
		return err
	}
	if runtime.GOOS == "darwin" && providerName == "claude" {
		return c.keychain.remove(accountDir, false)
	}
	paths, err := c.Paths(providerName)
	if err != nil {
		return err
	}
	credentialPath := filepath.Join(accountDir, paths.CredentialFile)
	if err := os.Remove(credentialPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf(
			"provideraccounts: remove introduced %s credentials: %w",
			providerName,
			err,
		)
	}
	return nil
}
