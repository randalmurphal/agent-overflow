package provideraccounts

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// This file owns the mutations that move a credential between the canonical
// native store and a saved slot: activation, sign-out, slot removal, and the
// orphan sweep. The storage primitives they compose live in credentials.go.

// Activate atomically replaces the canonical provider credential with the
// selected account. When switching away from currentAccountID, the current
// canonical bytes are first preserved in that account slot so native refresh
// token rotations are not lost. A canonical credential that is missing or is
// the provider's own sign-out husk carries nothing worth preserving; the
// activation proceeds without touching currentAccountID's slot.
func (c *Credentials) Activate(providerName, currentAccountID, targetAccountID string) error {
	var current *CredentialSnapshot
	if currentAccountID != "" && currentAccountID != targetAccountID {
		snapshot, err := c.ReadCredentialSnapshot(providerName, "", true)
		switch {
		case IsCredentialMissing(err):
			currentAccountID = ""
		case err != nil:
			return fmt.Errorf("provideraccounts: read current account before switch: %w", err)
		case c.credentialSignedOut(providerName, snapshot.Data):
			currentAccountID = ""
		default:
			current = &snapshot
		}
	}
	return c.ActivateWithSnapshot(providerName, currentAccountID, targetAccountID, current)
}

// ActivateWithSnapshot is Activate with a caller-verified current credential.
// The snapshot is the caller's claim of what the outgoing account held when
// it validated the switch; if the canonical store has moved by the time the
// final overwrite happens, the newer bytes win the preservation (see the
// re-preserve below) — a mid-switch move is a provider rotation of a
// single-use chain far more often than anything else, and a preserved
// pre-rotation snapshot is a bricked login.
func (c *Credentials) ActivateWithSnapshot(
	providerName string,
	currentAccountID string,
	targetAccountID string,
	current *CredentialSnapshot,
) error {
	if currentAccountID != "" && currentAccountID != targetAccountID {
		if current == nil {
			return errors.New("provideraccounts: current credential snapshot is required")
		}
		// A husk carries nothing worth preserving, and the outgoing slot
		// already holds that account's last saved pair. Skipping is the whole
		// behavior — refusing the switch instead would strand the user on a
		// login nothing inside the app ever replaces (incident 2026-08-03).
		// Dropping the snapshot also stands the re-preserve below down: there
		// is no rotation to detect against bytes that were never a token.
		if c.credentialSignedOut(providerName, current.Data) {
			current = nil
		} else if err := c.WriteAccountCredential(providerName, currentAccountID, current.Data); err != nil {
			return fmt.Errorf("provideraccounts: preserve current account before switch: %w", err)
		}
	}
	data, err := c.ReadCredential(providerName, targetAccountID, false)
	if err != nil {
		return fmt.Errorf("provideraccounts: read selected credentials: %w", err)
	}
	// Retire the outgoing identity before the incoming credential lands.
	// Ordered this way every outcome converges on a consistent pair: if
	// the clear fails nothing has moved, and if the credential write
	// fails the provider re-derives the identity it already had. The
	// reverse order has a failure mode that does not self-heal — new
	// tokens described by the previous account's identity.
	if currentAccountID != targetAccountID {
		if err := c.retireProviderIdentity(providerName); err != nil {
			return err
		}
	}
	// Last-moment re-preserve: a provider process running against the
	// canonical home (a live session, the user's own CLI in a terminal)
	// can rotate the credential between the caller's snapshot and this
	// overwrite. Claude refresh tokens are single-use, so saving the
	// pre-rotation snapshot while discarding the rotation bricks the
	// outgoing account. Re-read canonical now and, if it moved, preserve
	// the newer bytes instead — unless they are the provider's sign-out
	// husk, which must never overwrite a slot's last saved pair. The
	// read is best-effort: a missing canonical means nothing rotated
	// worth keeping beyond the snapshot already preserved above.
	if currentAccountID != "" && currentAccountID != targetAccountID && current != nil {
		if latest, err := c.ReadCredentialSnapshot(providerName, "", true); err == nil &&
			!bytes.Equal(latest.Data, current.Data) &&
			!c.credentialSignedOut(providerName, latest.Data) {
			if err := c.WriteAccountCredential(providerName, currentAccountID, latest.Data); err != nil {
				return fmt.Errorf(
					"provideraccounts: preserve rotated current account before switch: %w",
					err,
				)
			}
		}
	}
	if err := c.writeActiveCredential(providerName, data); err != nil {
		return fmt.Errorf("provideraccounts: activate %s credentials: %w", providerName, err)
	}
	return nil
}

// RemoveActive signs the provider out of its canonical home. The
// identity record goes with the credential — leaving it behind would
// describe a login that no longer exists, which is the same split-state
// bug a switch avoids and exactly what the provider's own logout
// clears.
func (c *Credentials) RemoveActive(providerName string) error {
	if err := c.retireProviderIdentity(providerName); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" && providerName == "claude" {
		paths, err := c.Paths(providerName)
		if err != nil {
			return err
		}
		return c.keychain.remove(paths.SharedHome, true)
	}
	activePath, err := c.ActiveCredentialPath(providerName)
	if err != nil {
		return err
	}
	info, err := os.Lstat(activePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("provideraccounts: inspect active credentials: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("provideraccounts: active credential path %s is not a regular file", activePath)
	}
	if err := os.Remove(activePath); err != nil {
		return fmt.Errorf("provideraccounts: remove active credentials: %w", err)
	}
	return nil
}

// RemoveAccount deletes one saved credential slot. The validated account ID
// and non-symlink managed root confine removal to Agent Overflow's own storage.
func (c *Credentials) RemoveAccount(providerName, accountID string) error {
	paths, err := c.Paths(providerName)
	if err != nil {
		return err
	}
	accountDir, err := c.accountDirectory(providerName, accountID)
	if err != nil {
		return err
	}
	if runtime.GOOS == "darwin" && providerName == "claude" {
		if err := c.keychain.remove(accountDir, false); err != nil {
			return err
		}
	}
	root, err := os.OpenRoot(paths.SharedHome)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("provideraccounts: open %s home for account removal: %w", providerName, err)
	}
	defer root.Close()
	if err := root.RemoveAll(filepath.Join(accountDirectoryName, accountID)); err != nil {
		return fmt.Errorf("provideraccounts: remove saved account credentials: %w", err)
	}
	return nil
}

// PruneOrphanedAccounts removes credential slots that have no corresponding
// metadata account and returns the IDs it removed, so the caller can record
// each destruction. A crash can leave one slot behind between credential
// creation and metadata commit; registered account directories are never
// inspected or modified by this sweep.
//
// An empty keep-set prunes nothing. Zero registered accounts cannot be told
// apart from a process reading a different metadata store than the one these
// slots belong to — a fresh --data-dir, a test overriding the config root
// but not the home — and the slots hold logins whose refresh tokens are
// single-use and unrecoverable, so the sweep refuses to guess. The crash
// orphan it exists for is cleaned on the first sweep after any account is
// registered again.
func (c *Credentials) PruneOrphanedAccounts(
	providerName string,
	keepAccountIDs map[string]bool,
) ([]string, error) {
	paths, err := c.Paths(providerName)
	if err != nil {
		return nil, err
	}
	rootPath := filepath.Join(paths.SharedHome, accountDirectoryName)
	info, err := os.Lstat(rootPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("provideraccounts: inspect managed account root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("provideraccounts: managed account root %s is not a directory", rootPath)
	}
	if len(keepAccountIDs) == 0 {
		return nil, nil
	}
	entries, err := os.ReadDir(rootPath)
	if err != nil {
		return nil, fmt.Errorf("provideraccounts: list managed accounts: %w", err)
	}
	var pruned []string
	var pruneErrs []error
	for _, entry := range entries {
		accountID := entry.Name()
		if !safeAccountID.MatchString(accountID) {
			pruneErrs = append(pruneErrs, fmt.Errorf(
				"provideraccounts: invalid managed account entry %q",
				accountID,
			))
			continue
		}
		if keepAccountIDs[accountID] {
			continue
		}
		if err := c.RemoveAccount(providerName, accountID); err != nil {
			pruneErrs = append(pruneErrs, err)
			continue
		}
		pruned = append(pruned, accountID)
	}
	return pruned, errors.Join(pruneErrs...)
}
