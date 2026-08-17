package provideraccounts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// SavedCredential is one account slot's state captured before a
// multi-step account mutation, so a failure can put the slot back
// exactly as it was found.
//
// It exists because "this slot has no credential" and "this slot did
// not exist" are different starting states with different undos, and
// neither can be recovered from the bytes alone. Inferring the undo
// from a nil credential conflates them, and the conflation destroys
// saved logins: an account whose credential was momentarily unreadable
// looks identical to one this operation just created, so the rollback
// deletes a slot it never made — and the account can only be recovered
// by logging in again.
//
// Capture before you write; restore only what you captured. The zero
// value restores nothing, so a rollback path that forgot to capture
// cannot delete anything either.
type SavedCredential struct {
	providerName    string
	accountID       string
	data            []byte
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
// the husk is not a login, so the restore removes it rather than rewriting
// it. Both states put the account in the same place — needing a fresh
// sign-in — and the removal is the honest one.
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
		saved.data = snapshot.Data
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

// RestoreAccountCredential returns one slot to its captured state:
// rewriting the credential it held, removing a credential this
// operation introduced, or removing a slot this operation created. It
// never removes storage that predates the capture.
//
// An uncaptured (zero) value is a no-op rather than an error: rollback
// runs on the failure path, where turning a missed capture into a
// second error would bury the original cause.
func (c *Credentials) RestoreAccountCredential(saved SavedCredential) error {
	if !saved.capturedForSlot {
		return nil
	}
	if saved.hadCredential {
		if err := c.WriteAccountCredential(saved.providerName, saved.accountID, saved.data); err != nil {
			return fmt.Errorf(
				"provideraccounts: restore saved %s credentials: %w",
				saved.providerName,
				err,
			)
		}
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
